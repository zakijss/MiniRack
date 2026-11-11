// MiniRack shows live stats for the machines on your tailnet, read from
// Glances, a Beszel hub and Vantage.
package main

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"

	"github.com/zakijss/minideck/appdir"
	"github.com/zakijss/minideck/secrets"
	"github.com/zakijss/minideck/tnet"
	"github.com/zakijss/minideck/ui"
	"github.com/zakijss/minideck/version"
)

// Settings is settings.json: the sources to read, how often, and how the
// machines are shown.
type Settings struct {
	RefreshSeconds int          `json:"refresh_seconds"`
	Sources        []SourceConf `json:"sources"`
	// TempUnit is "C" (the default) or "F".
	TempUnit string `json:"temp_unit,omitempty"`
	// Sort orders the machines: "" (as the sources list them), "name",
	// "cpu", "memory", "load" or "status".
	Sort string `json:"sort,omitempty"`
	// HideDown leaves out machines that don't answer (default off).
	HideDown bool `json:"hide_down,omitempty"`
}

// fahrenheit reports whether temperatures are shown in °F.
func (s Settings) fahrenheit() bool { return s.TempUnit == "F" }

type state struct {
	mu      sync.Mutex
	systems []System // as shown: sorted, without hidden machines
	hidden  int      // machines left out (Settings › Down machines)
	updated time.Time
	busy    bool
	history map[string][]float64 // CPU % per machine, oldest first

	// the last good reading of each source, shown (marked stale) when a
	// refresh fails for a moment: one timeout shouldn't blank a card
	good   map[int][]System
	goodAt map[int]time.Time
}

// failed reports whether a source returned only errors (but not a refused
// login, which won't fix itself).
func failed(r []System) bool {
	for _, x := range r {
		if !x.Fail || x.Login {
			return false
		}
	}
	return len(r) > 0
}

func loadSettings(dir string) (Settings, error) {
	s := Settings{RefreshSeconds: 5}
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("settings.json: %w", err)
	}
	if s.RefreshSeconds < 2 {
		s.RefreshSeconds = 2
	}
	return s, nil
}

func main() {
	version.Flag("minirack")
	version.Note("minirack")
	scr, err := ui.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer scr.Close()
	input := ui.Input()
	client = tnet.Client(8 * time.Second)

	dir := appdir.Dir()
	ui.TraceMark("screen open")
	// Read once: the settings screens update sec themselves and save it.
	// Unreadable (new device?): logins are asked again; the old file is
	// kept aside rather than overwritten.
	sec, err := secrets.LoadOrSetAside(filepath.Join(dir, "secrets.enc"))
	ui.TraceMark("secrets read")
	if err != nil {
		log.Printf("secrets: %v", err)
	}
	selKey := "" // the selected machine, kept while Settings is open
	for {
		cfg, err := loadSettings(dir)
		if err != nil {
			scr.Message("MiniRack", err.Error()+". Fix or delete settings.json in App/MiniRack, then open MiniRack again.", ui.Hint{Button: "B", Label: "Exit"})
			waitExit(input)
			return
		}
		if len(cfg.Sources) == 0 {
			if !welcome(scr, input) || !editSource(scr, input, dir, &cfg, sec, -1) {
				return
			}
			continue
		}
		if !dashboard(scr, input, cfg, sec, &selKey) {
			return
		}
		manageSources(scr, input, dir, &cfg, sec)
	}
}

