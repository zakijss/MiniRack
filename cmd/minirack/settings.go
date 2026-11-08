package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zakijss/minideck/atomicfile"
	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/ui"
)

var types = []struct{ label, typ, port, about string }{
	{"Glances", "glances", "61208", "One machine running glances -w"},
	{"Beszel", "beszel", "8090", "Every machine your Beszel hub watches"},
	{"Vantage", "vantage", "8106", "A Vantage console and its peers"},
}

func typeInfo(label string) (typ, port string) {
	for _, t := range types {
		if t.label == label || t.typ == label {
			return t.typ, t.port
		}
	}
	return "glances", "61208"
}

func typeLabel(typ string) string {
	for _, t := range types {
		if t.typ == typ {
			return t.label
		}
	}
	return typ
}

func saveSettings(dir string, cfg Settings) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, "settings.json"), append(b, '\n'), 0644)
}

// newSecretKey names the secrets.enc entry for a new Beszel source's
// password. It must differ from every key in sec and every key a source
// names: after secrets.enc is reset (a new device key), sources still name
// keys that sec no longer has, and reusing one would overwrite that
// source's password when it is entered again.
func newSecretKey(sec map[string]string, sources []SourceConf) string {
	used := map[string]bool{}
	for k := range sec {
		used[k] = true
	}
	for _, c := range sources {
		used[c.Secret] = true
	}
	for n := 1; ; n++ {
		if k := fmt.Sprintf("beszel-%d", n); !used[k] {
			return k
		}
	}
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

func welcome(scr *ui.Screen, input <-chan ui.Event) bool {
	return scr.Confirm(input, "Welcome to MiniRack",
		"See your servers' CPU, memory, disks and temperatures. Add where the numbers come from: Glances, a Beszel hub or Vantage.",
		"Add a source")
}

// editSource adds (i < 0) or edits source i, saving on success.
func editSource(scr *ui.Screen, input <-chan ui.Event, dir string, cfg *Settings, sec map[string]string, i int) bool {
	var c SourceConf
	if i >= 0 {
		c = cfg.Sources[i]
	} else {
		c.Type = "glances"
	}
	kind := &ui.Field{Label: "Type", Value: typeLabel(c.Type), Options: []string{"Glances", "Beszel", "Vantage"}}
	name := &ui.Field{Label: "Name", Value: c.Name, Optional: true, Hint: "Empty: use the server's own name"}
	server := &ui.Field{Label: "Server", Value: c.URL}
	user := &ui.Field{Label: "Beszel login", Value: c.User, Placeholder: "email",
		Hint: "A read-only Beszel user is enough."}
	pass := &ui.Field{Label: "Password", Secret: true, Hint: "Stored encrypted on this device"}
	if c.Secret != "" && sec[c.Secret] != "" {
		pass.Placeholder = "(saved)"
	}
	isBeszel := func() bool { t, _ := typeInfo(kind.Value); return t == "beszel" }
	user.Hidden = func() bool { return !isBeszel() }
	pass.Hidden = user.Hidden
	refresh := func() {
		_, port := typeInfo(kind.Value)
		for _, t := range types {
			if t.label == kind.Value {
				kind.Hint = t.about
			}
		}
		server.Placeholder = "http://homeserver:" + port
		server.Hint = "Its address, e.g. homeserver:" + port
		server.Pick = func() (string, bool) { return scr.PickHost(input, tnet.HostChoices, port) }
	}
	refresh()
	kind.Changed = refresh

	build := func() (SourceConf, string) {
		t, port := typeInfo(kind.Value)
		n := SourceConf{Type: t, Name: strings.TrimSpace(name.Value), URL: tnet.NormalizeURL(server.Value, port)}
		pw := ""
		if t == "beszel" {
			n.User = strings.TrimSpace(user.Value)
			n.Secret = c.Secret
			if n.Secret == "" {
				n.Secret = newSecretKey(sec, cfg.Sources)
			}
			pw = pass.Value
			if pw == "" {
				pw = sec[n.Secret]
			}
		}
		return n, pw
	}

	title := "Add a source"
	if i >= 0 {
		title = "Edit source"
	}
	f := &ui.Form{Title: title, Fields: []*ui.Field{kind, name, server, user, pass}}
	f.Check = func() error {
		n, pw := build()
		var fe fetcher
		switch n.Type {
		case "glances":
			fe = &glances{c: n}
		case "beszel":
			if n.User == "" || pw == "" {
				return loginErr("Enter the Beszel login and password.")
			}
			fe = &beszel{c: n, password: pw}
		default:
			fe = &vantage{c: n}
		}
		for _, sys := range fe.Fetch() {
			if sys.Fail {
				return errors.New(sys.Err)
			}
		}
		return nil
	}
	f.Commit = func() error {
		n, _ := build()
		if n.Type == "beszel" && pass.Value != "" {
			sec[n.Secret] = pass.Value
			if err := secrets.Save(filepath.Join(dir, "secrets.enc"), sec); err != nil {
				return err
			}
		}
		if i >= 0 {
			cfg.Sources[i] = n
		} else {
			cfg.Sources = append(cfg.Sources, n)
		}
		return saveSettings(dir, *cfg)
	}
	if i >= 0 {
		f.Delete = func() {
			if s := cfg.Sources[i].Secret; s != "" {
				delete(sec, s)
				if err := secrets.Save(filepath.Join(dir, "secrets.enc"), sec); err != nil {
					log.Printf("secrets: %v", err)
				}
			}
			cfg.Sources = append(cfg.Sources[:i], cfg.Sources[i+1:]...)
			if err := saveSettings(dir, *cfg); err != nil {
				scr.Notice(input, "Couldn't save", err.Error()+". Check that the card has free space.")
			}
		}
	}
	return f.Run(scr, input)
}

// setting is a settings row that left and right change in place.
type setting int

const (
	setRefresh setting = iota
	setTemp
	setSort
	setDown
)

// action is a settings row that A opens.
type action int

const (
	actAdd action = iota
	actAppearance
	actDone
)

var (
	intervals = []int{2, 5, 10, 30, 60}
	sorts     = []struct{ id, label string }{
		{"", "As listed"}, {"name", "Name"}, {"cpu", "CPU"}, {"memory", "Memory"}, {"status", "Status"},
	}
)

func sortLabel(id string) string {
	for _, o := range sorts {
		if o.id == id {
			return o.label
		}
	}
	return sorts[0].label
}

// settingsItems is the settings list: the sources, then adding one, the
// refresh interval, how the machines are shown, Appearance and Done. A
// source row's Value is its index.
func settingsItems(cfg Settings) []ui.Item {
	var items []ui.Item
	for i, c := range cfg.Sources {
		label := typeLabel(c.Type)
		if c.Name != "" {
			label += ": " + c.Name
		}
		items = append(items, ui.Item{Label: label, Detail: hostOf(c.URL), Value: i})
	}
	temp, down := "°C", "Shown"
	if cfg.fahrenheit() {
		temp = "°F"
	}
	if cfg.HideDown {
		down = "Hidden"
	}
	appearance := ui.AppearanceItem()
	appearance.Value = actAppearance
	return append(items,
		ui.Item{Label: "+ Add a source", Value: actAdd},
		ui.Item{Label: "Refresh every", Detail: fmt.Sprintf("%d s", cfg.RefreshSeconds), Value: setRefresh},
		ui.Item{Label: "Temperature", Detail: temp, Value: setTemp},
		ui.Item{Label: "Sort machines", Detail: sortLabel(cfg.Sort), Value: setSort},
		ui.Item{Label: "Down machines", Detail: down, Value: setDown},
		appearance,
		ui.Item{Label: "Done", Value: actDone})
}

// stepInt moves cur step places along list, wrapping round. A value not in
// list goes to the next larger one (or the next smaller one going back).
func stepInt(list []int, cur, step int) int {
	for i, v := range list {
		if v == cur {
			return list[((i+step)%len(list)+len(list))%len(list)]
		}
	}
	if step > 0 {
		for _, v := range list {
			if v > cur {
				return v
			}
		}
		return list[0]
	}
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] < cur {
			return list[i]
		}
	}
	return list[len(list)-1]
}

