package app

import (
	"context"
	"fmt"
	"slices"

	"github.com/levmv/skot/internal/canonicalpath"
	"github.com/levmv/skot/internal/state"
	workspacetools "github.com/levmv/skot/tools"
)

// FilesystemPathOrigin is the layer which contributed one effective path.
type FilesystemPathOrigin uint8

const (
	// FilesystemPathRemembered is stored for this workspace and is the only
	// origin a running session may add to or remove from.
	FilesystemPathRemembered FilesystemPathOrigin = iota
	// FilesystemPathInvocation was given to this run with -add-dir or
	// -protect-path and lasts until it exits.
	FilesystemPathInvocation
	// FilesystemPathSettings is a protected path loaded from config.json, which
	// applies to every run using that Skot data directory.
	FilesystemPathSettings
)

// FilesystemPath is one effective path of the live policy and where it came
// from. The path is canonical, so a caller may hand it straight back to a
// removal.
type FilesystemPath struct {
	Path   string
	Origin FilesystemPathOrigin
}

// FilesystemPaths reports the effective added directories and protected paths
// with the layer each came from.
func (application *Application) FilesystemPaths() (added, protected []FilesystemPath) {
	application.mu.RLock()
	additions, protection := application.state.additions, application.state.protection
	application.mu.RUnlock()
	added = describeFilesystemPaths(additions.Paths(), application.config.invocationAddedPaths, nil)
	protected = describeFilesystemPaths(protection.Paths(),
		application.config.invocationProtectedPaths, application.config.settingsProtectedPaths)
	return added, protected
}

// describeFilesystemPaths labels effective paths with the layer that
// contributed them; what no other layer supplied is remembered for this
// workspace. A path held by more than one layer keeps the origin a session
// cannot change, so a caller never offers to remove one which the next rebuild
// would restore.
func describeFilesystemPaths(effective, invocation, settings []string) []FilesystemPath {
	described := make([]FilesystemPath, 0, len(effective))
	for _, path := range effective {
		origin := FilesystemPathRemembered
		switch {
		case slices.Contains(settings, path):
			origin = FilesystemPathSettings
		case slices.Contains(invocation, path):
			origin = FilesystemPathInvocation
		}
		described = append(described, FilesystemPath{Path: path, Origin: origin})
	}
	return described
}

// filesystemLayers is the resolved filesystem policy of one run together with
// the layers it was built from. The layers decide what a running session may
// change: it owns only what the workspace remembered.
type filesystemLayers struct {
	additions  *workspacetools.AddedDirectoryPolicy
	protection *workspacetools.ProtectedPathPolicy

	invocationAdded     []string
	invocationProtected []string
	settingsProtected   []string
	workspaceAdded      []string
	workspaceProtected  []string
}

// resolveFilesystemLayers merges the paths this invocation was started with,
// the protected paths from config.json, and the paths remembered for this
// workspace. A remembered path which no longer resolves is reported as a notice
// and skipped: the run must still start. Invocation and settings paths are
// errors instead, because the user is naming them right now.
func resolveFilesystemLayers(root string, invocationAdded, invocationProtected, settingsProtected []string, remembered state.WorkspaceSettings) (filesystemLayers, []string, error) {
	invocationAdditions, err := workspacetools.NewAddedDirectoryPolicy(root, invocationAdded)
	if err != nil {
		return filesystemLayers{}, nil, fmt.Errorf("initialize added directories: %w", err)
	}
	settingsProtection, err := workspacetools.NewProtectedPathPolicy(root, settingsProtected)
	if err != nil {
		return filesystemLayers{}, nil, fmt.Errorf("initialize protected paths: %w", err)
	}
	invocationProtection, err := workspacetools.NewProtectedPathPolicy(root, invocationProtected)
	if err != nil {
		return filesystemLayers{}, nil, fmt.Errorf("initialize protected paths: %w", err)
	}
	layers := filesystemLayers{
		invocationAdded:     invocationAdditions.Paths(),
		invocationProtected: invocationProtection.Paths(),
		settingsProtected:   settingsProtection.Paths(),
	}
	workspaceAdded, notices := rememberedPaths(remembered.AddedPaths, "added path", root, resolveAddedDirectory)
	workspaceProtected, protectedNotices := rememberedPaths(remembered.ProtectedPaths, "protected path", root, resolveProtectedPath)
	layers.workspaceAdded, layers.workspaceProtected = workspaceAdded, workspaceProtected
	notices = append(notices, protectedNotices...)

	layers.additions, err = workspacetools.NewAddedDirectoryPolicy(root, slices.Concat(layers.invocationAdded, layers.workspaceAdded))
	if err != nil {
		return filesystemLayers{}, nil, fmt.Errorf("initialize added directories: %w", err)
	}
	layers.protection, err = workspacetools.NewProtectedPathPolicy(root,
		slices.Concat(layers.settingsProtected, layers.invocationProtected, layers.workspaceProtected))
	if err != nil {
		return filesystemLayers{}, nil, fmt.Errorf("initialize protected paths: %w", err)
	}
	return layers, notices, nil
}