// dashboard shows the machines until the user exits (false) or opens
// settings with Y (true). selKey is the selected machine, which it keeps
// up to date.
func dashboard(scr *ui.Screen, input <-chan ui.Event, cfg Settings, sec map[string]string, selKey *string) bool {
	var fetchers []fetcher
	for _, c := range cfg.Sources {
		switch c.Type {
		case "glances":
			fetchers = append(fetchers, &glances{c: c})
		case "beszel":
			key := c.Secret
			if key == "" {
				key = "beszel"
			}
			fetchers = append(fetchers, &beszel{c: c, password: sec[key]})
		case "vantage":
			fetchers = append(fetchers, &vantage{c: c})
		}
	}

	st := &state{}
	redraw := make(chan struct{}, 1)
	poke := func() {
		select {
		case redraw <- struct{}{}:
		default:
		}
	}
	refresh := func() {
		st.mu.Lock()
		if st.busy {
			st.mu.Unlock()
			return
		}
		st.busy = true
		st.mu.Unlock()
		poke()
		results := make([][]System, len(fetchers))
		var wg sync.WaitGroup
		for i, f := range fetchers {
			wg.Add(1)
			go func(i int, f fetcher) {
				defer wg.Done()
				results[i] = f.Fetch()
			}(i, f)
		}
		wg.Wait()
		st.mu.Lock()
		if st.good == nil {
			st.good, st.goodAt = map[int][]System{}, map[int]time.Time{}
		}
		keep := 3*time.Duration(cfg.RefreshSeconds)*time.Second + time.Minute
		var all []System
		for i, r := range results {
			if !failed(r) {
				st.good[i], st.goodAt[i] = r, time.Now()
			} else if g, ok := st.good[i]; ok && time.Since(st.goodAt[i]) < keep {
				r = make([]System, len(g))
				copy(r, g)
				for j := range r {
					r[j].Stale = st.goodAt[i]
				}
			}
			all = append(all, r...)
		}
		shown, hidden := arrange(all, cfg)
		st.systems, st.hidden, st.updated, st.busy = shown, hidden, time.Now(), false
		if st.history == nil {
			st.history = map[string][]float64{}
		}
		for _, x := range all {
			if x.Up && x.Stale.IsZero() {
				k := machineKey(x)
				h := append(st.history[k], x.CPU)
				if len(h) > 60 {
					h = h[len(h)-60:]
				}
				st.history[k] = h
			}
		}
		st.mu.Unlock()
		poke()
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			refresh()
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(cfg.RefreshSeconds) * time.Second):
			}
		}
	}()

	sel, detail, scroll := 0, false, 0
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	marquee := ui.TickerClock() // the now-playing strip's frames
	defer marquee.Stop()
	skipDraw := false
	for {
		// Music playing in MiniTape's background player scrolls across the top.
		scr.SetTicker(ui.ReadNow())
		st.mu.Lock()
		systems, hidden, updated, busy := st.systems, st.hidden, st.updated, st.busy
		sel = find(systems, *selKey, sel)
		var hist []float64
		var span time.Duration
		if sel < len(systems) {
			x := systems[sel]
			if len(x.History) > 1 { // the source's own history
				hist, span = x.History, x.HistSpan
			} else {
				hist = append(hist, st.history[machineKey(x)]...)
				span = time.Duration(max(0, len(hist)-1)*cfg.RefreshSeconds) * time.Second
			}
		}
		st.mu.Unlock()
		if sel >= len(systems) {
			sel = max(0, len(systems)-1)
		}
		if !skipDraw {
			if detail && len(systems) > 0 {
				drawDetail(scr, systems[sel], hist, span, &scroll, cfg.fahrenheit())
			} else {
				detail = false
				drawOverview(scr, systems, sel, updated, busy, hidden, cfg.fahrenheit())
			}
			scr.Present()
		}
		skipDraw = false

		var frame <-chan time.Time
		if ui.NeedsFrame() {
			frame = time.After(ui.FrameDelay)
		}
		select {
		case <-frame:
			continue
		case ev, ok := <-input:
			if !ok {
				return false
			}
			if !ev.Pressed {
				continue
			}
			switch ev.Button {
			case ui.Menu:
				return false
			case ui.B:
				if detail {
					detail = false
				} else {
					return false
				}
			case ui.Y:
				return true
			case ui.Select:
				if !detail {
					scr.Appearance(input)
				}
			case ui.A:
				if len(systems) > 0 {
					detail, scroll = true, 0
				}
			case ui.X:
				go refresh()
			case ui.Up:
				if detail {
					scroll = max(0, scroll-1)
				} else if sel >= 2 {
					sel -= 2
				}
			case ui.Down:
				if detail {
					scroll++
				} else if sel+2 < len(systems) {
					sel += 2
				}
			case ui.Left:
				if !detail && sel > 0 {
					sel--
				}
			case ui.Right:
				if !detail && sel+1 < len(systems) {
					sel++
				}
			case ui.L1:
				if detail && sel > 0 {
					sel, scroll = sel-1, 0
				}
			case ui.R1:
				if detail && sel+1 < len(systems) {
					sel, scroll = sel+1, 0
				}
			}
		case <-redraw:
		case <-tick.C:
		case <-marquee.C:
			if ui.ReadNow() != "" {
				scr.DrawTicker()
				scr.Present()
			}
			skipDraw = true
		}
		if sel < len(systems) {
			*selKey = machineKey(systems[sel])
		}
	}
}

