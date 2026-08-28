package app

import (
	"context"
	"strings"
	"testing"

	"github.com/levmv/skot/internal/state"
)

func TestApplicationSwitchesAndSharesDisplayProfile(t *testing.T) {
	home, firstRoot, secondRoot := t.TempDir(), t.TempDir(), t.TempDir()
	first, err := Open(context.Background(), Config{Home: home, Root: firstRoot, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.CurrentDisplayProfile() != state.DisplayCompact {
		t.Fatalf("default display = %q", first.CurrentDisplayProfile())
	}
	if err := first.SwitchDisplayProfile(" DETAILED "); err != nil {
		t.Fatal(err)
	}
	if err := first.SwitchDisplayProfile("verbose"); err == nil || !strings.Contains(err.Error(), "invalid display profile") {
		t.Fatalf("invalid switch error = %v", err)
	}
	if first.CurrentDisplayProfile() != state.DisplayDetailed {
		t.Fatalf("display after invalid switch = %q", first.CurrentDisplayProfile())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(context.Background(), Config{Home: home, Root: secondRoot, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.CurrentDisplayProfile() != state.DisplayDetailed {
		t.Fatalf("shared display = %q", second.CurrentDisplayProfile())
	}
}