// rememberedPaths keeps the stored values which still resolve and turns the
// rest into notices naming the workspace they were remembered for.
func rememberedPaths(values []string, subject, root string, resolve func(root, value string) (string, error)) ([]string, []string) {
	var paths, notices []string
	for _, value := range values {
		resolved, err := resolve(root, value)
		if err != nil {
			notices = append(notices, fmt.Sprintf("invalid workspace %s %q for %s; ignored: %v", subject, value, root, err))
			continue
		}
		paths = append(paths, resolved)
	}
	return paths, notices
}

// resolveAddedDirectory canonicalizes one added directory and refuses a tree
// the workspace already covers, which would add nothing.
func resolveAddedDirectory(root, value string) (string, error) {
	policy, err := workspacetools.NewAddedDirectoryPolicy(root, []string{value})
	if err != nil {
		return "", err
	}
	resolved := policy.Paths()
	if len(resolved) == 0 {
		return "", fmt.Errorf("%s is already inside the workspace", value)
	}
	return resolved[0], nil
}

// resolveProtectedPath canonicalizes one protected path and refuses the only
// value which would make the workspace itself unreachable.
func resolveProtectedPath(root, value string) (string, error) {
	resolved, err := workspacetools.ResolvePolicyPath(root, value)
	if err != nil {
		return "", err
	}
	if canonicalpath.Contains(resolved, root) {
		return "", fmt.Errorf("%s contains the workspace", resolved)
	}
	return resolved, nil
}

// AddDirectory extends workspace scope with one directory tree and remembers it
// for this workspace. The tree must exist and lie outside the workspace, which
// already grants access to everything inside it.
func (application *Application) AddDirectory(ctx context.Context, path string) error {
	application.filesystemMu.Lock()
	defer application.filesystemMu.Unlock()

	directory, err := resolveAddedDirectory(application.config.root, path)
	if err != nil {
		return err
	}
	application.mu.RLock()
	additions := application.state.additions
	application.mu.RUnlock()
	added, protected := application.rememberedPathLists()
	if covering := coveringPath(additions.Paths(), directory); covering != "" {
		if covering == directory {
			return fmt.Errorf("%s is already an added directory", covering)
		}
		return fmt.Errorf("%s is already covered by added directory %s", directory, covering)
	}
	return application.applyWorkspaceFilesystemPaths(ctx, addPolicyPath(added, directory), protected)
}

// RemoveAddedDirectory drops one remembered added directory. Directories from
// -add-dir belong to this invocation and are refused rather than silently
// restored by the next start.
func (application *Application) RemoveAddedDirectory(ctx context.Context, path string) error {
	application.filesystemMu.Lock()
	defer application.filesystemMu.Unlock()

	resolved, err := workspacetools.ResolvePolicyPath(application.config.root, path)
	if err != nil {
		return err
	}
	added, protected := application.rememberedPathLists()
	next, removed := removePolicyPath(added, resolved)
	if !removed {
		effective, _ := application.FilesystemPaths()
		return refuseRemoval(effective, resolved, "added directory", "-add-dir")
	}
	return application.applyWorkspaceFilesystemPaths(ctx, next, protected)
}

// ProtectPath hides one path from model-owned tools and remembers it for this
// workspace. The path does not have to exist yet.
func (application *Application) ProtectPath(ctx context.Context, path string) error {
	application.filesystemMu.Lock()
	defer application.filesystemMu.Unlock()

	resolved, err := resolveProtectedPath(application.config.root, path)
	if err != nil {
		return err
	}
	application.mu.RLock()
	protection := application.state.protection
	application.mu.RUnlock()
	added, protected := application.rememberedPathLists()
	if covering := coveringPath(protection.Paths(), resolved); covering != "" {
		if covering == resolved {
			return fmt.Errorf("%s is already protected", covering)
		}
		return fmt.Errorf("%s is already covered by protected path %s", resolved, covering)
	}
	return application.applyWorkspaceFilesystemPaths(ctx, added, addPolicyPath(protected, resolved))
}