// machineKey names a machine across refreshes.
func machineKey(x System) string { return x.Source + "/" + x.Name }

// find returns the index of the machine named key, or sel when it isn't
// listed.
func find(systems []System, key string, sel int) int {
	for i, x := range systems {
		if machineKey(x) == key {
			return i
		}
	}
	return sel
}

func waitExit(input <-chan ui.Event) {
	for ev := range input {
		if ev.Pressed && (ev.Button == ui.B || ev.Button == ui.Menu) {
			return
		}
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t).Round(time.Second)
	if d < 2*time.Second {
		return "updated now"
	}
	return "updated " + d.String() + " ago"
}

const (
	cardW = 294
	cardH = 164
	gapX  = 12
	gapY  = 10
)

// drawOverview draws the machine cards. hidden is how many machines that
// are down were left out; f shows temperatures in °F.
func drawOverview(s *ui.Screen, systems []System, sel int, updated time.Time, busy bool, hidden int, f bool) {
	up := 0
	for _, x := range systems {
		if x.Up {
			up++
		}
	}
	sub := ago(updated)
	if len(systems) > 0 {
		sub = fmt.Sprintf("%d of %d up · %s", up, len(systems)+hidden, ago(updated))
	}
	if hidden > 0 {
		sub = fmt.Sprintf("%d of %d up · %d hidden · %s", up, len(systems)+hidden, hidden, ago(updated))
	}
	if len(systems) == 0 {
		top := s.StaticFrame("MiniRack", sub, ui.Hint{Button: "X", Label: "Refresh"}, ui.Hint{Button: "Y", Label: "Settings"}, ui.AppearanceHint, ui.Hint{Button: "B", Label: "Exit"})
		y := top + 120
		switch {
		case hidden > 0:
			s.TextCenter(ui.W/2, y, "No machine is answering", ui.Heading, ui.Text)
			s.TextCenter(ui.W/2, y+ui.LineHeight(ui.Heading)+10, "Settings › Down machines shows them", ui.Body, ui.Muted)
			return
		case !updated.IsZero():
			// read, but the sources list no machine (a new Beszel hub)
			s.TextCenter(ui.W/2, y, "No machines yet", ui.Heading, ui.Text)
			s.TextCenter(ui.W/2, y+ui.LineHeight(ui.Heading)+10, "Your sources list none. X reads them again.", ui.Body, ui.Muted)
			return
		}
		s.Loading(210, "Reading your servers…")
		return
	}
	top := s.StaticFrame("MiniRack", sub, ui.Hint{Button: "A", Label: "Open"}, ui.Hint{Button: "X", Label: "Refresh"}, ui.Hint{Button: "Y", Label: "Settings"}, ui.AppearanceHint, ui.Hint{Button: "B", Label: "Exit"})
	firstRow := 0
	if sel/2 >= 2 {
		firstRow = sel/2 - 1
	}
	for i := firstRow * 2; i < len(systems) && i < (firstRow+2)*2; i++ {
		col, row := i%2, i/2-firstRow
		x := 20 + col*(cardW+gapX)
		y := top + row*(cardH+gapY)
		drawCard(s, image.Rect(x, y, x+cardW, y+cardH), systems[i], i == sel, f)
	}
	if rows := (len(systems) + 1) / 2; rows > 2 {
		// page dots
		cy := float64(ui.H - 56)
		for r := 0; r < rows; r++ {
			c := ui.Line
			if r == sel/2 {
				c = ui.Muted
			}
			s.Circle(float64(ui.W/2-(rows-1)*7+r*14), cy, 3, c)
		}
	}
}

