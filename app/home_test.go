package app

import (
	"path/filepath"
	"testing"
)

func TestResolveHomeDefaultsToDotSkotAndHonorsExplicitValue(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)

	want := filepath.Join(userHome, ".skot")
	resolved, err := ResolveHome("")
	if err != nil || resolved != want {
		t.Fatalf("default = %q, %v; want %q", resolved, err, want)
	}

	explicit := t.TempDir()
	resolved, err = ResolveHome(explicit)
	if err != nil || resolved != explicit {
		t.Fatalf("explicit home = %q, %v; want %q", resolved, err, explicit)
	}

	workingDirectory := t.TempDir()
	t.Chdir(workingDirectory)
	want = filepath.Join(workingDirectory, "state")
	resolved, err = ResolveHome(" data/../state ")
	if err != nil || resolved != want {
		t.Fatalf("relative home = %q, %v; want %q", resolved, err, want)
	}
}
