package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInteractiveStorePersistsSharedDisplayProfile(t *testing.T) {
	home, firstRoot, secondRoot := t.TempDir(), t.TempDir(), t.TempDir()
	if _, err := Open(home); err != nil {
		t.Fatal(err)
	}
	first, err := OpenInteractive(home, firstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetDisplaySelection(" COMPACT "); err != nil {
		t.Fatal(err)
	}
	second, err := OpenInteractive(home, secondRoot)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := second.Settings()
	if err != nil || settings.Display != DisplayCompact {
		t.Fatalf("display = %q, err = %v", settings.Display, err)
	}
}

func TestInvalidStoredDisplayProfileIsIgnored(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	path := filepath.Join(home, "interactive.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"ui":{"display":"everything"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenInteractive(home, root)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Display != "" || len(settings.Notices) != 1 || !strings.Contains(settings.Notices[0], "invalid display profile") {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestNormalizeDisplayProfile(t *testing.T) {
	for input, want := range map[string]string{
		"": DisplayCompact, " DETAILED ": DisplayDetailed, "compact": DisplayCompact, "FULL": DisplayFull,
	} {
		if got, err := NormalizeDisplayProfile(input); err != nil || got != want {
			t.Fatalf("NormalizeDisplayProfile(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"verbose", "minimal", "normal"} {
		if _, err := NormalizeDisplayProfile(input); err == nil {
			t.Fatalf("invalid display profile %q accepted", input)
		}
	}
}