// change steps setting r forward (step 1) or back (-1).
func change(cfg *Settings, r setting, step int) {
	switch r {
	case setRefresh:
		cfg.RefreshSeconds = stepInt(intervals, cfg.RefreshSeconds, step)
	case setTemp:
		if cfg.fahrenheit() {
			cfg.TempUnit = ""
		} else {
			cfg.TempUnit = "F"
		}
	case setSort:
		i := 0
		for j, o := range sorts {
			if o.id == cfg.Sort {
				i = j
			}
		}
		cfg.Sort = sorts[((i+step)%len(sorts)+len(sorts))%len(sorts)].id
	case setDown:
		cfg.HideDown = !cfg.HideDown
	}
}

// arrange puts the machines in the order Settings asks for and leaves out
// the ones that are down when it says to hide them; hidden is how many
// were left out. A refused login is never hidden: it needs fixing in
// Settings.
func arrange(all []System, cfg Settings) (shown []System, hidden int) {
	shown = make([]System, 0, len(all))
	for _, x := range all {
		if cfg.HideDown && !x.Up && !x.Login {
			hidden++
			continue
		}
		shown = append(shown, x)
	}
	// value is what the CPU and memory orders compare; machines that are
	// down have no reading and go last.
	value := func(x System) float64 {
		if !x.Up {
			return -1
		}
		if cfg.Sort == "memory" {
			return x.Mem
		}
		return x.CPU
	}
	switch cfg.Sort {
	case "name":
		sort.SliceStable(shown, func(i, j int) bool {
			return strings.ToLower(shown[i].Name) < strings.ToLower(shown[j].Name)
		})
	case "cpu", "memory":
		sort.SliceStable(shown, func(i, j int) bool { return value(shown[i]) > value(shown[j]) })
	case "status":
		sort.SliceStable(shown, func(i, j int) bool { return statusRank(shown[i]) < statusRank(shown[j]) })
	}
	return shown, hidden
}