// tempColor marks a hot machine: orange from 75 °C, red from 90 °C.
func tempColor(c float64) color.RGBA {
	switch {
	case c >= 90:
		return ui.Bad
	case c >= 75:
		return ui.Warn
	}
	return ui.Text
}

// loadColor marks a busy machine by its load per CPU thread: orange from
// 1 (every thread busy, work waiting), red from 2. Without a thread count
// the load can't be judged.
func loadColor(x System) color.RGBA {
	n := max(x.Cores, x.Threads)
	if len(x.Load) == 0 || n == 0 {
		return ui.Text
	}
	switch l := x.Load[0] / float64(n); {
	case l >= 2:
		return ui.Bad
	case l >= 1:
		return ui.Warn
	}
	return ui.Text
}

// statsColor is the colour of a card's load and temperature line: the
// worse of the two, else the usual grey.
func statsColor(x System) color.RGBA {
	t, l := ui.Text, loadColor(x)
	if x.Temp > 0 {
		t = tempColor(x.Temp)
	}
	switch {
	case t == ui.Bad || l == ui.Bad:
		return ui.Bad
	case t == ui.Warn || l == ui.Warn:
		return ui.Warn
	}
	return ui.Muted
}

func statusColor(x System) color.RGBA {
	if x.Up {
		return ui.Good
	}
	return ui.Bad
}

func tag(s *ui.Screen, xr, y int, text string, c color.RGBA) {
	w := tagWidth(text)
	s.RoundRectAlpha(image.Rect(xr-w, y, xr, y+22), 11, ui.Text, 0.12)
	s.Text(xr-w+6, y+3, text, ui.Caps, c)
}

func tagWidth(text string) int { return ui.Measure(ui.Caps, text) + 12 }

// Card layout, measured for the pixel font: the name starts after the
// status dot and may run up to the tag at the top right.
const (
	cardNameX   = 32 // from the card's left edge
	cardPadR    = 14
	cardStatsW  = cardW - 36
	cardNameGap = 8
)

// cardTag is the word at the top right of a card and its colour: the
// source while the machine answers, otherwise what is wrong. A machine
// that is up needs no "UP": its live numbers and green dot say so.
func cardTag(x System) (string, color.RGBA) {
	switch {
	case x.Up && !x.Stale.IsZero():
		return "NO REPLY", ui.Warn // showing the last good reading
	case x.Up:
		return strings.ToUpper(x.Source), ui.Muted
	case x.Login:
		return "LOGIN", ui.Bad
	case x.Fail:
		return "UNREACHABLE", ui.Bad
	}
	return "DOWN", ui.Bad
}

// cardName picks the largest face the host name fits in, so "homeserver"
// stays whole. When it needs the room, the source tag goes (the detail
// page names the source); a status tag stays and only then is a long name
// shortened. tag is false when the card should draw no tag.
func cardName(x System) (name string, f font.Face, tag bool) {
	label, _ := cardTag(x)
	withTag := cardW - cardNameX - cardPadR - tagWidth(label) - cardNameGap
	bare := cardW - cardNameX - cardPadR
	for _, f := range []font.Face{ui.Heading, ui.Body} {
		if ui.Measure(f, x.Name) <= withTag {
			return x.Name, f, true
		}
	}
	sourceTag := x.Up && x.Stale.IsZero()
	if sourceTag {
		for _, f := range []font.Face{ui.Heading, ui.Body} {
			if ui.Measure(f, x.Name) <= bare {
				return x.Name, f, false
			}
		}
		return ui.Fit(ui.Body, x.Name, bare), ui.Body, false
	}
	return ui.Fit(ui.Body, x.Name, withTag), ui.Body, true
}

