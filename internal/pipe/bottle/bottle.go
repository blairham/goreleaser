package bottle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"time"

	"github.com/caarlos0/log"
	"github.com/goreleaser/goreleaser/v2/internal/artifact"
	"github.com/goreleaser/goreleaser/v2/internal/commitauthor"
	"github.com/goreleaser/goreleaser/v2/internal/skips"
	"github.com/goreleaser/goreleaser/v2/internal/tmpl"
	"github.com/goreleaser/goreleaser/v2/pkg/config"
	"github.com/goreleaser/goreleaser/v2/pkg/context"
)

const (
	extraConfig = "HomebrewBottleConfig"
	extraTag    = "BottleTag"

	// defaultMacOS matches the oldest macOS supported by current Go
	// toolchains. Homebrew pours a bottle on its tagged release and newer.
	defaultMacOS = "monterey"
)

// ErrNoBinaries happens when no darwin/linux binaries match a config.
var ErrNoBinaries = errors.New("no darwin or linux binaries found")

// Archive builds bottles out of the release binaries. It runs before
// checksums, so bottles are checksummed, signed and uploaded like any other
// release artifact.
type Archive struct{}

func (Archive) String() string { return "homebrew bottles" }

func (Archive) Skip(ctx *context.Context) bool {
	return skips.Any(ctx, skips.Homebrew) || len(ctx.Config.HomebrewBottles) == 0
}

func (Archive) Default(ctx *context.Context) error {
	for i := range ctx.Config.HomebrewBottles {
		cfg := &ctx.Config.HomebrewBottles[i]
		cfg.CommitAuthor = commitauthor.Default(cfg.CommitAuthor)
		if cfg.CommitMessageTemplate == "" {
			cfg.CommitMessageTemplate = "Brew formula update for {{ .ProjectName }} version {{ .Tag }}"
		}
		if cfg.Name == "" {
			cfg.Name = ctx.Config.ProjectName
		}
		if cfg.Directory == "" {
			cfg.Directory = "Formula"
		}
		if cfg.Goamd64 == "" {
			cfg.Goamd64 = "v1"
		}
		if cfg.Bottles.MacOS == "" {
			cfg.Bottles.MacOS = defaultMacOS
		}
	}
	return nil
}

func (Archive) Run(ctx *context.Context) error {
	for i := range ctx.Config.HomebrewBottles {
		cfg := &ctx.Config.HomebrewBottles[i]
		name, err := tmpl.New(ctx).Apply(cfg.Name)
		if err != nil {
			return err
		}
		cfg.Name = name
		if err := buildBottles(ctx, *cfg); err != nil {
			return err
		}
	}
	return nil
}

// binaries returns the binaries of cfg grouped by bottle tag.
func binaries(ctx *context.Context, cfg config.HomebrewBottle) (map[string][]*artifact.Artifact, error) {
	filters := []artifact.Filter{
		artifact.ByTypes(artifact.Binary, artifact.UniversalBinary),
		artifact.Or(artifact.ByGoos("darwin"), artifact.ByGoos("linux")),
		artifact.Or(
			artifact.And(artifact.ByGoarch("amd64"), artifact.ByGoamd64(cfg.Goamd64)),
			artifact.ByGoarch("arm64"),
			artifact.ByGoarch("all"),
		),
		artifact.OnlyReplacingUnibins,
	}
	if len(cfg.IDs) > 0 {
		filters = append(filters, artifact.ByIDs(cfg.IDs...))
	}

	byTag := map[string][]*artifact.Artifact{}
	for _, art := range ctx.Artifacts.Filter(artifact.And(filters...)).List() {
		for _, tag := range tagsFor(art.Goos, art.Goarch, cfg.Bottles.MacOS) {
			byTag[tag] = append(byTag[tag], art)
		}
	}
	if len(byTag) == 0 {
		return nil, ErrNoBinaries
	}
	return byTag, nil
}

// tagsFor maps a GOOS/GOARCH pair to Homebrew bottle tags.
func tagsFor(goos, goarch, macos string) []string {
	switch goos + "/" + goarch {
	case "darwin/arm64":
		return []string{"arm64_" + macos}
	case "darwin/amd64":
		return []string{macos}
	case "darwin/all":
		return []string{"arm64_" + macos, macos}
	case "linux/arm64":
		return []string{"arm64_linux"}
	case "linux/amd64":
		return []string{"x86_64_linux"}
	}
	return nil
}

// bottleFilename is the asset name Homebrew downloads from a non-GitHub
// Packages root_url: "<name>-<version>.<tag>.bottle.tar.gz".
func bottleFilename(name, version, tag string) string {
	return fmt.Sprintf("%s-%s.%s.bottle.tar.gz", name, version, tag)
}

