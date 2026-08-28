package state

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/levmv/skot/internal/privatefs"
)

const (
	interactiveVersion    = 1
	maxModelHistory       = 20
	defaultLockTimeout    = time.Second
	interactiveStateLabel = "interactive state"
)

// WorkspaceSettings is the valid preference subset for one canonical
// workspace. A nil ReasoningEffort means no workspace override; a non-nil empty
// value means the user explicitly selected the provider default.
type WorkspaceSettings struct {
	Model           string
	ReasoningEffort *string
	ModelAPI        string
	ToolSet         string
	Scope           string
	AddedPaths      []string
	ProtectedPaths  []string
}

// ModelPreference is one remembered model selection. A nil ReasoningEffort
// means no override; a non-nil empty value means the provider default was
// explicitly selected. ModelAPI is the protocol the user attached to a route
// this build does not describe; it is an opaque string here, and the caller
// which owns the protocol vocabulary decides whether it still applies.
type ModelPreference struct {
	Model           string
	ReasoningEffort *string
	ModelAPI        string
}

// InteractiveSettings is one read-only view of the machine-owned interactive
// state. Notices describe known invalid values which were ignored without
// rewriting the source document.
type InteractiveSettings struct {
	Theme   string
	Display string
	// ModelHistory is every deliberate selection from any workspace, most
	// recent first.
	ModelHistory []ModelPreference
	Workspace    WorkspaceSettings
	Notices      []string
}

// LastModel is the selection a workspace without its own record starts from.
func (settings InteractiveSettings) LastModel() ModelPreference {
	if len(settings.ModelHistory) == 0 {
		return ModelPreference{}
	}
	return settings.ModelHistory[0]
}

type interactiveDocument struct {
	Version    int                          `json:"version"`
	UI         interactiveUIDocument        `json:"ui"`
	Workspaces map[string]workspaceDocument `json:"workspaces,omitempty"`
}

type interactiveUIDocument struct {
	Theme        *string                `json:"theme,omitzero"`
	Display      *string                `json:"display,omitzero"`
	ModelHistory []modelHistoryDocument `json:"model_history,omitempty"`

	// LegacyRecentModels accepts the pre-history key so an existing document
	// does not turn a strict decode into a startup failure. Its entries are
	// never exposed through Settings and are dropped on the next write.
	LegacyRecentModels jsontext.Value `json:"recent_models,omitzero"`
}

// modelHistoryDocument always carries the effort the selection was made with;
// "default" is the explicitly selected provider default. An api is recorded
// only for a selection whose protocol the user had to choose.
type modelHistoryDocument struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	API             string `json:"api,omitempty"`
}

// Pointer fields preserve absent versus present-but-invalid string values.
// Mutating a different field can therefore rewrite the document atomically
// without silently healing or deleting the invalid value.
type workspaceDocument struct {
	Model           *string  `json:"model,omitzero"`
	ReasoningEffort *string  `json:"reasoning_effort,omitzero"`
	ModelAPI        *string  `json:"model_api,omitzero"`
	ToolSet         *string  `json:"tool_set,omitzero"`
	Scope           *string  `json:"scope,omitzero"`
	AddedPaths      []string `json:"added_paths,omitempty"`
	ProtectedPaths  []string `json:"protected_paths,omitempty"`
}

// InteractiveStore is bound to one canonical workspace but coordinates
// mutations through the single home-wide interactive lock and JSON document.
type InteractiveStore struct {
	mu          sync.Mutex
	dir         string
	path        string
	lockPath    string
	workspace   string
	lockTimeout time.Duration
}