// cardStats is the line under the gauges: load, temperature and uptime in
// the narrow capitals, dropping the load label and then the hours of a
// long uptime before anything is cut. f shows the temperature in °F.
func cardStats(x System, f bool) string {
	try := func(loadLabel bool, upt string) string {
		var parts []string
		if len(x.Load) > 0 {
			l := fmt.Sprintf("%.2f", x.Load[0])
			if loadLabel {
				l = "LOAD " + l
			}
			parts = append(parts, l)
		}
		if x.Temp > 0 {
			parts = append(parts, degrees(x.Temp, f, ""))
		}
		if upt != "" {
			parts = append(parts, "UP "+strings.ToUpper(upt))
		}
		return strings.Join(parts, " · ")
	}
	short := x.Uptime
	if d, _, ok := strings.Cut(x.Uptime, " "); ok && strings.HasSuffix(d, "d") {
		short = d
	}
	for _, line := range []string{try(true, x.Uptime), try(false, x.Uptime), try(false, short)} {
		if ui.Measure(ui.Caps, line) <= cardStatsW {
			return line
		}
	}
	return ui.Fit(ui.Caps, try(false, short), cardStatsW)
}

func drawCard(s *ui.Screen, r image.Rectangle, x System, selected, f bool) {
	if selected {
		s.PanelTint(r, 18, 0.16)
		s.Outline(r, 18, 3, ui.Accent)
	} else {
		s.Panel(r, 18)
	}
	s.Circle(float64(r.Min.X+19), float64(r.Min.Y+25), 5, statusColor(x))
	name, nf, showTag := cardName(x)
	// centre a smaller face on the same line as the Heading would sit
	s.Text(r.Min.X+cardNameX, r.Min.Y+13+(ui.LineHeight(ui.Heading)-ui.LineHeight(nf))/2, name, nf, ui.Text)
	if label, lc := cardTag(x); showTag {
		tag(s, r.Max.X-cardPadR, r.Min.Y+14, label, lc)
	}
	if !x.Up {
		y := r.Min.Y + 56
		for _, line := range clampLines(ui.Wrap(ui.Small, x.Err, r.Dx()-36), 4, func(l string) bool {
			return ui.Measure(ui.Small, l) <= r.Dx()-36
		}) {
			s.Text(r.Min.X+18, y, line, ui.Small, ui.Muted)
			y += ui.LineHeight(ui.Small)
		}
		return
	}
	disk := x.Disk
	if disk <= 0 && len(x.Disks) == 0 {
		disk = -1
	}
	cy := r.Min.Y + 78
	for i, g := range []struct {
		v float64
		l string
	}{{x.CPU, "CPU"}, {x.Mem, "MEMORY"}, {disk, "DISK"}} {
		cx := r.Min.X + 52 + i*95
		s.Gauge(cx, cy, 30, g.v, g.l)
	}
	s.Text(r.Min.X+18, r.Max.Y-26, cardStats(x, f), ui.Caps, statsColor(x))
}

func human(b float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", b, units[i])
}

