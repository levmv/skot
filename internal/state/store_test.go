package state

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStoreLoadsAuthoredConfigAndKeepsLegacyInteractiveFieldsInert(t *testing.T) {
	home := t.TempDir()
	raw := `{"tool_sets":{"review":["read","grep"]},"agent_models":["openai/gpt-5-mini"],"protected_paths":[".env","~/private"],"model":"old/model","reasoning_effort":"high","recent_models":["older/model"],"tool_set":"edit","scope":"machine","theme":"dark"}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(raw), 0o400); err != nil {
		t.Fatal(err)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(settings.ToolSets["review"], ","); got != "read,grep" {
		t.Fatalf("tool sets = %#v", settings.ToolSets)
	}
	if got := strings.Join(settings.ProtectedPaths, ","); got != ".env,~/private" {
		t.Fatalf("protected paths = %#v", settings.ProtectedPaths)
	}
	if got := strings.Join(settings.AgentModels, ","); got != "openai/gpt-5-mini" {
		t.Fatalf("agent models = %#v", settings.AgentModels)
	}
	legacy, err := store.LegacyInteractiveKeys()
	if err != nil {
		t.Fatal(err)
	}
	wantLegacy := []string{"model", "reasoning_effort", "recent_models", "tool_set", "scope", "theme"}
	if !slices.Equal(legacy, wantLegacy) {
		t.Fatalf("legacy fields = %#v", legacy)
	}
	if persisted, err := os.ReadFile(filepath.Join(home, "config.json")); err != nil || string(persisted) != raw {
		t.Fatalf("config changed = %q, %v", persisted, err)
	}
	info, err := os.Stat(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
}

func TestStoreRejectsSharedConfigPermissionsWithoutChangingThem(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home); err == nil || !strings.Contains(err.Error(), "permissions 0640") {
		t.Fatalf("error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("config mode = %v, %v", info, err)
	}
}

func TestThemeNormalizationDefaultsToAutoAndRejectsInvalidValues(t *testing.T) {
	if got, err := NormalizeTheme(""); err != nil || got != ThemeAuto {
		t.Fatalf("default theme = %q, %v", got, err)
	}
	if _, err := NormalizeTheme("sepia"); err == nil {
		t.Fatal("invalid theme accepted")
	}
}

func TestStoreRejectsSymlinkedConfigFile(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "config.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(home); err == nil {
		t.Fatal("symlinked config file was accepted")
	}
}

func TestStoreRejectsUnknownConfigFields(t *testing.T) {
	for field, raw := range map[string]string{
		"protected_path": `{"protected_path":[".env"]}`,
		"sandbox":        `{"sandbox":"off"}`,
	} {
		t.Run(field, func(t *testing.T) {
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(home); err == nil || !strings.Contains(err.Error(), `unknown field "`+field+`"`) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestStoreRejectsSymlinkedCredentials(t *testing.T) {
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, "auth.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(home); err == nil {
		t.Fatal("symlinked credentials were accepted")
	}
}

func TestStoreKeepsNamedCredentialsSeparateFromSettings(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"protected_paths":[".env"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(" DeepSeek ", " secret-token "); err != nil {
		t.Fatal(err)
	}
	token, ok, err := store.APIKey("deepseek")
	if err != nil || !ok || token != "secret-token" {
		t.Fatalf("API key = %q, %v, %v", token, ok, err)
	}
	configRaw, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	authRaw, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configRaw), "secret-token") || !strings.Contains(string(authRaw), `"kind": "api_key"`) || !strings.Contains(string(authRaw), `"deepseek": "deepseek"`) {
		t.Fatalf("config/auth separation failed: config=%s auth=%s", configRaw, authRaw)
	}
	info, err := os.Stat(filepath.Join(home, "auth.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auth mode = %v, %v", info, err)
	}
	if err := store.DeleteAPIKey("deepseek"); err != nil {
		t.Fatal(err)
	}
	if token, ok, err := store.APIKey("deepseek"); err != nil || ok || token != "" {
		t.Fatalf("deleted API key = %q, %v, %v", token, ok, err)
	}
}
