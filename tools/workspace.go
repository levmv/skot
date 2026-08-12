package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/filesearch"
)

type workspace struct {
	root       string
	searcher   *filesearch.Searcher
	protection *ProtectedPathPolicy
}

const (
	parallelSafe = true
	serialTool   = false
)

// NewWorkspaceTools builds the initial file tools and returns their canonical,
// symlink-free workspace root.
func NewWorkspaceTools(root string) ([]agent.Tool, string, error) {
	return NewWorkspaceToolsWithProtection(root, nil)
}

// NewWorkspaceToolsWithProtection applies one shared protected-path policy to
// all model-owned file operations.
func NewWorkspaceToolsWithProtection(root string, protection *ProtectedPathPolicy) ([]agent.Tool, string, error) {
	workspace, err := newWorkspaceWithProtection(root, protection)
	if err != nil {
		return nil, "", err
	}
	tools := []agent.Tool{
		workspace.tool(
			"read",
			"Read a UTF-8 text file with stable line numbers. Use offset and limit to continue large files.",
			`{"type":"object","properties":{"path":{"type":"string","description":"File path relative to the workspace root."},"offset":{"type":"integer","minimum":1,"description":"One-based first line to return. Defaults to 1."},"limit":{"type":"integer","minimum":1,"maximum":2000,"default":200,"description":"Maximum lines to return."}},"required":["path"],"additionalProperties":false}`,
			parallelSafe,
			workspace.read,
		),
		workspace.tool(
			"ls",
			"List one directory without ignore filtering or recursion. Includes hidden and .git entries, reports file/directory/symlink types and raw symlink targets, and does not follow listed entries.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Directory relative to the workspace root. Defaults to the root."},"offset":{"type":"integer","minimum":1,"description":"One-based first entry to return. Defaults to 1."},"limit":{"type":"integer","minimum":1,"maximum":1000,"default":200,"description":"Maximum entries to return."}},"additionalProperties":false}`,
			parallelSafe,
			workspace.ls,
		),
		workspace.tool(
			"grep",
			"Regex-search UTF-8 repository files, including hidden files. Honors ignores; skips .git, binary/invalid UTF-8, discovered symlinks, and oversized lines. Results are capped.",
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression to search for."},"path":{"type":"string","description":"File or directory relative to the workspace root. Defaults to the root."},"include":{"type":"string","description":"Optional file glob such as *.go or **/*_test.go."}},"required":["pattern"],"additionalProperties":false}`,
			parallelSafe,
			workspace.grep,
		),
		workspace.tool(
			"glob",
			"Find regular repository files by glob, including hidden files. A positive glob may explicitly select repository-ignored paths; .git and discovered symlinks are always skipped. Results are capped.",
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob such as **/*.go."},"path":{"type":"string","description":"Directory relative to the workspace root. Defaults to the root."}},"required":["pattern"],"additionalProperties":false}`,
			parallelSafe,
			workspace.glob,
		),
		workspace.tool(
			"edit",
			"Replace one exact, unique text occurrence in a UTF-8 file atomically. Fails on ambiguous matches.",
			`{"type":"object","properties":{"path":{"type":"string","description":"File path relative to the workspace root."},"old_text":{"type":"string","description":"Exact text that must occur exactly once."},"new_text":{"type":"string","description":"Replacement text; may be empty."}},"required":["path","old_text","new_text"],"additionalProperties":false}`,
			serialTool,
			workspace.edit,
		),
		workspace.tool(
			"write",
			"Create or replace one complete UTF-8 file atomically. To avoid overwriting a concurrent change, pass the sha256 returned by read or edit as expected_sha256.",
			`{"type":"object","properties":{"path":{"type":"string","description":"File path relative to the workspace root."},"content":{"type":"string","description":"Complete new file content."},"expected_sha256":{"type":"string","description":"Optional sha256 returned by read or edit for the existing file; the write fails if it is stale."}},"required":["path","content"],"additionalProperties":false}`,
			serialTool,
			workspace.write,
		),
	}
	return tools, workspace.root, nil
}

func newWorkspace(root string) (*workspace, error) {
	return newWorkspaceWithProtection(root, nil)
}

func newWorkspaceWithProtection(root string, protection *ProtectedPathPolicy) (*workspace, error) {
	abs, err := ResolveWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	searcher, err := filesearch.New(abs, filesearch.Options{Exclude: protection.Protects})
	if err != nil {
		return nil, err
	}
	return &workspace{root: abs, searcher: searcher, protection: protection}, nil
}

// ResolveWorkspaceRoot returns an absolute, symlink-free directory path.
func ResolveWorkspaceRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "."
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace root is not a directory: %s", abs)
	}
	return filepath.Clean(abs), nil
}

func (workspace *workspace) tool(name, description, schema string, parallelSafe bool, run func(context.Context, string) (agent.ToolOutput, error)) agent.Tool {
	return agent.Tool{
		Spec: agent.ToolSpec{
			Name:         name,
			Description:  description,
			InputSchema:  json.RawMessage(schema),
			ParallelSafe: parallelSafe,
		},
		Run: run,
	}
}

func (workspace *workspace) resolvePath(path string) (string, string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	if filepath.IsAbs(path) {
		return "", "", fmt.Errorf("absolute paths are not allowed: %s", path)
	}
	abs := filepath.Clean(filepath.Join(workspace.root, filepath.Clean(path)))
	if !isWithinRoot(workspace.root, abs) {
		return "", "", fmt.Errorf("path escapes workspace root: %s", path)
	}
	return abs, workspace.displayPath(abs), nil
}

func (workspace *workspace) resolveExistingPath(path string) (string, string, os.FileInfo, error) {
	return workspace.resolveExistingPathWithProtection(path, true)
}

func (workspace *workspace) resolveExistingPathWithProtection(path string, enforce bool) (string, string, os.FileInfo, error) {
	abs, display, err := workspace.resolvePath(path)
	if err != nil {
		return "", "", nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", nil, err
	}
	if !isWithinRoot(workspace.root, resolved) {
		return "", "", nil, fmt.Errorf("symlink target escapes workspace root: %s", display)
	}
	if enforce && (workspace.protected(abs) || workspace.protected(resolved)) {
		return "", "", nil, fmt.Errorf("path is protected: %s", display)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", "", nil, fmt.Errorf("%s is not a regular file or directory", display)
	}
	return resolved, display, info, nil
}

func (workspace *workspace) protected(path string) bool {
	return workspace != nil && workspace.protection != nil && workspace.protection.Protects(path)
}

func (workspace *workspace) resolveReadableFile(path string) (string, string, error) {
	abs, display, info, err := workspace.resolveExistingPath(path)
	if err != nil {
		return "", "", err
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("%s is a directory", display)
	}
	return abs, display, nil
}

func (workspace *workspace) displayPath(path string) string {
	relative, err := filepath.Rel(workspace.root, path)
	if err != nil || relative == "." {
		return "."
	}
	return filepath.ToSlash(relative)
}

func decodeArgs(raw string, target any) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid tool arguments: multiple JSON values")
	}
	return nil
}

func clampLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	return min(value, maximum)
}

func isWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
