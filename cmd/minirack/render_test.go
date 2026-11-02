package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zakijss/minideck/ui"
)

// sample is a dashboard with names and readings that are hard to fit: a
// ten-letter host name, a fifteen-letter one, a long uptime and a
// temperature, and one machine not answering (its tag takes room).
func sample() []System {
	return []System{
		{Name: "homeserver", Source: "glances", Up: true, CPU: 7.5, Mem: 38, Disk: 72,
			Load: []float64{0.31, 0.2, 0.1}, Temp: 47, Uptime: "12d 3h"},
		{Name: "media-server-02", Source: "beszel", Up: true, CPU: 92, Mem: 81, Disk: 55,
			Load: []float64{13.08, 2.9, 2.2}, Temp: 78, Uptime: "103d 22h"},
		{Name: "backup-box", Source: "glances", Up: true, CPU: 1, Mem: 20, Disk: 91,
			Load: []float64{0.05}, Uptime: "2h 05m", Stale: time.Now().Add(-time.Minute)},
		{Name: "laptop", Source: "beszel", Err: "The hub reports this system as down."},
		{Name: "nas", Source: "vantage", Up: true, CPU: 3, Mem: 12, Disk: -1, Uptime: "5h"},
	}
}

// TestCardsFit checks that the card text fits without "…" for the sample:
// the pixel font is wide, and a cut-off host name is the first thing
// people notice.
func TestCardsFit(t *testing.T) {
	for _, x := range sample() {
		if !x.Up {
			continue
		}
		if name, _, _ := cardName(x); strings.HasSuffix(name, "…") {
			t.Errorf("name cut: %q", name)
		}
		for _, f := range []bool{false, true} {
			if line := cardStats(x, f); strings.HasSuffix(line, "…") {
				t.Errorf("%s: stats line cut: %q", x.Name, line)
			}
		}
	}
}

// TestScreens draws the overview, a detail page and the settings to PNG
// files in DIR for review. It runs only with MINIDECK_GALLERY=DIR set.
func TestScreens(t *testing.T) {
	dir := os.Getenv("MINIDECK_GALLERY")
	if dir == "" {
		t.Skip("set MINIDECK_GALLERY=DIR to draw the screens")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, "minirack-overview.png"))
	s, err := ui.Open()
	if err != nil {
		t.Fatal(err)
	}
	systems := sample()
	drawOverview(s, systems, 1, time.Now(), false, 0, false)
	s.Present()
	s.Close()

	t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, "minirack-detail.png"))
	if s, err = ui.Open(); err != nil {
		t.Fatal(err)
	}
	scroll := 0
	drawDetail(s, systems[0], []float64{5, 9, 12, 7, 30, 22, 8, 6}, time.Hour, &scroll, false)
	s.Present()
	s.Close()

	t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, "minirack-settings.png"))
	if s, err = ui.Open(); err != nil {
		t.Fatal(err)
	}
	closed := make(chan ui.Event)
	close(closed) // the settings draw once and return
	manageSources(s, closed, t.TempDir(), &Settings{RefreshSeconds: 5, Sources: []SourceConf{
		{Type: "glances", URL: "http://homeserver:61208"}, {Type: "beszel", Name: "hub", URL: "http://hub:8090"}}}, nil)
	s.Close()

	// Lower down: the display settings, changed from the defaults.
	t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, "minirack-settings-display.png"))
	if s, err = ui.Open(); err != nil {
		t.Fatal(err)
	}
	manageSources(s, presses(ui.Down, ui.Down, ui.Down, ui.Down, ui.Down, ui.Down), t.TempDir(), &Settings{RefreshSeconds: 5,
		TempUnit: "F", Sort: "status", HideDown: true, Sources: []SourceConf{
			{Type: "glances", URL: "http://homeserver:61208"}, {Type: "beszel", Name: "hub", URL: "http://hub:8090"}}}, nil)
	s.Close()

	// The overview with machines that are down hidden, by CPU, in °F.
	t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, "minirack-overview-sorted.png"))
	if s, err = ui.Open(); err != nil {
		t.Fatal(err)
	}
	shown, hidden := arrange(systems, Settings{Sort: "cpu", HideDown: true})
	drawOverview(s, shown, 0, time.Now(), false, hidden, true)
	s.Present()
	s.Close()

	// Every machine down and hidden.
	t.Setenv("MINIDECK_UI_PNG", filepath.Join(dir, "minirack-all-hidden.png"))
	if s, err = ui.Open(); err != nil {
		t.Fatal(err)
	}
	drawOverview(s, nil, 0, time.Now(), false, 3, false)
	s.Present()
	s.Close()
}
