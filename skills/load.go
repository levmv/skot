package skills

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Load reads the current document, rechecking AllowPath. SKILL.md symlinks
// must stay inside the package, and the name must match the discovered entry.
func (entry Entry) Load() (Document, error) {
	if entry.directory == "" {
		return Document{}, errors.New("skill was not discovered from a source")
	}
	if err := checkPath(entry.root, entry.allowPath); err != nil {
		return Document{}, err
	}
	if err := checkPath(entry.directory, entry.allowPath); err != nil {
		return Document{}, err
	}
	packagePath, err := filepath.EvalSymlinks(entry.directory)
	if err != nil {
		return Document{}, err
	}
	if err := checkPath(packagePath, entry.allowPath); err != nil {
		return Document{}, err
	}
	directory, err := openSource(packagePath)
	if err != nil {
		return Document{}, err
	}
	defer directory.Close()
	file, err := checkedManifest(directory, packagePath, entry.allowPath)
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		return Document{}, fmt.Errorf("read SKILL.md: %w", err)
	}
	document, err := Parse(string(raw))
	if err != nil {
		return Document{}, err
	}
	if document.Metadata.Name != entry.Metadata.Name {
		return Document{}, errors.New("skill name changed; discover it again")
	}
	return document, nil
}

func checkPath(path string, allowPath func(string) bool) error {
	if allowPath != nil && !allowPath(path) {
		return &os.PathError{Op: "read skill", Path: path, Err: os.ErrPermission}
	}
	return nil
}

func checkedManifest(root *os.Root, directory string, allowPath func(string) bool) (*os.File, error) {
	path := filepath.Join(directory, "SKILL.md")
	if err := checkPath(path, allowPath); err != nil {
		return nil, err
	}
	file, err := openManifest(root)
	if err != nil {
		return nil, err
	}
	// Check the resolved name against the opened handle before reading. A
	// symlink retargeted between discovery and loading must not bypass policy.
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		err = checkPath(resolved, allowPath)
	}
	if err == nil {
		var expected, opened os.FileInfo
		expected, err = os.Stat(resolved)
		if err == nil {
			opened, err = file.Stat()
		}
		if err == nil && !os.SameFile(expected, opened) {
			err = errors.New("skill manifest changed while opening")
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}
