package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/levmv/skot/internal/session"
)

const (
	interactiveVersion    = 1
	maxRecentModels       = 20
	defaultLockTimeout    = time.Second
	interactiveStateLabel = "interactive state"
)

// WorkspaceSettings is the valid preference subset for one canonical
// workspace. A nil ReasoningEffort means no workspace override; a non-nil empty
// value means the user explicitly selected the provider default.
type WorkspaceSettings struct {
	Model           string
	ReasoningEffort *string
	ToolSet         string
	Scope           string
}

// InteractiveSettings is one read-only view of the machine-owned interactive
// state. Notices describe known invalid values which were ignored without
// rewriting the source document.
type InteractiveSettings struct {
	Theme        string
	RecentModels []string
	Workspace    WorkspaceSettings
	Notices      []string
}

type interactiveDocument struct {
	Version    int                          `json:"version"`
	UI         interactiveUIDocument        `json:"ui,omitempty"`
	Workspaces map[string]workspaceDocument `json:"workspaces,omitempty"`
}

type interactiveUIDocument struct {
	Theme        *string  `json:"theme,omitempty"`
	RecentModels []string `json:"recent_models,omitempty"`
}

// Pointer fields preserve absent versus present-but-invalid values. Mutating a
// different field can therefore rewrite the document atomically without
// silently healing or deleting the invalid value.
type workspaceDocument struct {
	Model           *string `json:"model,omitempty"`
	ReasoningEffort *string `json:"reasoning_effort,omitempty"`
	ToolSet         *string `json:"tool_set,omitempty"`
	Scope           *string `json:"scope,omitempty"`
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
	dir, err := session.ResolveHome(home)
	if err != nil {
		return nil, err
	}
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace == "." || !filepath.IsAbs(workspace) {
		return nil, errors.New("interactive workspace must be an absolute path")
	}
	if err := inspectStateDirectory(dir, "Skot home"); err != nil {
		return nil, err
	}
	store := &InteractiveStore{
		dir: dir, path: filepath.Join(dir, "interactive.json"),
		lockPath: filepath.Join(dir, "interactive.lock"), workspace: workspace,
		lockTimeout: defaultLockTimeout,
	}
	if err := inspectStoreFile(store.path, interactiveStateLabel); err != nil {
		return nil, err
	}
	if _, err := store.loadDocument(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *InteractiveStore) Settings() (InteractiveSettings, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := inspectStoreFile(store.path, interactiveStateLabel); err != nil {
		return InteractiveSettings{}, err
	}
	document, err := store.loadDocument()
	if err != nil {
		return InteractiveSettings{}, err
	}
	return document.settings(store.workspace), nil
}

func (store *InteractiveStore) SetModelSelection(model, reasoningEffort string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return errors.New("model is required")
	}
	reasoningEffort = strings.ToLower(strings.TrimSpace(reasoningEffort))
	encodedEffort := reasoningEffort
	if encodedEffort == "" {
		encodedEffort = "default"
	}
	return store.mutate(func(document *interactiveDocument) bool {
		workspace := document.workspace(store.workspace)
		if storedStringEquals(workspace.Model, model) && storedStringEquals(workspace.ReasoningEffort, encodedEffort) {
			return false
		}
		previous := validStoredString(workspace.Model)
		workspace.Model = stringPointer(model)
		workspace.ReasoningEffort = stringPointer(encodedEffort)
		document.Workspaces[store.workspace] = workspace
		document.UI.RecentModels = recentModels(document.UI.RecentModels, previous, model)
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
		workspace.ToolSet = stringPointer(toolSet)
		document.Workspaces[store.workspace] = workspace
		return true
	})
}

