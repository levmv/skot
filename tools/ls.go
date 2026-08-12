package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/levmv/skot/agent"
)

const (
	defaultListEntries    = 200
	maxListEntries        = 1000
	maxListDirOutputBytes = 256 * 1024
)

func (workspace *workspace) ls(ctx context.Context, raw string) (agent.ToolOutput, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolOutput{}, err
	}
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return agent.ToolOutput{}, err
	}
	abs, display, info, err := workspace.resolveExistingPath(args.Path)
	if err != nil {
		return agent.ToolOutput{}, err
	}
	if !info.IsDir() {
		return agent.ToolOutput{}, fmt.Errorf("%s is not a directory", display)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return agent.ToolOutput{}, fmt.Errorf("list directory %s: %w", display, err)
	}
	visible := entries[:0]
	for _, entry := range entries {
		entryPath := filepath.Join(abs, entry.Name())
		if workspace.protected(entryPath) {
			continue
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			return agent.ToolOutput{}, fmt.Errorf("inspect entry %s: %w", displayPath(entry.Name()), infoErr)
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			if resolved, resolveErr := filepath.EvalSymlinks(entryPath); resolveErr == nil && workspace.protected(resolved) {
				continue
			}
		}
		visible = append(visible, entry)
	}
	entries = visible
	offset := args.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := clampLimit(args.Limit, defaultListEntries, maxListEntries)
	start := min(offset-1, len(entries))
	if start >= len(entries) {
		return agent.ToolOutput{Content: "no entries\n"}, nil
	}
	lines := make([]string, 0, min(limit, len(entries)-start))
	usedBytes := 0
	nextIndex := start
	for nextIndex < len(entries) && len(lines) < limit {
		if err := ctx.Err(); err != nil {
			return agent.ToolOutput{}, err
		}
		line, err := formatDirectoryEntry(abs, entries[nextIndex])
		if err != nil {
			return agent.ToolOutput{}, fmt.Errorf("list directory %s: %w", display, err)
		}
		if len(lines) > 0 && usedBytes+len(line)+1 > maxListDirOutputBytes {
			break
		}
		lines = append(lines, line)
		usedBytes += len(line) + 1
		nextIndex++
	}
	var output strings.Builder
	fmt.Fprintf(&output, "entries: %d\n", len(lines))
	if nextIndex < len(entries) {
		output.WriteString("truncated: true\n")
		fmt.Fprintf(&output, "continue: ls(path=%q, offset=%d, limit=%d)\n", display, nextIndex+1, limit)
	}
	output.WriteByte('\n')
	output.WriteString(strings.Join(lines, "\n"))
	output.WriteByte('\n')
	return agent.ToolOutput{Content: output.String()}, nil
}

func formatDirectoryEntry(directory string, entry os.DirEntry) (string, error) {
	name := entry.Name()
	displayName := displayPath(name)
	info, err := entry.Info()
	if err != nil {
		return "", fmt.Errorf("inspect entry %s: %w", displayName, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(filepath.Join(directory, name))
		if err != nil {
			return "", fmt.Errorf("read symlink %s: %w", displayName, err)
		}
		return "symlink\t" + displayName + " -> " + displayPath(target), nil
	}
	if info.IsDir() {
		return "dir\t" + displayPath(name+"/"), nil
	}
	if info.Mode().IsRegular() {
		return "file\t" + displayName, nil
	}
	return "other\t" + displayName + " (mode " + info.Mode().String() + ")", nil
}

func displayPath(path string) string {
	if path == "" || strings.IndexFunc(path, func(value rune) bool {
		return unicode.IsSpace(value) || unicode.IsControl(value) || value == '"' || value == '\\'
	}) >= 0 {
		return strconv.Quote(path)
	}
	return path
}