// OpenInteractive validates the interactive document without creating its lock
// file. Callers must only invoke it for interactive launches.
func OpenInteractive(home, workspace string) (*InteractiveStore, error) {
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace == "." || !filepath.IsAbs(workspace) {
		return nil, errors.New("interactive workspace must be an absolute path")
	}
	if err := inspectHome(home); err != nil {
		return nil, err
	}
	store := &InteractiveStore{
		dir: home, path: filepath.Join(home, "interactive.json"),
		lockPath: filepath.Join(home, "interactive.lock"), workspace: workspace,
		lockTimeout: defaultLockTimeout,
	}
	if err := privatefs.InspectRegularFile(store.path, interactiveStateLabel); err != nil {
		return nil, err
	}
	privatefs.TryRestrictPermissions(store.path)
	if _, err := store.loadDocument(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *InteractiveStore) Settings() (InteractiveSettings, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := privatefs.InspectRegularFile(store.path, interactiveStateLabel); err != nil {
		return InteractiveSettings{}, err
	}
	privatefs.TryRestrictPermissions(store.path)
	document, err := store.loadDocument()
	if err != nil {
		return InteractiveSettings{}, err
	}
	return document.settings(store.workspace), nil
}

// SetModelSelection records one deliberate selection. An empty api removes a
// protocol recorded earlier: the route either describes itself now or is being
// selected without that choice.
func (store *InteractiveStore) SetModelSelection(model, reasoningEffort, api string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model is required")
	}
	reasoningEffort = strings.ToLower(strings.TrimSpace(reasoningEffort))
	encodedEffort := reasoningEffort
	if encodedEffort == "" {
		encodedEffort = "default"
	}
	api = strings.ToLower(strings.TrimSpace(api))
	return store.mutate(func(document *interactiveDocument) bool {
		workspace := document.workspace(store.workspace)
		history := pushModelHistory(document.UI.ModelHistory, modelHistoryDocument{
			Model: model, ReasoningEffort: encodedEffort, API: api,
		})
		storedAPI := storedStringEquals(workspace.ModelAPI, api) || (workspace.ModelAPI == nil && api == "")
		if storedStringEquals(workspace.Model, model) && storedStringEquals(workspace.ReasoningEffort, encodedEffort) &&
			storedAPI && slices.Equal(history, document.UI.ModelHistory) {
			return false
		}
		workspace.Model = new(model)
		workspace.ReasoningEffort = new(encodedEffort)
		workspace.ModelAPI = nil
		if api != "" {
			workspace.ModelAPI = new(api)
		}
		document.Workspaces[store.workspace] = workspace
		document.UI.ModelHistory = history
		return true
	})
}

func (store *InteractiveStore) SetToolSetSelection(toolSet string) error {
	toolSet = strings.ToLower(strings.TrimSpace(toolSet))
	if toolSet == "" {
		return errors.New("tool set is required")
	}
	return store.mutate(func(document *interactiveDocument) bool {
		workspace := document.workspace(store.workspace)
		if storedStringEquals(workspace.ToolSet, toolSet) {
			return false
		}
		workspace.ToolSet = new(toolSet)
		document.Workspaces[store.workspace] = workspace
		return true
	})
}

func (store *InteractiveStore) SetScopeSelection(scope string) error {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if !validScope(scope) {
		return fmt.Errorf("invalid filesystem scope %q; expected workspace or machine", scope)
	}
	return store.mutate(func(document *interactiveDocument) bool {
		workspace := document.workspace(store.workspace)
		if storedStringEquals(workspace.Scope, scope) {
			return false
		}
		workspace.Scope = new(scope)
		document.Workspaces[store.workspace] = workspace
		return true
	})
}

// SetFilesystemPaths records the workspace-specific additions to filesystem
// access. Callers pass canonical paths after validating the live policy.
func (store *InteractiveStore) SetFilesystemPaths(addedPaths, protectedPaths []string) error {
	var err error
	addedPaths, err = normalizedPathList(addedPaths, "added path")
	if err != nil {
		return err
	}
	protectedPaths, err = normalizedPathList(protectedPaths, "protected path")
	if err != nil {
		return err
	}
	return store.mutate(func(document *interactiveDocument) bool {
		workspace := document.workspace(store.workspace)
		if slices.Equal(workspace.AddedPaths, addedPaths) &&
			slices.Equal(workspace.ProtectedPaths, protectedPaths) {
			return false
		}
		workspace.AddedPaths = addedPaths
		workspace.ProtectedPaths = protectedPaths
		document.Workspaces[store.workspace] = workspace
		return true
	})
}