func (store *InteractiveStore) SetScopeSelection(scope string) error {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if !validScope(scope) {
		return fmt.Errorf("invalid filesystem scope %q; expected auto, workspace, or machine", scope)
	}
	return store.mutate(func(document *interactiveDocument) bool {
		workspace := document.workspace(store.workspace)
		if storedStringEquals(workspace.Scope, scope) {
			return false
		}
		workspace.Scope = stringPointer(scope)
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
		document.UI.Theme = stringPointer(theme)
		return true
	})
}

func (store *InteractiveStore) mutate(change func(*interactiveDocument) bool) (returnErr error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ensureStateDirectory(store.dir, "Skot home"); err != nil {
		return err
	}
	lock, err := acquireInteractiveLock(store.lockPath, store.lockTimeout)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, releaseInteractiveLock(lock)) }()
	if err := inspectStoreFile(store.path, interactiveStateLabel); err != nil {
		return err
	}
	document, err := store.loadDocument()
	if err != nil {
		return err
	}
	if !change(&document) {
		return nil
	}
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return interactiveDocument{}, fmt.Errorf("decode interactive state: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return interactiveDocument{}, errors.New("decode interactive state: multiple JSON values")
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
	settings := InteractiveSettings{
		RecentModels: validRecentModels(document.UI.RecentModels),
	}
	if document.UI.Theme != nil {
		if theme, err := NormalizeTheme(*document.UI.Theme); err == nil {
			settings.Theme = theme
		} else {
			settings.Notices = append(settings.Notices, err.Error()+" in interactive state; ignored")
		}
	}
	if len(settings.RecentModels) != len(document.UI.RecentModels) {
		settings.Notices = append(settings.Notices, "interactive recent_models contains empty, duplicate, or excess entries; invalid entries were ignored")
	}
	raw, exists := document.Workspaces[workspace]
	if !exists {
		return settings
	}
	settings.Workspace.Model = validWorkspaceString("model", raw.Model, workspace, &settings.Notices)
	settings.Workspace.ToolSet = validWorkspaceString("tool_set", raw.ToolSet, workspace, &settings.Notices)
	if raw.Scope != nil {
		scope := strings.ToLower(strings.TrimSpace(*raw.Scope))
		if validScope(scope) {
			settings.Workspace.Scope = scope
		} else {
			settings.Notices = append(settings.Notices, fmt.Sprintf("invalid workspace scope %q for %s; ignored", *raw.Scope, workspace))
		}
	}
	if raw.ReasoningEffort != nil {
		effort := strings.ToLower(strings.TrimSpace(*raw.ReasoningEffort))
		switch effort {
		case "":
			settings.Notices = append(settings.Notices, fmt.Sprintf("empty reasoning_effort for %s; use \"default\" for provider default; ignored", workspace))
		case "default":
			settings.Workspace.ReasoningEffort = stringPointer("")
		default:
			settings.Workspace.ReasoningEffort = stringPointer(effort)
		}
	}
	return settings
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

func validStoredString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func storedStringEquals(value *string, expected string) bool {
	return value != nil && *value == expected
}

func validScope(value string) bool {
	switch value {
	case "auto", "workspace", "machine":
		return true
	default:
		return false
	}
}

func validRecentModels(models []string) []string {
	result := make([]string, 0, min(maxRecentModels, len(models)))
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		key := normalizeModelURI(model)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, model)
		if len(result) == maxRecentModels {
			break
		}
	}
	return result
}

func recentModels(models []string, previous, current string) []string {
	recent := make([]string, 0, min(maxRecentModels, len(models)+1))
	seen := make(map[string]struct{}, cap(recent)+1)
	currentKey := normalizeModelURI(current)
	if currentKey != "" {
		seen[currentKey] = struct{}{}
	}
	add := func(model string) {
		model = strings.TrimSpace(model)
		key := normalizeModelURI(model)
		if key == "" || len(recent) == maxRecentModels {
			return
		}
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		recent = append(recent, model)
	}
	add(previous)
	for _, model := range models {
		add(model)
	}
	return recent
}

func normalizeModelURI(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func stringPointer(value string) *string { return &value }