func buildBottles(ctx *context.Context, cfg config.HomebrewBottle) error {
	byTag, err := binaries(ctx, cfg)
	if err != nil {
		return err
	}

	files := make([]config.HomebrewBottleFile, 0, len(cfg.Bottles.Files))
	for _, f := range cfg.Bottles.Files {
		if (f.Src == "") == (f.Link == "") {
			return fmt.Errorf("bottles.files %q: set exactly one of src and link", f.Dst)
		}
		src, err := tmpl.New(ctx).Apply(f.Src)
		if err != nil {
			return err
		}
		files = append(files, config.HomebrewBottleFile{Src: src, Dst: f.Dst, Link: f.Link})
	}

	dir := filepath.Join(ctx.Config.Dist, "homebrew_bottles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	for _, tag := range slices.Sorted(maps.Keys(byTag)) {
		filename := bottleFilename(cfg.Name, ctx.Version, tag)
		dst := filepath.Join(dir, filename)
		log.WithField("bottle", dst).Info("creating")
		entries, err := bottleEntries(cfg, byTag[tag], files)
		if err != nil {
			return fmt.Errorf("could not create %s: %w", filename, err)
		}
		if err := writeBottle(dst, path.Join(cfg.Name, ctx.Version), ctx.Git.CommitDate, entries); err != nil {
			return fmt.Errorf("could not create %s: %w", filename, err)
		}
		ctx.Artifacts.Add(&artifact.Artifact{
			Type: artifact.HomebrewBottle,
			Name: filename,
			Path: dst,
			Extra: map[string]any{
				artifact.ExtraID: cfg.Name,
				extraTag:         tag,
				extraConfig:      cfg,
			},
		})
	}
	return nil
}

// binaryDst is where a binary goes in the keg: bottles.binaries, else bin/.
func binaryDst(cfg config.HomebrewBottle, name string) string {
	for _, b := range cfg.Bottles.Binaries {
		if b.Name == name && b.Dst != "" {
			return path.Clean(b.Dst)
		}
	}
	return path.Join("bin", name)
}

// entry is one thing in a bottle, relative to the keg.
type entry struct {
	src  string // file to copy; empty for a symlink
	dst  string
	link string // symlink target
	exec bool   // force 0755 (binaries)
}

func bottleEntries(cfg config.HomebrewBottle, bins []*artifact.Artifact, files []config.HomebrewBottleFile) ([]entry, error) {
	var entries []entry
	for _, bin := range bins {
		entries = append(entries, entry{src: bin.Path, dst: binaryDst(cfg, bin.Name), exec: true})
	}
	for _, f := range files {
		dst := path.Clean(f.Dst)
		if f.Link != "" {
			entries = append(entries, entry{dst: dst, link: f.Link})
			continue
		}
		info, err := os.Stat(f.Src)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			entries = append(entries, entry{src: f.Src, dst: dst})
			continue
		}
		var walked []entry
		if err := filepath.WalkDir(f.Src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(f.Src, p)
			if err != nil {
				return err
			}
			walked = append(walked, entry{src: p, dst: path.Join(dst, filepath.ToSlash(rel))})
			return nil
		}); err != nil {
			return nil, err
		}
		entries = append(entries, walked...)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.dst] {
			return nil, fmt.Errorf("%s is in the bottle twice", e.dst)
		}
		seen[e.dst] = true
	}
	return entries, nil
}

// writeBottle writes a bottle tarball laid out as Homebrew expects: every
// entry under <name>/<version>/, with its parent directories.
func writeBottle(dst, keg string, mtime time.Time, entries []entry) (err error) {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()

	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)

	dirs := map[string]bool{}
	for _, e := range entries {
		for d := path.Dir(path.Join(keg, e.dst)); d != "." && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir,
			Name:     d + "/",
			Mode:     0o755,
			ModTime:  mtime,
		}); err != nil {
			return err
		}
	}
	for _, e := range entries {
		name := path.Join(keg, e.dst)
		if e.link != "" {
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeSymlink,
				Name:     name,
				Linkname: e.link,
				Mode:     0o777,
				ModTime:  mtime,
			}); err != nil {
				return err
			}
			continue
		}
		if err := addFile(tw, e.src, name, e.exec, mtime); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gw.Close()
}

func addFile(tw *tar.Writer, src, dst string, exec bool, mtime time.Time) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	mode := int64(0o644)
	if exec || info.Mode()&0o111 != 0 {
		mode = 0o755
	}
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     dst,
		Mode:     mode,
		Size:     info.Size(),
		ModTime:  mtime,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, in)
	return err
}
