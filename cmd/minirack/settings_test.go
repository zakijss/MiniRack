package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zakijss/minideck/ui"
)

func names(systems []System) []string {
	var n []string
	for _, x := range systems {
		n = append(n, x.Name)
	}
	return n
}

// An older settings.json, without the display settings, keeps today's
// behaviour: °C, machines as listed, none hidden.
func TestSettingsDefaults(t *testing.T) {
	dir := t.TempDir()
	old := `{"refresh_seconds": 10, "sources": [{"type": "glances", "url": "http://a:61208"}]}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.fahrenheit() || cfg.Sort != "" || cfg.HideDown {
		t.Fatalf("defaults changed: %+v", cfg)
	}
	shown, hidden := arrange(sample(), cfg)
	if hidden != 0 || !reflect.DeepEqual(names(shown), names(sample())) {
		t.Errorf("default order %v, %d hidden", names(shown), hidden)
	}
}

// The display settings are saved and read back.
func TestSettingsSaved(t *testing.T) {
	dir := t.TempDir()
	cfg := Settings{RefreshSeconds: 5, TempUnit: "F", Sort: "memory", HideDown: true,
		Sources: []SourceConf{{Type: "glances", URL: "http://a:61208"}}}
	if err := saveSettings(dir, cfg); err != nil {
		t.Fatal(err)
	}
	got, err := loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("read back %+v, want %+v", got, cfg)
	}
}

// A and right step a setting forward, left steps it back, and each change
// is saved.
func TestSettingsChange(t *testing.T) {
	scr, _ := lookScreen(t)
	dir := t.TempDir()
	cfg := Settings{RefreshSeconds: 5}
	// no sources: Add, Refresh every, Temperature, Sort machines, Down machines
	manageSources(scr, presses(
		ui.Down, ui.Right, // refresh 10 s
		ui.Down, ui.A, // °F
		ui.Down, ui.Left, ui.Left, ui.Left, // sort: Status, Load, then Memory
		ui.Down, ui.A, // hide down machines
		ui.B), dir, &cfg, nil)
	want := Settings{RefreshSeconds: 10, TempUnit: "F", Sort: "memory", HideDown: true}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("settings %+v, want %+v", cfg, want)
	}
	got, err := loadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("saved %+v, want %+v", got, want)
	}
}

func TestStepInt(t *testing.T) {
	for _, c := range []struct{ cur, step, want int }{
		{5, 1, 10}, {60, 1, 2}, {2, -1, 60}, {10, -1, 5},
		{7, 1, 10}, {7, -1, 5}, {90, 1, 2}, {1, -1, 60},
	} {
		if got := stepInt(intervals, c.cur, c.step); got != c.want {
			t.Errorf("stepInt(%d, %d) = %d, want %d", c.cur, c.step, got, c.want)
		}
	}
}

func TestSortCycles(t *testing.T) {
	var cfg Settings
	var seen []string
	for range sorts {
		change(&cfg, setSort, 1)
		seen = append(seen, sortLabel(cfg.Sort))
	}
	if want := []string{"Name", "CPU", "Memory", "Load", "Status", "As listed"}; !reflect.DeepEqual(seen, want) {
		t.Errorf("sort steps %v, want %v", seen, want)
	}
	change(&cfg, setSort, -1)
	if cfg.Sort != "status" {
		t.Errorf("left from As listed gives %q", cfg.Sort)
	}
}

func TestArrange(t *testing.T) {
	login := System{Name: "Vault", Source: "beszel", Login: true, Fail: true}
	all := append(sample(), login)
	for _, c := range []struct {
		cfg    Settings
		want   []string
		hidden int
	}{
		{Settings{Sort: "name"}, []string{"backup-box", "homeserver", "laptop", "media-server-02", "nas", "Vault"}, 0},
		{Settings{Sort: "cpu"}, []string{"media-server-02", "homeserver", "nas", "backup-box", "laptop", "Vault"}, 0},
		{Settings{Sort: "memory"}, []string{"media-server-02", "homeserver", "backup-box", "nas", "laptop", "Vault"}, 0},
		{Settings{Sort: "load"}, []string{"media-server-02", "homeserver", "backup-box", "nas", "laptop", "Vault"}, 0},
		{Settings{Sort: "status"}, []string{"Vault", "laptop", "backup-box", "homeserver", "media-server-02", "nas"}, 0},
		// a refused login stays: it needs fixing in Settings
		{Settings{HideDown: true}, []string{"homeserver", "media-server-02", "backup-box", "nas", "Vault"}, 1},
		{Settings{Sort: "cpu", HideDown: true}, []string{"media-server-02", "homeserver", "nas", "backup-box", "Vault"}, 1},
	} {
		shown, hidden := arrange(all, c.cfg)
		if !reflect.DeepEqual(names(shown), c.want) || hidden != c.hidden {
			t.Errorf("%+v: %v, %d hidden; want %v, %d", c.cfg, names(shown), hidden, c.want, c.hidden)
		}
	}
	if all[0].Name != "homeserver" {
		t.Error("arrange reordered its input")
	}
}

func TestDegrees(t *testing.T) {
	for _, c := range []struct {
		c    float64
		f    bool
		sep  string
		want string
	}{
		{47, false, "", "47°C"}, {47, true, "", "117°F"}, {0, true, " ", "32 °F"}, {100, true, " ", "212 °F"}, {78, false, " ", "78 °C"},
	} {
		if got := degrees(c.c, c.f, c.sep); got != c.want {
			t.Errorf("degrees(%v, %v) = %q, want %q", c.c, c.f, got, c.want)
		}
	}
	x := sample()[0]
	if line := cardStats(x, true); line != "LOAD 0.31 · 117°F · UP 12D 3H" {
		t.Errorf("card line in °F: %q", line)
	}
}

// The selected machine stays selected when a refresh reorders the cards.
func TestSelectionFollowsMachine(t *testing.T) {
	before, _ := arrange(sample(), Settings{})
	key := machineKey(before[1]) // media-server-02
	after, _ := arrange(sample(), Settings{Sort: "name"})
	if i := find(after, key, 1); after[i].Name != "media-server-02" {
		t.Errorf("selection moved to %s", after[i].Name)
	}
	if i := find(after, "gone/box", 2); i != 2 {
		t.Errorf("a machine no longer listed moved the selection to %d", i)
	}
}

// A hot or overloaded machine shows its temperature and load in orange or
// red; load counts only when the thread count is known.
func TestHeatColours(t *testing.T) {
	for _, c := range []struct {
		temp float64
		want any
	}{{47, ui.Text}, {75, ui.Warn}, {89, ui.Warn}, {90, ui.Bad}} {
		if got := tempColor(c.temp); got != c.want {
			t.Errorf("tempColor(%v) = %v", c.temp, got)
		}
	}
	for _, c := range []struct {
		x    System
		want any
	}{
		{System{Load: []float64{13}}, ui.Text}, // threads unknown
		{System{Load: []float64{3}, Cores: 2, Threads: 4}, ui.Text},
		{System{Load: []float64{4}, Cores: 2, Threads: 4}, ui.Warn},
		{System{Load: []float64{8}, Threads: 4}, ui.Bad},
	} {
		if got := loadColor(c.x); got != c.want {
			t.Errorf("loadColor(%+v) = %v", c.x, got)
		}
	}
	if got := statsColor(sample()[0]); got != ui.Muted {
		t.Errorf("a quiet machine's line is %v", got)
	}
	if got := statsColor(System{Temp: 92, Load: []float64{1}, Threads: 4}); got != ui.Bad {
		t.Errorf("a hot machine's line is %v", got)
	}
}
