package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zakijss/minideck/ui"
)

// presses is input that presses each button in turn, then ends.
func presses(bs ...ui.Button) <-chan ui.Event {
	ch := make(chan ui.Event, len(bs))
	for _, b := range bs {
		ch <- ui.Event{Button: b, Pressed: true}
	}
	close(ch)
	return ch
}

// lookScreen opens a screen that draws nowhere, with the shared look saved
// to a temporary file. The look in use is put back after the test.
func lookScreen(t *testing.T) (*ui.Screen, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "appearance.json")
	t.Setenv("MINIDECK_UI_NULL", "1")
	t.Setenv("MINIDECK_APPEARANCE", path)
	scr, err := ui.Open()
	if err != nil {
		t.Fatal(err)
	}
	start := ui.CurrentAppearance()
	t.Cleanup(func() { scr.SetAppearance(start) })
	return scr, path
}

// wantLookChanged checks that Appearance was opened: RIGHT on its first
// row moved to the next theme, and B saved it.
func wantLookChanged(t *testing.T, start ui.Appearance, path string) {
	t.Helper()
	if ui.CurrentAppearance().Theme == start.Theme {
		t.Errorf("theme still %q: Appearance didn't open", start.Theme)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("look not saved: %v", err)
	}
}

func TestSettingsHaveAppearance(t *testing.T) {
	items := settingsItems(Settings{Sources: []SourceConf{{Type: "glances", URL: "http://a:61208"}}, RefreshSeconds: 5})
	if n := len(items); n < 2 || items[n-2].Label != "Appearance" || items[n-1].Label != "Done" {
		t.Fatalf("settings end with %+v", items[len(items)-2:])
	}
}

func TestSettingsOpenAppearance(t *testing.T) {
	scr, path := lookScreen(t)
	start := ui.CurrentAppearance()
	cfg := Settings{RefreshSeconds: 5}
	// no sources: Add, Refresh every, Temperature, Sort machines, Down
	// machines, Appearance, Done
	manageSources(scr, presses(ui.Down, ui.Down, ui.Down, ui.Down, ui.Down, ui.A, ui.Right, ui.B, ui.B), t.TempDir(), &cfg, map[string]string{})
	wantLookChanged(t, start, path)
}

func TestSelectOpensAppearance(t *testing.T) {
	scr, path := lookScreen(t)
	start := ui.CurrentAppearance()
	if dashboard(scr, presses(ui.Select, ui.Right, ui.B, ui.B), Settings{RefreshSeconds: 5}, nil) {
		t.Error("B on the dashboard should exit, not open settings")
	}
	wantLookChanged(t, start, path)
}