// UnprotectPath drops one remembered protected path. Protection from
// config.json or -protect-path is not weakened from a running session.
func (application *Application) UnprotectPath(ctx context.Context, path string) error {
	application.filesystemMu.Lock()
	defer application.filesystemMu.Unlock()

	resolved, err := workspacetools.ResolvePolicyPath(application.config.root, path)
	if err != nil {
		return err
	}
	added, protected := application.rememberedPathLists()
	next, removed := removePolicyPath(protected, resolved)
	if !removed {
		_, effective := application.FilesystemPaths()
		return refuseRemoval(effective, resolved, "protected path", "-protect-path")
	}
	return application.applyWorkspaceFilesystemPaths(ctx, added, next)
}

// rememberedPathLists copies the added and protected paths this workspace
// remembers, which are the only lists a session rewrites.
func (application *Application) rememberedPathLists() (added, protected []string) {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return slices.Clone(application.state.workspaceAddedPaths), slices.Clone(application.state.workspaceProtectedPaths)
}

// applyWorkspaceFilesystemPaths installs the remembered paths on top of the
// ones this invocation started with as a single transaction, then persists
// them. A failure to persist leaves the live policy in force, like every other
// remembered preference. filesystemMu must be held by the caller.
func (application *Application) applyWorkspaceFilesystemPaths(ctx context.Context, addedPaths, protectedPaths []string) error {
	root := application.config.root
	additions, err := workspacetools.NewAddedDirectoryPolicy(root, slices.Concat(application.config.invocationAddedPaths, addedPaths))
	if err != nil {
		return err
	}
	protection, err := workspacetools.NewProtectedPathPolicy(root, slices.Concat(
		application.config.settingsProtectedPaths, application.config.invocationProtectedPaths, protectedPaths))
	if err != nil {
		return err
	}
	application.mu.RLock()
	scope := application.state.security.Scope
	application.mu.RUnlock()
	if err := application.applyFilesystemPolicy(ctx, scope, additions, protection); err != nil {
		return err
	}
	application.mu.Lock()
	application.state.workspaceAddedPaths = addedPaths
	application.state.workspaceProtectedPaths = protectedPaths
	application.mu.Unlock()
	return application.persistInteractivePreference("filesystem paths", func(preferences *state.InteractiveStore) error {
		return preferences.SetFilesystemPaths(addedPaths, protectedPaths)
	})
}

// addPolicyPath appends path and drops the entries it covers, so the stored
// list keeps matching the compacted policy the user sees.
func addPolicyPath(paths []string, path string) []string {
	next := make([]string, 0, len(paths)+1)
	for _, existing := range paths {
		if canonicalpath.Contains(path, existing) {
			continue
		}
		next = append(next, existing)
	}
	return append(next, path)
}

func removePolicyPath(paths []string, path string) ([]string, bool) {
	next := make([]string, 0, len(paths))
	for _, existing := range paths {
		if existing == path {
			continue
		}
		next = append(next, existing)
	}
	return next, len(next) != len(paths)
}

// coveringPath returns the effective entry which already covers path, if any.
func coveringPath(paths []string, path string) string {
	for _, existing := range paths {
		if canonicalpath.Contains(existing, path) {
			return existing
		}
	}
	return ""
}

// refuseRemoval explains why a path is not the session's to drop, naming the
// layer which owns it. Origins come from the effective list, so one place
// decides who owns a path.
func refuseRemoval(effective []FilesystemPath, path, subject, flag string) error {
	for _, entry := range effective {
		if entry.Path != path {
			continue
		}
		switch entry.Origin {
		case FilesystemPathSettings:
			return fmt.Errorf("%s comes from config.json", path)
		case FilesystemPathInvocation:
			return fmt.Errorf("%s was given with %s for this run", path, flag)
		}
	}
	if covering := coveringPath(filesystemPathValues(effective), path); covering != "" && covering != path {
		return fmt.Errorf("%s is covered by %s %s; remove that one instead", path, subject, covering)
	}
	return fmt.Errorf("%s is not a remembered %s", path, subject)
}

func filesystemPathValues(paths []FilesystemPath) []string {
	values := make([]string, 0, len(paths))
	for _, path := range paths {
		values = append(values, path.Path)
	}
	return values
}
