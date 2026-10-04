package state

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/levmv/skot/internal/privatefs"
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

	LegacyModel           jsontext.Value `json:"model,omitzero"`
	LegacyReasoningEffort jsontext.Value `json:"reasoning_effort,omitzero"`
	LegacyRecentModels    jsontext.Value `json:"recent_models,omitzero"`
	LegacyToolSet         jsontext.Value `json:"tool_set,omitzero"`
	LegacyScope           jsontext.Value `json:"scope,omitzero"`
	LegacyTheme           jsontext.Value `json:"theme,omitzero"`
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
		raw  jsontext.Value
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
	Provider string         `json:"provider"`
	Kind     string         `json:"kind"`
	Payload  jsontext.Value `json:"payload"`
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
	store, err := OpenCredentials(home)
	if err != nil {
		return nil, err
	}
	if err := privatefs.InspectRegularFile(store.path, "config file"); err != nil {
		return nil, err
	}
	privatefs.TryRestrictPermissions(store.path)
	if _, err := store.Settings(); err != nil {
		return nil, err
	}
	return store, nil
}

// OpenCredentials opens auth.json independently of application configuration.
func OpenCredentials(home string) (*Store, error) {
	if err := inspectHome(home); err != nil {
		return nil, err
	}
	store := &Store{dir: home, path: filepath.Join(home, "config.json"), authPath: filepath.Join(home, "auth.json")}
	if err := privatefs.InspectRegularFile(store.authPath, "credential store"); err != nil {
		return nil, err
	}
	privatefs.TryRestrictPermissions(store.authPath)
	if _, err := store.loadCredentials(); err != nil {
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
	credentials, err := store.loadCredentials()
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

func (store *Store) SetAPIKey(ctx context.Context, provider, token string) error {
	provider = normalizeProvider(provider)
	token = strings.TrimSpace(token)
	if provider == "" || token == "" {
		return errors.New("provider and API key are required")
	}
	payload, err := json.Marshal(apiKeyPayload{Token: token}, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode API key profile: %w", err)
	}
	return store.UpdateCredential(ctx, provider, func(CredentialProfile) (CredentialProfile, error) {
		return CredentialProfile{Provider: provider, Kind: "api_key", Payload: payload}, nil
	})
}

func (store *Store) DeleteAPIKey(ctx context.Context, provider string) error {
	return store.UpdateCredential(ctx, provider, func(profile CredentialProfile) (CredentialProfile, error) {
		if profile.Kind == "api_key" {
			return CredentialProfile{}, nil
		}
		return profile, nil
	})
}

// Credential reads an atomic snapshot; the returned payload belongs to the
// caller. Reads need no process lock because writes replace the whole file.
func (store *Store) Credential(provider string) (CredentialProfile, error) {
	credentials, err := store.loadCredentials()
	if err != nil {
		return CredentialProfile{}, err
	}
	provider = normalizeProvider(provider)
	profile := credentials.Profiles[credentials.Defaults[provider]]
	if normalizeProvider(profile.Provider) != provider {
		return CredentialProfile{}, nil
	}
	return profile, nil
}

// UpdateCredential serializes read/modify/write across processes, including a
// remote OAuth refresh. Refresh tokens can rotate, so the callback must read
// the latest profile under this lock. An empty result deletes the credential.
func (store *Store) UpdateCredential(ctx context.Context, provider string, update func(CredentialProfile) (CredentialProfile, error)) error {
	provider = normalizeProvider(provider)
	if provider == "" {
		return errors.New("credential provider is required")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := privatefs.EnsureDirectory(store.dir, "state directory"); err != nil {
		return err
	}
	lock, err := acquireStateLock(ctx, filepath.Join(store.dir, "auth.lock"), "credential store")
	if err != nil {
		return err
	}
	defer releaseStateLock(lock)
	credentials, err := store.loadCredentials()
	if err != nil {
		return err
	}
	name := credentials.Defaults[provider]
	before := credentials.Profiles[name]
	if normalizeProvider(before.Provider) != provider {
		before = CredentialProfile{}
	}
	input := before
	input.Payload = bytes.Clone(before.Payload)
	if err := ctx.Err(); err != nil {
		return err
	}
	after, err := update(input)
	if err != nil {
		return err
	}
	// Once a refresh succeeds its rotated token must be saved even if the
	// request was cancelled during the exchange.
	if before.Provider == after.Provider && before.Kind == after.Kind && bytes.Equal(before.Payload, after.Payload) {
		return nil
	}
	if after.Kind == "" {
		delete(credentials.Profiles, name)
		delete(credentials.Defaults, provider)
	} else {
		if normalizeProvider(after.Provider) != provider {
			return errors.New("credential update changed provider")
		}
		if name == "" {
			name = provider
		}
		credentials.Profiles[name] = after
		credentials.Defaults[provider] = name
	}
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
	if err := json.Unmarshal(raw, &document, json.RejectUnknownMembers(true)); err != nil {
		return configDocument{}, fmt.Errorf("decode config: %w", err)
	}
	return document, nil
}

func (store *Store) loadCredentials() (credentialData, error) {
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
	raw, err := json.Marshal(value, json.Deterministic(true), jsontext.WithIndentPrefix(""), jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("encode %s: %w", label, err)
	}
	if err := ensureHome(dir); err != nil {
		return err
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

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
