package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/levmv/skot/agent"
	"github.com/levmv/skot/internal/filesearch"
)

type workspace struct {
	searcher *filesearch.Searcher
	access   *FilesystemAccess
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
	access, err := NewFilesystemAccess(root, ScopeWorkspace, nil, protection)
	if err != nil {
		return nil, "", err
	}
	return NewWorkspaceToolsWithAccess(access)
}

// NewWorkspaceToolsWithAccess builds file tools backed by access. Sharing the
// same access with a ProcessManager keeps both enforcement routes on one
// atomically published policy.
func NewWorkspaceToolsWithAccess(access *FilesystemAccess) ([]agent.Tool, string, error) {
	workspace, err := newWorkspaceWithAccess(access)
	if err != nil {
		return nil, "", err
	}
	return workspace.tools(), workspace.searcher.Root(), nil
}

func (workspace *workspace) tools() []agent.Tool {
	tools := []agent.Tool{
		workspace.tool(
			"read",
			"Read a UTF-8 text file with stable line numbers. Use offset and limit to continue large files.",
			`{"type":"object","properties":{"path":{"type":"string","description":"File path. Relative paths start at the workspace; outside paths require machine scope or an added directory."},"offset":{"type":"integer","minimum":1,"description":"One-based first line to return. Defaults to 1."},"limit":{"type":"integer","minimum":1,"maximum":2000,"default":200,"description":"Maximum lines to return."}},"required":["path"],"additionalProperties":false}`,
			parallelSafe,
			workspace.read,
		),
		workspace.tool(
			"ls",
			"List one directory without ignore filtering or recursion. Includes hidden and .git entries, reports file/directory/symlink types and raw symlink targets, and does not follow listed entries.",
			`{"type":"object","properties":{"path":{"type":"string","description":"Directory path. Relative paths start at the workspace; outside paths require machine scope or an added directory. Defaults to the workspace."},"offset":{"type":"integer","minimum":1,"description":"One-based first entry to return. Defaults to 1."},"limit":{"type":"integer","minimum":1,"maximum":1000,"default":200,"description":"Maximum entries to return."}},"additionalProperties":false}`,
			parallelSafe,
			workspace.ls,
		),
		workspace.tool(
			"grep",
			"Regex-search UTF-8 files, including hidden files. Honors ignores; skips .git, binary/invalid UTF-8, discovered symlinks, and oversized lines. Results are capped.",
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Regular expression to search for."},"path":{"type":"string","description":"File or directory path. Relative paths start at the workspace; outside paths require machine scope or an added directory. Defaults to the workspace."},"include":{"type":"string","description":"Optional file glob such as *.go or **/*_test.go."}},"required":["pattern"],"additionalProperties":false}`,
			parallelSafe,
			workspace.grep,
		),
		workspace.tool(
			"glob",
			"Find regular files by glob, including hidden files. A positive glob may explicitly select ignored paths; .git and discovered symlinks are always skipped. Results are capped.",
			`{"type":"object","properties":{"pattern":{"type":"string","description":"Glob such as **/*.go."},"path":{"type":"string","description":"Directory path. Relative paths start at the workspace; outside paths require machine scope or an added directory. Defaults to the workspace."}},"required":["pattern"],"additionalProperties":false}`,
			parallelSafe,
			workspace.glob,
		),
		workspace.tool(
			"edit",
			"Replace one exact, unique text occurrence in a UTF-8 file atomically. Fails on ambiguous matches.",
			`{"type":"object","properties":{"path":{"type":"string","description":"File path. Relative paths start at the workspace; outside paths require machine scope or an added directory."},"old_text":{"type":"string","description":"Exact text that must occur exactly once."},"new_text":{"type":"string","description":"Replacement text; may be empty."}},"required":["path","old_text","new_text"],"additionalProperties":false}`,
			serialTool,
			workspace.edit,
		),
		workspace.tool(
			"write",
			"Create or replace one complete UTF-8 file atomically. To avoid overwriting a concurrent change, pass the sha256 returned by read or edit as expected_sha256.",
			`{"type":"object","properties":{"path":{"type":"string","description":"File path. Relative paths start at the workspace; any path resolving outside it requires machine scope."},"content":{"type":"string","description":"Complete new file content."},"expected_sha256":{"type":"string","description":"Optional sha256 returned by read or edit for the existing file; the write fails if it is stale."}},"required":["path","content"],"additionalProperties":false}`,
			serialTool,
			workspace.write,
		),
	}
	return tools
}

func newWorkspaceWithAccess(access *FilesystemAccess) (*workspace, error) {
	if access == nil {
		return nil, errors.New("filesystem access is nil")
	}
	policy := access.current.Load()
	if policy == nil {
		return nil, errors.New("filesystem access is uninitialized")
	}
	searcher, err := filesearch.New(policy.workspace, filesearch.Options{Exclude: policy.protection.Protects})
	if err != nil {
		return nil, err
	}
	return &workspace{searcher: searcher, access: access}, nil
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

func decodeArgs(raw string, target any) error {
	return agent.DecodeToolArguments(raw, target)
}

func clampLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	return min(value, maximum)
}

// isWithinRoot checks lexical containment only; it does not resolve symlinks.
func isWithinRoot(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
