package ui

import (
	"bytes"
	"context"
	"errors"
	"image/color"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func TestDarkThemeUsesBrightAccent(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model, err := newScreenModel(context.Background(), &fakeAgent{theme: ThemeDark}, Config{}, &bytes.Buffer{})
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

func TestLightThemeUsesNeutralUserGutter(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model, err := newScreenModel(context.Background(), &fakeAgent{theme: ThemeLight}, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	// The user bar is a flat rule that sits below muted text: no fill, no bold.
	if got := model.userBarStyle.GetForeground(); got != lipgloss.Color("250") {
		t.Fatalf("user bar foreground = %#v", got)
	}
	if model.userBarStyle.GetBackground() != lipgloss.NewStyle().GetBackground() || model.userBarStyle.GetBold() {
		t.Fatalf("user bar is not flat: %#v", model.userBarStyle)
	}
}

func TestThemeAppliesToMarkdownPalette(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("SK_COLOR", "always")
	model, err := newScreenModel(context.Background(), &fakeAgent{theme: ThemeDark}, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	text := "## Title\n`inline`\n```\ncode block\n```"
	dark := strings.Join(model.markdown.renderLinesAtWidth(text, 80), "\n")

	model.applyTerminalTheme(false)
	light := strings.Join(model.markdown.renderLinesAtWidth(text, 80), "\n")
	if dark == light {
		t.Fatalf("markdown palette did not change with theme: %q", dark)
	}
}

func TestAutoThemeWaitsForBackgroundOrTimeout(t *testing.T) {
	var output bytes.Buffer
	model, err := newScreenModel(context.Background(), &fakeAgent{theme: ThemeAuto}, Config{}, &output)
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

	model, err = newScreenModel(context.Background(), &fakeAgent{theme: ThemeAuto}, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	updated, _ = model.Update(themeQueryTimeoutMsg{})
	fallback := updated.(screenModel)
	if fallback.themePending || !fallback.darkTheme {
		t.Fatalf("fallback state: pending=%v dark=%v", fallback.themePending, fallback.darkTheme)
	}
}

func TestThemeCommandShowsAndSwitchesTheme(t *testing.T) {
	fake := &fakeAgent{theme: ThemeDark}
	model, err := newScreenModel(context.Background(), fake, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	model.composer.setValue("/theme")
	model, command := model.submitInput()
	if command != nil || model.picker.kind != pickerTheme || len(model.picker.items) != 3 || model.picker.index != 2 || !model.picker.items[2].current {
		t.Fatalf("theme picker = %#v, command=%v", model.picker, command)
	}
	if !model.picker.numberSelectionEnabled() {
		t.Fatalf("theme picker without digit shortcuts = %#v", model.picker)
	}

	model, command = model.handleKey(tea.KeyPressMsg{Text: "2", Code: '2', BaseCode: '2'})
	if command != nil || model.picker.active() || model.theme != ThemeLight || fake.theme != ThemeLight || model.darkTheme || model.themePending {
		t.Fatalf("light selection: picker=%#v theme=%q stored=%q dark=%v pending=%v command=%v", model.picker, model.theme, fake.theme, model.darkTheme, model.themePending, command)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "theme: dark → light" {
		t.Fatalf("theme status = %q", got)
	}
}

func TestThemeCommandAutoRedetectsWithDarkFallback(t *testing.T) {
	fake := &fakeAgent{theme: ThemeLight}
	model, err := newScreenModel(context.Background(), fake, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.resize(80, 24)
	model.composer.setValue("/theme auto")
	model, command := model.submitInput()
	// Auto applies the dark fallback up front, before OSC 11 has a chance to answer.
	if command == nil || model.theme != ThemeAuto || fake.theme != ThemeAuto || !model.themePending || !model.darkTheme {
		t.Fatalf("auto selection: theme=%q stored=%q pending=%v dark=%v command=%v", model.theme, fake.theme, model.themePending, model.darkTheme, command)
	}

	stale := model.themeQuery - 1
	updated, _ := model.Update(themeQueryTimeoutMsg{generation: stale})
	model = updated.(screenModel)
	if !model.themePending {
		t.Fatal("stale theme timeout completed the current detection")
	}
	updated, _ = model.Update(themeQueryTimeoutMsg{generation: model.themeQuery})
	fallback := updated.(screenModel)
	if fallback.themePending || !fallback.darkTheme {
		t.Fatalf("auto fallback: pending=%v dark=%v", fallback.themePending, fallback.darkTheme)
	}

	fallback.composer.setValue("/theme")
	fallback, _ = fallback.submitInput()
	fallback, command = fallback.handleKey(tea.KeyPressMsg{Text: "1", Code: '1', BaseCode: '1'})
	if command == nil || !fallback.themePending {
		t.Fatalf("reselect auto: pending=%v command=%v", fallback.themePending, command)
	}
}

func TestThemeCommandRejectsInvalidTheme(t *testing.T) {
	fake := &fakeAgent{theme: ThemeLight}
	model, err := newScreenModel(context.Background(), fake, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.composer.setValue("/theme sepia")
	model, command := model.submitInput()
	if command != nil || model.theme != ThemeLight || fake.theme != ThemeLight || model.composer.value() != "/theme sepia" {
		t.Fatalf("invalid theme: theme=%q stored=%q input=%q command=%v", model.theme, fake.theme, model.composer.value(), command)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != `theme: invalid terminal theme "sepia"; expected auto, light, or dark` {
		t.Fatalf("theme error = %q", got)
	}
}

func TestThemeCommandKeepsCurrentThemeWhenSavingFails(t *testing.T) {
	fake := &fakeAgent{theme: ThemeDark, themeErr: errors.New("disk full")}
	model, err := newScreenModel(context.Background(), fake, Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	model.composer.setValue("/theme light")
	model, command := model.submitInput()
	if command != nil || model.theme != ThemeDark || fake.theme != ThemeDark || !model.darkTheme || model.composer.value() != "/theme light" {
		t.Fatalf("failed save: theme=%q stored=%q dark=%v input=%q command=%v", model.theme, fake.theme, model.darkTheme, model.composer.value(), command)
	}
	if got := model.transcript.blocks[len(model.transcript.blocks)-1].text; got != "theme: disk full" {
		t.Fatalf("theme error = %q", got)
	}
}