// chart draws CPU history as a filled line chart in r. The scale fits the
// values (10, 25, 50 or 100%), so a quiet server isn't a flat line along the
// bottom.
func chart(s *ui.Screen, r image.Rectangle, hist []float64, span time.Duration) {
	s.Panel(r, 18)
	title := "CPU"
	if span >= time.Minute {
		title = "CPU · LAST " + strings.ToUpper(shortDur(span))
	}
	s.Text(r.Min.X+16, r.Min.Y+12, title, ui.Caps, ui.Muted)
	plot := image.Rect(r.Min.X+16, r.Min.Y+38, r.Max.X-16, r.Max.Y-16)
	top := 10.0
	for _, v := range hist {
		for v*1.15 > top && top < 100 {
			switch top {
			case 10:
				top = 25
			case 25:
				top = 50
			default:
				top = 100
			}
		}
	}
	s.TextRight(r.Max.X-16, r.Min.Y+12, fmt.Sprintf("%.0f%%", top), ui.Caps, ui.Faint)
	for _, f := range []float64{0, 0.5, 1} {
		y := plot.Max.Y - int(f*float64(plot.Dy()))
		s.FillAlpha(image.Rect(plot.Min.X, y, plot.Max.X, y+1), ui.Text, 0.10)
	}
	if len(hist) < 2 {
		s.TextCenter((plot.Min.X+plot.Max.X)/2, plot.Min.Y+plot.Dy()/2-10, "Collecting…", ui.Small, ui.Faint)
		return
	}
	n := len(hist)
	pts := make([]ui.Pt, n)
	for i, v := range hist {
		pts[i] = ui.Pt{
			X: float64(plot.Min.X) + float64(i)*float64(plot.Dx())/float64(max(1, n-1)),
			Y: float64(plot.Max.Y) - min(1, max(0, v)/top)*float64(plot.Dy()),
		}
	}
	area := append([]ui.Pt{{X: pts[0].X, Y: float64(plot.Max.Y)}}, pts...)
	area = append(area, ui.Pt{X: pts[n-1].X, Y: float64(plot.Max.Y)})
	s.Polygon(area, ui.Mix(ui.Surface, ui.Text, 0.22))
	s.Stroke(pts, 2.5, ui.Accent)
	last := pts[n-1]
	s.Circle(last.X, last.Y, 4.5, ui.Accent)
}

func shortDur(d time.Duration) string {
	switch {
	case d >= 90*time.Minute:
		return fmt.Sprintf("%.0f h", d.Hours())
	case d >= 55*time.Minute:
		return "hour"
	case d >= time.Minute:
		return fmt.Sprintf("%.0f min", d.Minutes())
	}
	return fmt.Sprintf("%.0f s", d.Seconds())
}

// tile draws a small labelled value in colour c.
func tile(s *ui.Screen, r image.Rectangle, label, value string, c color.RGBA) {
	s.Panel(r, 16)
	s.Text(r.Min.X+14, r.Min.Y+9, label, ui.Caps, ui.Muted)
	s.Text(r.Min.X+14, r.Max.Y-9-ui.LineHeight(ui.Heading), ui.Fit(ui.Heading, value, r.Dx()-28), ui.Heading, c)
}

func rate(b float64) string {
	if b < 1024 {
		return fmt.Sprintf("%.0f B/s", b)
	}
	return human(b) + "/s"
}

// row is one line of the detail page's scrolling list.
type row struct {
	section bool
	draw    func(y int)
}

const rowH = 30

