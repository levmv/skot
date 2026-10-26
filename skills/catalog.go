package skills

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Source is a directory of skill packages.
// Path must be absolute. Symlinks may name the source or individual packages;
// their resolved directories are subject to DiscoveryOptions.AllowPath.
// Label identifies the source for display.
type Source struct {
	Path  string
	Label string
}

// Entry is a discovered package. Path is its canonical absolute SKILL.md path.
// Metadata is a snapshot; Load reads the current contents.
type Entry struct {
	Path     string
	Source   Source
	Metadata Metadata

	root      string
	directory string
	allowPath func(string) bool
}

// DiscoveryOptions restricts which packages are read.
type DiscoveryOptions struct {
	// AllowPath checks absolute paths, including resolved symlink targets,
	// during discovery and Load. Nil allows all paths.
	AllowPath func(Source, string) bool
	// Paths limits metadata reads to these exact canonical SKILL.md paths.
	// Nil discovers all packages. A listed path must still belong to a source.
	Paths []string
}

// Diagnostic describes a source or package which could not be discovered.
type Diagnostic struct {
	Path string
	Err  error
}

// Catalog contains packages sorted by name, keeping source order for equal names.
type Catalog struct {
	Entries     []Entry
	Diagnostics []Diagnostic
}

// ErrNotFound means a reference did not match any catalog entry.
var ErrNotFound = errors.New("skill not found")

var errOutsideSelection = errors.New("skill is outside the requested selection")

// Discover scans immediate subdirectories containing SKILL.md. It skips missing
// sources and manifests, reporting other failures in Diagnostics. Earlier
// sources take precedence for duplicate names and paths.
// On non-Unix platforms, Diagnostics wraps errors.ErrUnsupported.
func Discover(sources []Source) Catalog {
	return DiscoverWithOptions(sources, DiscoveryOptions{})
}

// DiscoverWithOptions applies restrictions to discovery and subsequent loads.
func DiscoverWithOptions(sources []Source, options DiscoveryOptions) Catalog {
	var catalog Catalog
	seen := make(map[string]bool)
	var selected map[string]bool
	if options.Paths != nil {
		selected = make(map[string]bool, len(options.Paths))
		for _, path := range options.Paths {
			selected[path] = true
		}
	}
	for _, source := range sources {
		var allowPath func(string) bool
		if options.AllowPath != nil {
			allowPath = func(path string) bool { return options.AllowPath(source, path) }
		}
		entries, diagnostics := discoverSource(source, allowPath, selected)
		catalog.Diagnostics = append(catalog.Diagnostics, diagnostics...)
		for _, entry := range entries {
			if !seen[entry.Path] {
				seen[entry.Path] = true
				catalog.Entries = append(catalog.Entries, entry)
			}
		}
	}
	slices.SortStableFunc(catalog.Entries, func(left, right Entry) int {
		return strings.Compare(left.Metadata.Name, right.Metadata.Name)
	})
	return catalog
}

// Resolve finds the first entry matching a name or an exact catalog path.
func (catalog Catalog) Resolve(reference string) (Entry, error) {
	for _, entry := range catalog.Entries {
		if entry.Path == reference || entry.Metadata.Name == reference {
			return entry, nil
		}
	}
	return Entry{}, fmt.Errorf("%w: %q", ErrNotFound, reference)
}

func discoverSource(source Source, allowPath func(string) bool, selected map[string]bool) ([]Entry, []Diagnostic) {
	fail := func(err error) ([]Entry, []Diagnostic) {
		return nil, []Diagnostic{{Path: source.Path, Err: err}}
	}
	if !filepath.IsAbs(source.Path) {
		return fail(errors.New("skill source path must be absolute"))
	}
	if err := checkPath(source.Path, allowPath); err != nil {
		return fail(err)
	}
	resolved, err := filepath.EvalSymlinks(source.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return fail(err)
	}
	if err := checkPath(resolved, allowPath); err != nil {
		return fail(err)
	}
	root, err := openSource(resolved)
	if err != nil {
		return fail(err)
	}
	defer root.Close()
	children, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fail(err)
	}
	var entries []Entry
	var diagnostics []Diagnostic
	for _, child := range children {
		if !child.IsDir() && child.Type()&os.ModeSymlink == 0 {
			continue
		}
		entry, err := discoverEntry(source, resolved, child.Name(), allowPath, selected)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, errOutsideSelection) {
			continue
		}
		if err != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Path: filepath.Join(source.Path, child.Name(), "SKILL.md"), Err: err,
			})
			continue
		}
		entries = append(entries, entry)
	}
	return entries, diagnostics
}

func discoverEntry(source Source, resolved, name string, allowPath func(string) bool, selected map[string]bool) (Entry, error) {
	if err := checkPath(filepath.Join(resolved, name), allowPath); err != nil {
		return Entry{}, err
	}
	// A linked package can live outside the source. Authorize its resolved
	// directory before opening it, then confine manifest reads to that package.
	path, err := filepath.EvalSymlinks(filepath.Join(resolved, name))
	if err != nil {
		return Entry{}, err
	}
	if err := checkPath(path, allowPath); err != nil {
		return Entry{}, err
	}
	if selected != nil && !selected[filepath.Join(path, "SKILL.md")] {
		return Entry{}, errOutsideSelection
	}
	directory, err := openSource(path)
	if err != nil {
		return Entry{}, err
	}
	defer directory.Close()
	file, err := checkedManifest(directory, path, allowPath)
	if err != nil {
		return Entry{}, err
	}
	defer file.Close()
	metadata, _, err := readMetadata(file)
	if err != nil {
		return Entry{}, err
	}
	if metadata.Name != filepath.Base(path) {
		return Entry{}, fmt.Errorf("skill name %q must match directory %q", metadata.Name, filepath.Base(path))
	}
	return Entry{
		Path: filepath.Join(path, "SKILL.md"), Source: source, Metadata: metadata,
		root: resolved, directory: path, allowPath: allowPath,
	}, nil
}
