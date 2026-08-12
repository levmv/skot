package ui

import (
	"bytes"
	"context"
	"image/color"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestNormalizeTheme(t *testing.T) {
	for input, want := range map[string]string{"": ThemeAuto, " AUTO ": ThemeAuto, "light": ThemeLight, "DARK": ThemeDark} {
		if got, err := NormalizeTheme(input); err != nil || got != want {
			t.Fatalf("NormalizeTheme(%q) = %q, %v", input, got, err)
		}
	}
	if _, err := NormalizeTheme("sepia"); err == nil {
		t.Fatal("invalid theme accepted")
	}
}

func TestDarkThemeUsesBrightAccent(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model, err := newScreenModel(context.Background(), &fakeAgent{}, Config{Theme: ThemeDark}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !model.darkTheme || model.themePending {
		t.Fatalf("dark theme state: dark=%v pending=%v", model.darkTheme, model.themePending)
	}
	if model.accentStyle.GetForeground() == nil || !model.accentStyle.GetBold() {
		t.Fatalf("accent style is incomplete: %#v", model.accentStyle)
	}
}

func TestAutoThemeWaitsForBackgroundOrTimeout(t *testing.T) {
	var output bytes.Buffer
	model, err := newScreenModel(context.Background(), &fakeAgent{}, Config{Theme: ThemeAuto}, &output)
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	if !model.themePending || model.Init() == nil {
		t.Fatalf("auto theme state: pending=%v init=%v", model.themePending, model.Init())
	}
	updated, _ := model.Update(tea.BackgroundColorMsg{Color: color.White})
	detected := updated.(screenModel)
	if detected.themePending || detected.darkTheme {
		t.Fatalf("detected light state: pending=%v dark=%v", detected.themePending, detected.darkTheme)
	}

	model, err = newScreenModel(context.Background(), &fakeAgent{}, Config{Theme: ThemeAuto}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	updated, _ = model.Update(themeQueryTimeoutMsg{})
	fallback := updated.(screenModel)
	if fallback.themePending || fallback.darkTheme {
		t.Fatalf("fallback state: pending=%v dark=%v", fallback.themePending, fallback.darkTheme)
	}
}