// statusRank orders machines for the Status sort: the ones that need
// attention first, then the ones showing an old reading, then the rest.
func statusRank(x System) int {
	switch {
	case x.Login:
		return 0
	case !x.Up:
		return 1
	case !x.Stale.IsZero():
		return 2
	}
	return 3
}

// degrees formats a temperature read in °C, in °F when f is set, with sep
// between the number and the unit ("47°C" on a card, "47 °C" on the
// detail page).
func degrees(c float64, f bool, sep string) string {
	unit := "°C"
	if f {
		c, unit = c*9/5+32, "°F"
	}
	return fmt.Sprintf("%.0f%s%s", c, sep, unit)
}

// manageSources lists the sources to add, edit or remove, the display
// settings and Appearance. A opens a row; on a setting, A and right step
// it forward and left steps it back.
func manageSources(scr *ui.Screen, input <-chan ui.Event, dir string, cfg *Settings, sec map[string]string) {
	l := ui.List{}
	for {
		l.Items = settingsItems(*cfg)
		l.Sel = min(l.Sel, len(l.Items)-1)
		top := scr.StaticFrame("MiniRack settings", "Left and right change a setting", ui.Hint{Button: "A", Label: "Choose"}, ui.Hint{Button: "B", Label: "Back"})
		l.Draw(scr, image.Rect(20, top, ui.W-26, ui.H-58))
		scr.Present()
		ev, ok, frame := scr.Next(input)
		if !ok {
			return
		}
		if frame || !ev.Pressed {
			continue
		}
		it := l.Selected()
		switch ev.Button {
		case ui.B, ui.Menu:
			return
		case ui.A, ui.Left, ui.Right:
			if it == nil {
				continue
			}
			if r, ok := it.Value.(setting); ok {
				step := 1
				if ev.Button == ui.Left {
					step = -1
				}
				old := *cfg
				change(cfg, r, step)
				if err := saveSettings(dir, *cfg); err != nil {
					*cfg = old
					scr.Notice(input, "Couldn't save", err.Error()+". Check that the card has free space.")
				}
				continue
			}
			if ev.Button != ui.A {
				l.Handle(ev)
				continue
			}
			switch v := it.Value.(type) {
			case int:
				editSource(scr, input, dir, cfg, sec, v)
			case action:
				switch v {
				case actAdd:
					editSource(scr, input, dir, cfg, sec, -1)
				case actAppearance:
					scr.Appearance(input)
				case actDone:
					return
				}
			}
		default:
			l.Handle(ev)
		}
	}
}