// drawDetail draws one machine's page; f shows temperatures in °F.
func drawDetail(s *ui.Screen, x System, hist []float64, span time.Duration, scroll *int, f bool) {
	s.Background()
	// "Beszel · Intel N100 · 4 cores"
	sub := []string{typeLabel(x.Source)}
	if x.CPUModel != "" {
		sub = append(sub, x.CPUModel)
	}
	if x.Cores > 0 {
		c := fmt.Sprintf("%d cores", x.Cores)
		if x.Threads > x.Cores {
			c = fmt.Sprintf("%d cores, %d threads", x.Cores, x.Threads)
		}
		sub = append(sub, c)
	}
	if !x.Stale.IsZero() {
		sub = []string{"Not answering just now · showing the reading from " + time.Since(x.Stale).Round(time.Second).String() + " ago"}
	}
	top := s.Header(x.Name, ui.Fit(ui.Small, strings.Join(sub, "  ·  "), ui.W-48))
	const m = 20 // side margin
	if !x.Up {
		label := "Down"
		if x.Login {
			label = "Can't log in"
		} else if x.Fail {
			label = "Unreachable"
		}
		s.Circle(m+12, float64(top+22), 7, ui.Bad)
		s.Text(m+28, top+6, label, ui.Title, ui.Bad)
		y := top + 54
		for _, line := range ui.Wrap(ui.Body, x.Err, ui.W-2*m-8) {
			s.Text(m+4, y, line, ui.Body, ui.Muted)
			y += ui.LineHeight(ui.Body)
		}
		s.Footer(ui.Hint{Button: "L1/R1", Label: "Other servers"}, ui.Hint{Button: "X", Label: "Refresh"}, ui.Hint{Button: "B", Label: "Back"})
		return
	}

	// Top: two big gauges and the CPU chart.
	cardTop := top + 4
	gauges := image.Rect(m, cardTop, 264, cardTop+144)
	s.Panel(gauges, 18)
	s.Gauge(84, cardTop+58, 44, x.CPU, "CPU")
	s.Gauge(200, cardTop+58, 44, x.Mem, "MEMORY")
	chart(s, image.Rect(276, cardTop, ui.W-m, cardTop+144), hist, span)

	// Tiles: load, uptime, temperature.
	load, up, temp, tc := "–", "–", "–", ui.Text
	if len(x.Load) >= 3 {
		load = fmt.Sprintf("%.2f  %.2f  %.2f", x.Load[0], x.Load[1], x.Load[2])
	}
	if x.Uptime != "" {
		up = x.Uptime
	}
	if x.Temp > 0 {
		temp, tc = degrees(x.Temp, f, " "), tempColor(x.Temp)
	}
	ty := cardTop + 154
	tile(s, image.Rect(m, ty, 264, ty+58), "LOAD", load, loadColor(x))
	tile(s, image.Rect(276, ty, 450, ty+58), "UPTIME", up, ui.Text)
	tile(s, image.Rect(462, ty, ui.W-m, ty+58), "TEMPERATURE", temp, tc)

	// Below: everything else, scrollable.
	const lx, rx = 26, ui.W - 26 // text columns, inside the cards' edges
	var rows []row
	section := func(t string) {
		rows = append(rows, row{true, func(y int) { s.Text(lx, y+10, t, ui.Caps, ui.Muted) }})
	}
	line := func(label, value string) {
		rows = append(rows, row{false, func(y int) {
			s.Text(lx, y+4, ui.Fit(ui.Small, label, 240), ui.Small, ui.Text)
			s.TextRight(rx, y+4, ui.Fit(ui.Small, value, rx-lx-260), ui.Small, ui.Muted)
		}})
	}
	bar := func(label string, pct float64, value string) {
		rows = append(rows, row{false, func(y int) {
			s.Text(lx, y+4, ui.Fit(ui.Small, label, 150), ui.Small, ui.Text)
			s.Bar(image.Rect(186, y+11, 372, y+19), pct/100, ui.Level(pct))
			s.TextRight(426, y+4, fmt.Sprintf("%.0f%%", pct), ui.Small, ui.Text)
			s.TextRight(rx, y+4, value, ui.Small, ui.Muted)
		}})
	}
	if x.MemTotal > 0 || x.Swap > 0 {
		section("MEMORY")
		if x.MemTotal > 0 {
			bar("Used", x.MemUsed/x.MemTotal*100, human(x.MemUsed)+" / "+human(x.MemTotal))
		}
		if x.Swap > 0 {
			bar("Swap", x.Swap, "")
		}
	}
	if x.NetRx > 0 || x.NetTx > 0 || x.DiskRead > 0 || x.DiskWrite > 0 {
		section("TRAFFIC")
		if x.NetRx > 0 || x.NetTx > 0 {
			line("Network", "↓ "+rate(x.NetRx)+"    ↑ "+rate(x.NetTx))
		}
		if x.DiskRead > 0 || x.DiskWrite > 0 {
			line("Disk", "read "+rate(x.DiskRead)+"    write "+rate(x.DiskWrite))
		}
	}
	if len(x.Disks) > 0 {
		section("DISKS")
		for _, d := range x.Disks {
			bar(d.Name, d.Pct, human(d.Used)+" / "+human(d.Total))
		}
	} else if x.Disk > 0 {
		section("DISKS")
		bar("Fullest", x.Disk, "")
	}
	if len(x.Services) > 0 {
		section("SERVICES")
		for _, sv := range x.Services {
			rows = append(rows, row{false, func(y int) {
				c := ui.Good
				switch sv.Status {
				case "up", "idle":
				case "warn":
					c = ui.Warn
				default:
					c = ui.Bad
				}
				s.Circle(lx+5, float64(y+14), 5, c)
				s.Text(lx+18, y+4, ui.Fit(ui.Small, sv.Name, 400), ui.Small, ui.Text)
				s.TextRight(rx, y+4, sv.Status, ui.Small, ui.Muted)
			}})
		}
	}
	if len(x.Sensors) > 1 {
		section("TEMPERATURES")
		for _, se := range x.Sensors {
			line(se.Label, degrees(se.Value, f, " "))
		}
	}
	var sys [][2]string
	for _, kv := range [][2]string{{"Host name", x.Host}, {"System", x.OS}, {"Kernel", x.Kernel}, {"Processor", x.CPUModel}, {"Agent", x.Agent}} {
		if kv[1] != "" && kv[1] != x.Name {
			sys = append(sys, kv)
		}
	}
	if len(sys) > 0 {
		section("SYSTEM")
		for _, kv := range sys {
			line(kv[0], kv[1])
		}
	}

	listTop, listBottom := ty+66, ui.H-58
	fit := (listBottom - listTop) / rowH
	*scroll = max(0, min(*scroll, len(rows)-fit))
	y := listTop
	for i := *scroll; i < len(rows) && y+rowH <= listBottom; i++ {
		// no heading as the last line, cut off from its rows
		if rows[i].section && y+2*rowH > listBottom {
			break
		}
		rows[i].draw(y)
		y += rowH
	}
	if len(rows) > fit { // scroll position
		h := max(16, (listBottom-listTop)*fit/len(rows))
		pos := listTop + (listBottom-listTop-h)**scroll/max(1, len(rows)-fit)
		s.RoundRectAlpha(image.Rect(ui.W-8, pos, ui.W-5, pos+h), 2, ui.Text, 0.4)
	}
	hints := []ui.Hint{{Button: "L1/R1", Label: "Other servers"}, {Button: "X", Label: "Refresh"}}
	if len(rows) > fit {
		hints = append(hints, ui.Hint{Button: "UP/DOWN", Label: "Scroll"})
	}
	s.Footer(append(hints, ui.Hint{Button: "B", Label: "Back"})...)
}

// clampLines keeps the first n lines and, when text was left out, ends the
// last one with an ellipsis (shortened until fits says it fits), so a cut
// message reads as cut. The full text is on the detail page.
func clampLines(lines []string, n int, fits func(string) bool) []string {
	if len(lines) <= n {
		return lines
	}
	lines = append([]string(nil), lines[:n]...)
	last := []rune(strings.TrimRight(lines[n-1], " .,;:"))
	cut := false
	for len(last) > 0 && !fits(string(last)+"…") {
		last, cut = last[:len(last)-1], true
	}
	l := string(last)
	if i := strings.LastIndexByte(l, ' '); cut && i > 0 {
		l = l[:i] // end on a whole word
	}
	lines[n-1] = strings.TrimRight(l, " .,;:") + "…"
	return lines
}
