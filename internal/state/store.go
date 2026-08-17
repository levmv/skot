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

	"github.com/levmv/skot/internal/session"
)

type Settings struct {
	ToolSets       map[string][]string `json:"tool_sets,omitempty"`
	AgentModels    []string            `json:"agent_models,omitempty"`
	ProtectedPaths []string            `json:"protected_paths,omitempty"`
}

// configDocument accepts the six pre-v1 interactive fields so upgrading does
// not turn a strict config decode into a startup failure. Their raw values are
// deliberately never exposed through Settings or written back by this store.
type configDocument struct {
	ToolSets       map[string][]string `json:"tool_sets,omitempty"`
	AgentModels    []string            `json:"agent_models,omitempty"`
	ProtectedPaths []string            `json:"protected_paths,omitempty"`

	LegacyModel           json.RawMessage `json:"model,omitempty"`
	LegacyReasoningEffort json.RawMessage `json:"reasoning_effort,omitempty"`
	LegacyRecentModels    json.RawMessage `json:"recent_models,omitempty"`
	LegacyToolSet         json.RawMessage `json:"tool_set,omitempty"`
	LegacyScope           json.RawMessage `json:"scope,omitempty"`
	LegacyTheme           json.RawMessage `json:"theme,omitempty"`
}

func (document configDocument) settings() Settings {
	return Settings{
		ToolSets: document.ToolSets, AgentModels: document.AgentModels,
		ProtectedPaths: document.ProtectedPaths,
	}
}

func (document configDocument) legacyInteractiveKeys() []string {
	fields := []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "model", raw: document.LegacyModel},
		{name: "reasoning_effort", raw: document.LegacyReasoningEffort},
		{name: "recent_models", raw: document.LegacyRecentModels},
		{name: "tool_set", raw: document.LegacyToolSet},
		{name: "scope", raw: document.LegacyScope},
		{name: "theme", raw: document.LegacyTheme},
	}
	keys := make([]string, 0, len(fields))
	for _, field := range fields {
		if field.raw != nil {
			keys = append(keys, field.name)
		}
	}
	return keys
}

const (
	ThemeAuto  = "auto"
	ThemeLight = "light"
	ThemeDark  = "dark"
)

func NormalizeTheme(value string) (string, error) {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "", ThemeAuto:
		return ThemeAuto, nil
	case ThemeLight, ThemeDark:
		return value, nil
	default:
		return "", fmt.Errorf("invalid terminal theme %q; expected auto, light, or dark", value)
	}
}

type CredentialProfile struct {
	Provider string          `json:"provider"`
	Kind     string          `json:"kind"`
	Payload  json.RawMessage `json:"payload"`
}

type credentialData struct {
	Profiles map[string]CredentialProfile `json:"profiles,omitempty"`
	Defaults map[string]string            `json:"defaults,omitempty"`
}

type apiKeyPayload struct {
	Token string `json:"token"`
}

type Store struct {
	mu       sync.Mutex
	dir      string
	path     string
	authPath string
}

func Open(home string) (*Store, error) {
	dir, err := session.ResolveHome(home)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create Skot home: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("restrict Skot home: %w", err)
	}
	store := &Store{dir: dir, path: filepath.Join(dir, "config.json"), authPath: filepath.Join(dir, "auth.json")}
	if err := inspectStoreFile(store.path, "config file"); err != nil {
		return nil, err
	}
	if err := inspectStoreFile(store.authPath, "credential store"); err != nil {
		return nil, err
	}
	if _, err := store.Settings(); err != nil {
		return nil, err
	}
	if _, err := store.loadCredentialsLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *Store) Settings() (Settings, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	document, err := store.loadConfig()
	if err != nil {
		return Settings{}, err
	}
	return document.settings(), nil
}

// LegacyInteractiveKeys reports deprecated keys which were accepted only to
// keep old pre-v1 config files readable. Callers decide whether their frontend
// should surface a cleanup notice.
func (store *Store) LegacyInteractiveKeys() ([]string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	document, err := store.loadConfig()
	if err != nil {
		return nil, err
	}
	return document.legacyInteractiveKeys(), nil
}

