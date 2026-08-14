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
	Model           string              `json:"model,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	RecentModels    []string            `json:"recent_models,omitempty"`
	Profile         string              `json:"profile,omitempty"`
	Profiles        map[string][]string `json:"profiles,omitempty"`
	AgentModels     []string            `json:"agent_models,omitempty"`
	Sandbox         string              `json:"sandbox,omitempty"`
	ProtectedPaths  []string            `json:"protected_paths,omitempty"`
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

const maxRecentModels = 20

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
	return store.load()
}

func (store *Store) SetDefaultModel(model string) error {
	return store.SetDefaultModelSelection(model, "")
}

func (store *Store) SetDefaultModelSelection(model, reasoningEffort string) error {
	model = strings.TrimSpace(model)
	return store.update(func(settings *Settings) {
		previous := settings.Model
		settings.Model = model
		settings.ReasoningEffort = strings.ToLower(strings.TrimSpace(reasoningEffort))
		settings.RecentModels = recentModels(settings.RecentModels, previous, model)
	})
}

func (store *Store) SetDefaultProfile(profile string) error {
	return store.update(func(settings *Settings) { settings.Profile = strings.TrimSpace(profile) })
}

func (store *Store) SetDefaultSandbox(policy string) error {
	return store.update(func(settings *Settings) { settings.Sandbox = strings.TrimSpace(policy) })
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

func (store *Store) update(change func(*Settings)) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	settings, err := store.load()
	if err != nil {
		return err
	}
	change(&settings)
	return store.save(settings)
}

func (store *Store) load() (Settings, error) {
	var settings Settings
	raw, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read config: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return Settings{}, fmt.Errorf("decode config: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Settings{}, errors.New("decode config: multiple JSON values")
	}
	return settings, nil
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

func (store *Store) save(settings Settings) error {
	return store.saveJSON(store.path, "config", settings)
}

func (store *Store) saveJSON(path, label string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", label, err)
	}
	file, err := os.CreateTemp(store.dir, "."+label+"-*.tmp")
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
	return nil
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

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}
