package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePersistsSettingsAtomicallyAndPrivately(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"profiles":{"review":["read","grep"]},"agent_models":["openai/gpt-5-mini"],"protected_paths":[".env","~/private"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultModel("openrouter/example/model"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultModelSelection("deepseek/next-model", " HIGH "); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultProfile("edit"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultSandbox("off"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := reopened.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Model != "deepseek/next-model" || settings.ReasoningEffort != "high" || len(settings.RecentModels) != 1 || settings.RecentModels[0] != "openrouter/example/model" || settings.Profile != "edit" || settings.Sandbox != "off" {
		t.Fatalf("settings = %#v", settings)
	}
	if got := strings.Join(settings.Profiles["review"], ","); got != "read,grep" {
		t.Fatalf("profiles = %#v", settings.Profiles)
	}
	if got := strings.Join(settings.ProtectedPaths, ","); got != ".env,~/private" {
		t.Fatalf("protected paths = %#v", settings.ProtectedPaths)
	}
	if got := strings.Join(settings.AgentModels, ","); got != "openai/gpt-5-mini" {
		t.Fatalf("agent models = %#v", settings.AgentModels)
	}
	info, err := os.Stat(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
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
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"protected_path":[".env"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home); err == nil || !strings.Contains(err.Error(), `unknown field "protected_path"`) {
		t.Fatalf("error = %v", err)
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
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetDefaultModel("deepseek/model"); err != nil {
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

func TestRecentModelsAreCaseInsensitiveUniqueAndBounded(t *testing.T) {
	models := []string{"OPENAI/Old", "deepseek/one", "openrouter/two", "openai/old"}
	for index := 0; index < 30; index++ {
		models = append(models, fmt.Sprintf("provider/model-%02d", index))
	}
	recent := recentModels(models, "openai/previous", "OPENAI/OLD")
	if len(recent) != maxRecentModels || recent[0] != "openai/previous" || recent[1] != "deepseek/one" {
		t.Fatalf("recent models = %#v", recent)
	}
	for _, model := range recent {
		if strings.EqualFold(model, "openai/old") {
			t.Fatalf("current model leaked into recent list: %#v", recent)
		}
	}
}