func (store *Store) APIKey(provider string) (string, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	provider = normalizeProvider(provider)
	credentials, err := store.loadCredentialsLocked()
	if err != nil {
		return "", false, err
	}
	name := credentials.Defaults[provider]
	profile, ok := credentials.Profiles[name]
	if !ok || normalizeProvider(profile.Provider) != provider || profile.Kind != "api_key" {
		return "", false, nil
	}
	var payload apiKeyPayload
	if err := json.Unmarshal(profile.Payload, &payload); err != nil {
		return "", false, fmt.Errorf("decode %s API key profile: %w", provider, err)
	}
	payload.Token = strings.TrimSpace(payload.Token)
	return payload.Token, payload.Token != "", nil
}

func (store *Store) SetAPIKey(provider, token string) error {
	provider = normalizeProvider(provider)
	token = strings.TrimSpace(token)
	if provider == "" || token == "" {
		return errors.New("provider and API key are required")
	}
	payload, err := json.Marshal(apiKeyPayload{Token: token})
	if err != nil {
		return fmt.Errorf("encode API key profile: %w", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	credentials, err := store.loadCredentialsLocked()
	if err != nil {
		return err
	}
	name := credentials.Defaults[provider]
	if name == "" {
		name = provider
	}
	credentials.Profiles[name] = CredentialProfile{Provider: provider, Kind: "api_key", Payload: payload}
	credentials.Defaults[provider] = name
	return store.saveJSON(store.authPath, "credentials", credentials)
}

func (store *Store) DeleteAPIKey(provider string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	provider = normalizeProvider(provider)
	credentials, err := store.loadCredentialsLocked()
	if err != nil {
		return err
	}
	name := credentials.Defaults[provider]
	if profile, ok := credentials.Profiles[name]; ok && normalizeProvider(profile.Provider) == provider && profile.Kind == "api_key" {
		delete(credentials.Profiles, name)
	}
	delete(credentials.Defaults, provider)
	return store.saveJSON(store.authPath, "credentials", credentials)
}

func (store *Store) Dir() string { return store.dir }

func (store *Store) loadConfig() (configDocument, error) {
	var document configDocument
	raw, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return document, nil
	}
	if err != nil {
		return configDocument{}, fmt.Errorf("read config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return configDocument{}, fmt.Errorf("decode config: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return configDocument{}, errors.New("decode config: multiple JSON values")
	}
	return document, nil
}

func (store *Store) loadCredentialsLocked() (credentialData, error) {
	credentials := credentialData{
		Profiles: make(map[string]CredentialProfile),
		Defaults: make(map[string]string),
	}
	raw, err := os.ReadFile(store.authPath)
	if errors.Is(err, os.ErrNotExist) {
		return credentials, nil
	}
	if err != nil {
		return credentialData{}, fmt.Errorf("read credentials: %w", err)
	}
	if err := json.Unmarshal(raw, &credentials); err != nil {
		return credentialData{}, fmt.Errorf("decode credentials: %w", err)
	}
	if credentials.Profiles == nil {
		credentials.Profiles = make(map[string]CredentialProfile)
	}
	if credentials.Defaults == nil {
		credentials.Defaults = make(map[string]string)
	}
	return credentials, nil
}

func (store *Store) saveJSON(path, label string, value any) error {
	return saveJSONAtomic(store.dir, path, label, value)
}

func saveJSONAtomic(dir, path, label string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", label, err)
	}
	file, err := os.CreateTemp(dir, "."+label+"-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", label, err)
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	fail := func(operation string, err error) error {
		_ = file.Close()
		return fmt.Errorf("%s %s: %w", operation, label, err)
	}
	if err := file.Chmod(0o600); err != nil {
		return fail("restrict temporary", err)
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return fail("write", err)
	}
	if err := file.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", label, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("publish %s: %w", label, err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync %s directory: %w", label, err)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func inspectStoreFile(path, label string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", label)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("restrict %s: %w", label, err)
	}
	return nil
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