func (store *InteractiveStore) SetThemeSelection(value string) error {
	theme, err := NormalizeTheme(value)
	if err != nil {
		return err
	}
	return store.mutate(func(document *interactiveDocument) bool {
		if storedStringEquals(document.UI.Theme, theme) {
			return false
		}
		document.UI.Theme = new(theme)
		return true
	})
}

func (store *InteractiveStore) SetDisplaySelection(value string) error {
	profile, err := NormalizeDisplayProfile(value)
	if err != nil {
		return err
	}
	return store.mutate(func(document *interactiveDocument) bool {
		if storedStringEquals(document.UI.Display, profile) {
			return false
		}
		document.UI.Display = new(profile)
		return true
	})
}

func (store *InteractiveStore) mutate(change func(*interactiveDocument) bool) (returnErr error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ensureHome(store.dir); err != nil {
		return err
	}
	lock, err := acquireInteractiveLock(store.lockPath, store.lockTimeout)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, releaseInteractiveLock(lock)) }()
	if err := privatefs.InspectRegularFile(store.path, interactiveStateLabel); err != nil {
		return err
	}
	privatefs.TryRestrictPermissions(store.path)
	document, err := store.loadDocument()
	if err != nil {
		return err
	}
	if !change(&document) {
		return nil
	}
	document.UI.LegacyRecentModels = nil
	return saveJSONAtomic(store.dir, store.path, "interactive", document)
}

func (store *InteractiveStore) loadDocument() (interactiveDocument, error) {
	document := interactiveDocument{Version: interactiveVersion}
	raw, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return interactiveDocument{}, fmt.Errorf("read interactive state: %w", err)
	}
	if err := json.Unmarshal(raw, &document, json.RejectUnknownMembers(true)); err != nil {
		return interactiveDocument{}, fmt.Errorf("decode interactive state: %w", err)
	}
	if document.Version != interactiveVersion {
		return interactiveDocument{}, fmt.Errorf("unsupported interactive state version %d (want %d)", document.Version, interactiveVersion)
	}
	return document, nil
}

func (document *interactiveDocument) workspace(root string) workspaceDocument {
	if document.Workspaces == nil {
		document.Workspaces = make(map[string]workspaceDocument)
	}
	return document.Workspaces[root]
}

func (document interactiveDocument) settings(workspace string) InteractiveSettings {
	settings := InteractiveSettings{}
	settings.ModelHistory = validModelHistory(document.UI.ModelHistory, &settings.Notices)
	if document.UI.Theme != nil {
		if theme, err := NormalizeTheme(*document.UI.Theme); err == nil {
			settings.Theme = theme
		} else {
			settings.Notices = append(settings.Notices, err.Error()+" in interactive state; ignored")
		}
	}
	if document.UI.Display != nil {
		if profile, err := NormalizeDisplayProfile(*document.UI.Display); err == nil {
			settings.Display = profile
		} else {
			settings.Notices = append(settings.Notices, err.Error()+" in interactive state; ignored")
		}
	}
	raw, exists := document.Workspaces[workspace]
	if !exists {
		return settings
	}
	preference := storedModelPreference(raw.Model, raw.ReasoningEffort, workspace, &settings.Notices)
	settings.Workspace.Model = preference.Model
	settings.Workspace.ReasoningEffort = preference.ReasoningEffort
	settings.Workspace.ModelAPI = strings.ToLower(validWorkspaceString("model_api", raw.ModelAPI, workspace, &settings.Notices))
	settings.Workspace.ToolSet = validWorkspaceString("tool_set", raw.ToolSet, workspace, &settings.Notices)
	if raw.Scope != nil {
		scope := strings.ToLower(strings.TrimSpace(*raw.Scope))
		if validScope(scope) {
			settings.Workspace.Scope = scope
		} else {
			settings.Notices = append(settings.Notices, fmt.Sprintf("invalid workspace scope %q for %s; ignored", *raw.Scope, workspace))
		}
	}
	settings.Workspace.AddedPaths = validWorkspacePathList("added_paths", raw.AddedPaths, workspace, &settings.Notices)
	settings.Workspace.ProtectedPaths = validWorkspacePathList("protected_paths", raw.ProtectedPaths, workspace, &settings.Notices)
	return settings
}

