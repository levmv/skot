package ui

import (
	"os"
	"path/filepath"
	"strings"
)

// pathCompletionLimit keeps one crowded directory from filling the screen.
const pathCompletionLimit = 50

// pathCompletionCache holds one directory listing for as long as a prompt keeps
// typing inside it. Without it every keystroke would read the directory again,
// which is a syscall per key and painful on a slow or network filesystem.
type pathCompletionCache struct {
	base  string
	names []string
}

func (cache *pathCompletionCache) directories(base string) []string {
	if cache.base != base {
		cache.base, cache.names = base, readCompletionDirectories(base)
	}
	return cache.names
}

func (cache *pathCompletionCache) reset() { *cache = pathCompletionCache{} }

// pathCompletionCandidates lists directories which continue the typed value.
// The user is naming a directory the model cannot reach yet, so completion
// reads the real filesystem instead of going through the model's tools. Every
// candidate keeps the form the user is typing: a relative prefix stays
// relative, "~/" stays home-based.
func pathCompletionCandidates(cache *pathCompletionCache, root, typed string) []string {
	typed = strings.TrimLeft(typed, " \t")
	if typed == "" {
		return nil
	}
	prefix := ""
	directory := typed
	if !strings.HasSuffix(typed, string(filepath.Separator)) {
		directory, prefix = filepath.Split(typed)
	}
	base, err := completionBase(root, directory)
	if err != nil {
		return nil
	}
	candidates := make([]string, 0, pathCompletionLimit)
	for _, name := range cache.directories(base) {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		// Hidden directories stay out of the way until they are asked for by
		// name, the way shell completion treats them.
		if strings.HasPrefix(name, ".") && !strings.HasPrefix(prefix, ".") {
			continue
		}
		candidates = append(candidates, directory+name+string(filepath.Separator))
		if len(candidates) == pathCompletionLimit {
			break
		}
	}
	return candidates
}

// readCompletionDirectories resolves the directory entries once, including the
// symlink checks, so filtering by prefix later costs nothing.
func readCompletionDirectories(base string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !isCompletionDirectory(filepath.Join(base, entry.Name()), entry) {
			continue
		}
		names = append(names, entry.Name())
	}
	return names
}

// completionBase resolves the directory part of a typed value the same way the
// filesystem policy resolves a stored one, so a completed path stays valid.
func completionBase(root, directory string) (string, error) {
	switch {
	case directory == "":
		return root, nil
	case directory == "~", strings.HasPrefix(directory, "~"+string(filepath.Separator)):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(directory, "~")), nil
	case filepath.IsAbs(directory):
		return directory, nil
	default:
		return filepath.Join(root, directory), nil
	}
}

// isCompletionDirectory also accepts a symlink to a directory: those are
// ordinary targets, and the policy resolves them when the path is added.
func isCompletionDirectory(path string, entry os.DirEntry) bool {
	if entry.IsDir() {
		return true
	}
	if entry.Type()&os.ModeSymlink == 0 {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
