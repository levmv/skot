package state

import (
	"bytes"
	"encoding/json/v2"
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

func TestStoreRepairsSharedConfigWithoutChangingHomePermissions(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, %v", info, err)
	}
	info, err = os.Stat(home)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("home mode = %v, %v", info, err)
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
			if _, err := Open(home); err == nil || !strings.Contains(err.Error(), `unknown object member name "`+field+`"`) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestStoreRejectsSymlinkedCredentials(t *testing.T) {
	for _, name := range []string{"store.bin", "auth.json"} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			outsideHome := t.TempDir()
			outside := filepath.Join(outsideHome, name)
			if name == "auth.json" {
				if err := os.WriteFile(outside, []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				source, err := OpenCredentials(outsideHome)
				if err != nil {
					t.Fatal(err)
				}
				if err := source.SetAPIKey(t.Context(), "example", "outside-key"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(home, name)); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if _, err := Open(home); err == nil {
				t.Fatal("symlinked credentials were accepted")
			}
		})
	}
}

func TestStoreKeepsCredentialsOpaqueAndPortable(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"protected_paths":[".env"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAPIKey(t.Context(), " DeepSeek ", " secret-token "); err != nil {
		t.Fatal(err)
	}
	const payload = `{"access_token":"access-fixture","refresh_token":"refresh-fixture"}`
	if err := store.UpdateCredential(t.Context(), "subscription", func(CredentialProfile) (CredentialProfile, error) {
		return CredentialProfile{Provider: "subscription", Kind: "oauth", Payload: []byte(payload)}, nil
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(home, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"secret-token", "access-fixture", "refresh-fixture"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("plaintext credential in %s", entry.Name())
			}
		}
	}
	path := filepath.Join(home, "store.bin")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %v, %v", info, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	copyHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(copyHome, "store.bin"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenCredentials(copyHome)
	if err != nil {
		t.Fatal(err)
	}
	if token, ok, err := restored.APIKey("deepseek"); err != nil || !ok || token != "secret-token" {
		t.Fatalf("restored API key = %q, %v, %v", token, ok, err)
	}
	profile, err := restored.Credential("subscription")
	if err != nil || profile.Kind != "oauth" {
		t.Fatalf("restored OAuth profile = %+v, %v", profile, err)
	}
	var tokens map[string]string
	if err := json.Unmarshal(profile.Payload, &tokens); err != nil || tokens["access_token"] != "access-fixture" || tokens["refresh_token"] != "refresh-fixture" {
		t.Fatalf("restored OAuth tokens = %v, %v", tokens, err)
	}
	if err := store.DeleteAPIKey(t.Context(), "deepseek"); err != nil {
		t.Fatal(err)
	}
	if token, ok, err := store.APIKey("deepseek"); err != nil || ok || token != "" {
		t.Fatalf("deleted API key = %q, %v, %v", token, ok, err)
	}
}