// storedModelPreference decodes the model and effort recorded for one
// workspace. Absent and present-but-invalid values stay distinguishable.
func storedModelPreference(model, effort *string, workspace string, notices *[]string) ModelPreference {
	preference := ModelPreference{}
	if model != nil {
		preference.Model = strings.TrimSpace(*model)
		if preference.Model == "" {
			*notices = append(*notices, fmt.Sprintf("empty workspace model for %s; ignored", workspace))
		}
	}
	if effort == nil {
		return preference
	}
	if strings.TrimSpace(*effort) == "" {
		*notices = append(*notices, fmt.Sprintf("empty reasoning_effort for %s; use \"default\" for provider default; ignored", workspace))
		return preference
	}
	preference.ReasoningEffort = decodeReasoningEffort(*effort)
	return preference
}

// decodeReasoningEffort maps a stored effort to an override. A nil result means
// no override; "default" means the provider default was chosen deliberately.
func decodeReasoningEffort(value string) *string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "":
		return nil
	case "default":
		return new("")
	default:
		return new(value)
	}
}

func validWorkspaceString(name string, value *string, workspace string, notices *[]string) string {
	if value == nil {
		return ""
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" {
		*notices = append(*notices, fmt.Sprintf("empty workspace %s for %s; ignored", name, workspace))
	}
	return normalized
}

func storedStringEquals(value *string, expected string) bool {
	return value != nil && *value == expected
}

func normalizedPathList(values []string, label string) ([]string, error) {
	result := make([]string, len(values))
	for index, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s %d is empty", label, index+1)
		}
		result[index] = value
	}
	return result, nil
}

func validWorkspacePathList(name string, value []string, workspace string, notices *[]string) []string {
	paths := make([]string, 0, len(value))
	for _, path := range value {
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, path)
		}
	}
	if len(paths) != len(value) {
		*notices = append(*notices, fmt.Sprintf("workspace %s for %s contains empty entries; ignored", name, workspace))
	}
	return paths
}

func validScope(value string) bool {
	switch value {
	case "workspace", "machine":
		return true
	default:
		return false
	}
}

func validModelHistory(entries []modelHistoryDocument, notices *[]string) []ModelPreference {
	history := make([]ModelPreference, 0, min(maxModelHistory, len(entries)))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		model := strings.TrimSpace(entry.Model)
		key := normalizeModelURI(model)
		if key == "" || len(history) == maxModelHistory {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		history = append(history, ModelPreference{
			Model: model, ReasoningEffort: decodeReasoningEffort(entry.ReasoningEffort),
			ModelAPI: strings.ToLower(strings.TrimSpace(entry.API)),
		})
	}
	if len(history) != len(entries) {
		*notices = append(*notices, "interactive model_history contains empty, duplicate, or excess entries; invalid entries were ignored")
	}
	return history
}

// pushModelHistory records one selection as the most recent, so the head is
// always the model a workspace without its own record starts from.
func pushModelHistory(history []modelHistoryDocument, selection modelHistoryDocument) []modelHistoryDocument {
	updated := make([]modelHistoryDocument, 0, min(maxModelHistory, len(history)+1))
	updated = append(updated, selection)
	seen := map[string]struct{}{normalizeModelURI(selection.Model): {}}
	for _, entry := range history {
		key := normalizeModelURI(entry.Model)
		if key == "" || len(updated) == maxModelHistory {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		updated = append(updated, entry)
	}
	return updated
}

func normalizeModelURI(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}
