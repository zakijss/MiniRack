package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zakijss/minideck/tnet"
)

// System is one machine as the dashboard shows it.
type System struct {
	Name     string
	Source   string // "glances", "beszel", "vantage"
	Up       bool
	Err      string
	Fail     bool    // couldn't read the source at all (vs. the machine being down)
	Login    bool    // the source answered but refused the login
	CPU      float64 // percent
	Mem      float64
	Disk     float64 // percent of the fullest disk
	Load     []float64
	Uptime   string
	Temp     float64 // 0 = unknown
	Disks    []Disk
	Services []Service
	Sensors  []Sensor

	// More detail, where the source has it (0 or "" = unknown).
	MemUsed, MemTotal float64 // bytes
	Swap              float64 // percent
	NetRx, NetTx      float64 // bytes/s, all real interfaces
	DiskRead          float64 // bytes/s
	DiskWrite         float64
	Host, OS, Kernel  string
	CPUModel          string
	Cores, Threads    int
	Agent             string        // the monitoring agent and its version
	History           []float64     // CPU %, oldest first, from the source
	HistSpan          time.Duration // the time History covers
	Stale             time.Time     // set when showing the last good reading after a failed refresh
}

// Disk is one file system and how full it is.
type Disk struct {
	Name        string
	Pct         float64
	Used, Total float64 // bytes
}

// Service is a service Vantage watches and its status ("up", "warn", ...).
type Service struct{ Name, Status string }

// Sensor is a temperature reading in °C.
type Sensor struct {
	Label string
	Value float64
}

// SourceConf is one source in settings.json.
type SourceConf struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	User   string `json:"user"`
	Secret string `json:"secret"` // key in secrets.enc holding the password
}

type fetcher interface {
	Fetch() []System
}

var client *http.Client

func getJSON(method, u string, body any, hdr map[string]string, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return shortErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, resp.Body)
		return &httpErr{resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out); err != nil {
		return shortErr(err) // a timeout while reading the body, too
	}
	return nil
}

type httpErr struct{ code int }

func (e *httpErr) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

// shortErr keeps error text readable on a small screen (and free of URLs).
func shortErr(err error) error { return tnet.Explain(err) }

func down(name, src string, err error) System {
	return System{Name: name, Source: src, Err: err.Error(), Fail: true, Login: errors.Is(err, errLogin)}
}

// errLogin marks a refused login (wrong or missing credentials).
var errLogin = errors.New("login refused")

type loginErr string

func (e loginErr) Error() string        { return string(e) }
func (e loginErr) Is(target error) bool { return target == errLogin }

// ---- Glances (REST API v4, falling back to v3) ----

type glances struct {
	c   SourceConf
	api string // "/api/4" or "/api/3"
	sys *glancesSystem
}

// glancesSystem is Glances' /system: the host's name and OS.
type glancesSystem struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os_name"`
	Distro   string `json:"linux_distro"`
	Platform string `json:"platform"`
	Version  string `json:"os_version"`
}

func (g *glances) get(path string, out any) error {
	base := strings.TrimRight(g.c.URL, "/")
	if g.api == "" {
		for _, v := range []string{"/api/4", "/api/3"} {
			if err := getJSON("GET", base+v+path, nil, nil, out); err == nil {
				g.api = v
				return nil
			} else if he, ok := err.(*httpErr); !ok || he.code != 404 {
				return err
			}
		}
		//lint:ignore ST1005 shown on screen as a sentence
		return errors.New("No Glances API at this address. Check that it runs glances -w.")
	}
	return getJSON("GET", base+g.api+path, nil, nil, out)
}

func (g *glances) Fetch() []System {
	name := g.c.Name
	var ql struct {
		CPU      float64 `json:"cpu"`
		Mem      float64 `json:"mem"`
		Swap     float64 `json:"swap"`
		CPUName  string  `json:"cpu_name"`
		LogCores int     `json:"cpu_log_core"`
		PhysCore int     `json:"cpu_phys_core"`
	}
	if err := g.get("/quicklook", &ql); err != nil {
		if name == "" {
			name = g.c.URL
		}
		return []System{down(name, "glances", err)}
	}
	s := System{Name: name, Source: "glances", Up: true, CPU: ql.CPU, Mem: ql.Mem, Swap: ql.Swap,
		CPUModel: ql.CPUName, Cores: ql.PhysCore, Threads: ql.LogCores, Agent: "Glances"}
	if g.sys == nil { // doesn't change: ask until it answers once
		var sys glancesSystem
		if g.get("/system", &sys) == nil {
			g.sys = &sys
		}
	}
	if g.sys != nil {
		s.Host = g.sys.Hostname
		s.OS = g.sys.Distro
		if s.OS == "" {
			s.OS = strings.TrimSpace(g.sys.OS + " " + g.sys.Platform)
		}
		s.Kernel = g.sys.Version
	}
	if s.Name == "" {
		s.Name = s.Host
		if s.Name == "" {
			s.Name = g.c.URL
		}
	}
	var mem struct{ Total, Used float64 }
	if g.get("/mem", &mem) == nil {
		s.MemUsed, s.MemTotal = mem.Used, mem.Total
	}
	var nets []struct {
		Name    string  `json:"interface_name"`
		RxRate  float64 `json:"bytes_recv_rate_per_sec"` // API 4
		TxRate  float64 `json:"bytes_sent_rate_per_sec"`
		Rx      float64 `json:"rx"` // API 3: bytes since the last update
		Tx      float64 `json:"tx"`
		Elapsed float64 `json:"time_since_update"`
	}
	if g.get("/network", &nets) == nil {
		for _, n := range nets {
			if virtualIface(n.Name) {
				continue
			}
			if n.RxRate > 0 || n.TxRate > 0 {
				s.NetRx += n.RxRate
				s.NetTx += n.TxRate
			} else if n.Elapsed > 0 {
				s.NetRx += n.Rx / n.Elapsed
				s.NetTx += n.Tx / n.Elapsed
			}
		}
	}
	// Glances keeps its own CPU history: the chart starts full.
	var hist map[string][][2]any
	if g.get("/cpu/history/90", &hist) == nil {
		s.History, s.HistSpan = glancesHistory(hist)
	}
	var load struct{ Min1, Min5, Min15 float64 }
	if g.get("/load", &load) == nil {
		s.Load = []float64{load.Min1, load.Min5, load.Min15}
	}
	var up any
	if g.get("/uptime", &up) == nil {
		switch v := up.(type) {
		case string:
			s.Uptime = trimUptime(v)
		case map[string]any:
			if t, ok := v["uptime"].(string); ok {
				s.Uptime = trimUptime(t)
			}
		}
	}
	var fs []struct {
		Mnt     string  `json:"mnt_point"`
		Percent float64 `json:"percent"`
		Size    float64 `json:"size"`
		Used    float64 `json:"used"`
	}
	if g.get("/fs", &fs) == nil {
		for _, f := range fs {
			s.Disks = append(s.Disks, Disk{f.Mnt, f.Percent, f.Used, f.Size})
			if f.Percent > s.Disk {
				s.Disk = f.Percent
			}
		}
	}
	var sensors []struct {
		Label string  `json:"label"`
		Value float64 `json:"value"`
		Type  string  `json:"type"`
		Unit  string  `json:"unit"`
	}
	if g.get("/sensors", &sensors) == nil {
		for _, x := range sensors {
			if strings.HasPrefix(x.Type, "temperature") && x.Value > 0 && x.Value < 150 {
				s.Sensors = append(s.Sensors, Sensor{x.Label, x.Value})
				if x.Value > s.Temp {
					s.Temp = x.Value
				}
			}
		}
	}
	return []System{s}
}

// trimUptime turns Glances' "3 days, 4:05:06" (or "4:05:06") into the
// same short form fmtSecs uses ("3d 4h", "4h 05m").
func trimUptime(u string) string {
	days, h, m, sec := 0, 0, 0, 0
	rest := u
	if i := strings.Index(u, "day"); i > 0 {
		fmt.Sscanf(strings.TrimSpace(u[:i]), "%d", &days)
		if j := strings.Index(u, ","); j > 0 {
			rest = strings.TrimSpace(u[j+1:])
		} else {
			rest = ""
		}
	}
	if rest != "" {
		if n, _ := fmt.Sscanf(rest, "%d:%d:%d", &h, &m, &sec); n < 2 {
			return u
		}
	}
	// in float64: days*86400 overflows a 32-bit int past ~68 years
	return fmtSecs(float64(days)*86400 + float64(h)*3600 + float64(m)*60 + float64(sec))
}

func fmtSecs(sec float64) string {
	d := time.Duration(sec) * time.Second
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, h)
	}
	return fmt.Sprintf("%dh %02dm", h, m)
}

// ---- Beszel hub (PocketBase API) ----

type beszel struct {
	c        SourceConf
	password string
	mu       sync.Mutex
	token    string

	// Beszel saves a stats record per system each minute: the last hour of
	// them gives the CPU chart and the detail (memory, network, disks,
	// temperatures). Read once a minute, not on every refresh.
	statsAt time.Time
	hist    map[string][]float64 // CPU % per system id
	latest  map[string]beszelStats
}

type beszelStats struct {
	CPU       float64            `json:"cpu"`
	Mem       float64            `json:"m"`  // GB
	MemUsed   float64            `json:"mu"` // GB
	Swap      float64            `json:"s"`
	SwapUsed  float64            `json:"su"`
	Disk      float64            `json:"d"` // GB
	DiskUsed  float64            `json:"du"`
	DiskRead  float64            `json:"dr"` // MB/s
	DiskWrite float64            `json:"dw"`
	NetSent   float64            `json:"ns"` // MB/s
	NetRecv   float64            `json:"nr"`
	Temps     map[string]float64 `json:"t"`
	ExtraFS   map[string]struct {
		Disk     float64 `json:"d"`
		DiskUsed float64 `json:"du"`
	} `json:"efs"`
}

const gib, mib = 1 << 30, 1 << 20

func (b *beszel) auth() map[string]string { return map[string]string{"Authorization": b.token} }

// get does an authenticated GET, logging in again if the token expired.
func (b *beszel) get(u string, out any) error {
	err := getJSON("GET", u, nil, b.auth(), out)
	if he, ok := err.(*httpErr); ok && (he.code == 401 || he.code == 403) {
		if err = b.login(); err == nil {
			err = getJSON("GET", u, nil, b.auth(), out)
		}
	}
	return err
}

func (b *beszel) login() error {
	var r struct {
		Token string `json:"token"`
	}
	err := getJSON("POST", strings.TrimRight(b.c.URL, "/")+"/api/collections/users/auth-with-password", map[string]string{
		"identity": b.c.User, "password": b.password}, nil, &r)
	if he, ok := err.(*httpErr); ok && (he.code == 400 || he.code == 401) {
		return loginErr("Beszel didn't accept the email and password. Press Y to change them.")
	}
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	b.token = r.Token
	return nil
}

func (b *beszel) Fetch() []System {
	b.mu.Lock()
	defer b.mu.Unlock()
	label := b.c.Name
	if label == "" {
		label = "Beszel"
	}
	if b.c.User == "" || b.password == "" {
		return []System{down(label, "beszel", loginErr("No login yet. Press Y to add the email and password."))}
	}
	if b.token == "" {
		if err := b.login(); err != nil {
			return []System{down(label, "beszel", err)}
		}
	}
	base := strings.TrimRight(b.c.URL, "/")
	var r struct {
		Items []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
			Info   struct {
				Host     string    `json:"h"`
				Kernel   string    `json:"k"`
				Cores    int       `json:"c"`
				Threads  int       `json:"t"`
				CPUModel string    `json:"m"`
				CPU      float64   `json:"cpu"`
				MP       float64   `json:"mp"`
				DP       float64   `json:"dp"`
				LA       []float64 `json:"la"`
				U        float64   `json:"u"`
				DT       float64   `json:"dt"`
				Agent    string    `json:"v"`
			} `json:"info"`
		} `json:"items"`
	}
	if err := b.get(base+"/api/collections/systems/records?perPage=200&sort=name", &r); err != nil {
		return []System{down(label, "beszel", err)}
	}
	if time.Since(b.statsAt) > time.Minute {
		b.readStats(base) // best effort: the cards work without it
	}
	var out []System
	for _, it := range r.Items {
		s := System{Name: it.Name, Source: "beszel", Up: it.Status == "up"}
		if !s.Up {
			s.Err = "Beszel: " + it.Status
			out = append(out, s)
			continue
		}
		in := it.Info
		s.CPU, s.Mem, s.Disk, s.Temp, s.Load = in.CPU, in.MP, in.DP, in.DT, in.LA
		if in.U > 0 {
			s.Uptime = fmtSecs(in.U)
		}
		s.Host, s.Kernel, s.CPUModel, s.Cores, s.Threads = in.Host, in.Kernel, in.CPUModel, in.Cores, in.Threads
		s.Agent = "Beszel agent"
		if in.Agent != "" {
			s.Agent += " " + in.Agent
		}
		if h := b.hist[it.ID]; len(h) > 1 {
			s.History, s.HistSpan = h, time.Duration(len(h)-1)*time.Minute
		}
		if st, ok := b.latest[it.ID]; ok {
			s.MemUsed, s.MemTotal = st.MemUsed*gib, st.Mem*gib
			if st.Swap > 0 {
				s.Swap = st.SwapUsed / st.Swap * 100
			}
			s.NetRx, s.NetTx = st.NetRecv*mib, st.NetSent*mib
			s.DiskRead, s.DiskWrite = st.DiskRead*mib, st.DiskWrite*mib
			if st.Disk > 0 {
				s.Disks = append(s.Disks, Disk{"/", st.DiskUsed / st.Disk * 100, st.DiskUsed * gib, st.Disk * gib})
			}
			names := make([]string, 0, len(st.ExtraFS))
			for n := range st.ExtraFS {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				f := st.ExtraFS[n]
				if f.Disk > 0 {
					s.Disks = append(s.Disks, Disk{n, f.DiskUsed / f.Disk * 100, f.DiskUsed * gib, f.Disk * gib})
				}
			}
			names = names[:0]
			for n := range st.Temps {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				s.Sensors = append(s.Sensors, Sensor{n, st.Temps[n]})
			}
		}
		out = append(out, s)
	}
	return out
}

// statsPages caps how many pages of stats readStats reads: 20 pages of
// 1000 records is an hour of every system for a hub with over 300 of them,
// and keeps the work bounded on the Miyoo with a bigger hub.
const statsPages = 20

// readStats loads the last hour of per-minute stats for every system.
// There is a record per system per minute, more than one page holds once a
// hub has 17 systems, so it reads page after page, newest first: if the cap
// cuts the hour short, what is lost is the oldest part of the chart, never
// the latest reading.
func (b *beszel) readStats(base string) {
	since := time.Now().UTC().Add(-61 * time.Minute).Format("2006-01-02 15:04:05.000Z")
	type point struct {
		at  string // PocketBase's dates sort as text
		cpu float64
	}
	points := map[string][]point{}
	latest, latestAt := map[string]beszelStats{}, map[string]string{}
	for page := 1; page <= statsPages; page++ {
		q := url.Values{
			"filter":    {fmt.Sprintf("type='1m' && created>='%s'", since)},
			"sort":      {"-created"},
			"page":      {fmt.Sprint(page)},
			"perPage":   {"1000"},
			"skipTotal": {"1"}, // counting every match costs the hub a query
			"fields":    {"system,created,stats"},
		}
		var r struct {
			PerPage int `json:"perPage"`
			Items   []struct {
				System  string      `json:"system"`
				Created string      `json:"created"`
				Stats   beszelStats `json:"stats"`
			} `json:"items"`
		}
		if err := b.get(base+"/api/collections/system_stats/records?"+q.Encode(), &r); err != nil {
			return
		}
		// Keep only what the cards use (the CPU chart and the latest
		// reading), whatever order the records came in: each page's full
		// records are dropped before the next is read.
		for _, it := range r.Items {
			points[it.System] = append(points[it.System], point{it.Created, it.Stats.CPU})
			if at, ok := latestAt[it.System]; !ok || it.Created > at {
				latest[it.System], latestAt[it.System] = it.Stats, it.Created
			}
		}
		// A short page is the last one. The hub may hand out fewer records
		// per page than asked for (its own cap): it reports that perPage.
		if per := r.PerPage; len(r.Items) == 0 || per <= 0 && len(r.Items) < 1000 || per > 0 && len(r.Items) < per {
			break
		}
	}
	b.statsAt = time.Now()
	b.hist, b.latest = map[string][]float64{}, latest
	for sys, ps := range points {
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].at < ps[j].at }) // the chart runs oldest first
		h := make([]float64, len(ps))
		for i, p := range ps {
			h[i] = p.cpu
		}
		b.hist[sys] = h
	}
}

// ---- Vantage (/api/status, read-only, no login) ----

type vantage struct{ c SourceConf }

func (v *vantage) Fetch() []System {
	name := v.c.Name
	if name == "" {
		name = "Vantage host"
	}
	var st struct {
		Host struct {
			CPU      float64   `json:"cpu"`
			Load     []float64 `json:"load"`
			Uptime   float64   `json:"uptime"`
			MemTotal float64   `json:"mem_total"`
			MemAvail float64   `json:"mem_avail"`
			Temp     *float64  `json:"temp"`
		} `json:"host"`
		Disks []struct {
			Name  string  `json:"name"`
			Total float64 `json:"total"`
			Used  float64 `json:"used"`
		} `json:"disks"`
		Services []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"services"`
		Peers map[string]struct {
			CPU      float64   `json:"cpu"`
			Load     []float64 `json:"load"`
			Uptime   float64   `json:"uptime"`
			MemTotal float64   `json:"mem_total"`
			MemAvail float64   `json:"mem_avail"`
			Temp     *float64  `json:"temp"`
			Fresh    bool      `json:"fresh"`
		} `json:"peers"`
	}
	if err := getJSON("GET", strings.TrimRight(v.c.URL, "/")+"/api/status", nil, nil, &st); err != nil {
		return []System{down(name, "vantage", err)}
	}
	h := st.Host
	s := System{Name: name, Source: "vantage", Up: true, CPU: h.CPU, Load: h.Load, Uptime: fmtSecs(h.Uptime)}
	if h.MemTotal > 0 {
		s.Mem = (h.MemTotal - h.MemAvail) / h.MemTotal * 100
	}
	if h.Temp != nil {
		s.Temp = *h.Temp
	}
	for _, d := range st.Disks {
		if d.Total > 0 {
			p := d.Used / d.Total * 100
			s.Disks = append(s.Disks, Disk{d.Name, p, d.Used, d.Total})
			if p > s.Disk {
				s.Disk = p
			}
		}
	}
	for _, sv := range st.Services {
		s.Services = append(s.Services, Service{sv.Name, sv.Status})
	}
	out := []System{s}
	names := make([]string, 0, len(st.Peers))
	for n := range st.Peers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := st.Peers[n]
		ps := System{Name: n, Source: "vantage", Up: p.Fresh, CPU: p.CPU, Load: p.Load, Uptime: fmtSecs(p.Uptime)}
		if !p.Fresh {
			ps.Err = "No recent data."
		}
		if p.MemTotal > 0 {
			ps.Mem = (p.MemTotal - p.MemAvail) / p.MemTotal * 100
		}
		if p.Temp != nil {
			ps.Temp = *p.Temp
		}
		out = append(out, ps)
	}
	return out
}

// virtualIface reports interfaces whose traffic is counted elsewhere too
// (loopback, containers, bridges, VPN tunnels).
func virtualIface(n string) bool {
	for _, p := range []string{"lo", "veth", "docker", "br-", "virbr", "tailscale", "wg", "tun", "cni", "flannel", "vnet"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// glancesHistory turns Glances' {"user": [[time, value]...], "system": ...}
// into total CPU % per sample, and the time covered.
func glancesHistory(h map[string][][2]any) ([]float64, time.Duration) {
	series := h["total"]
	var extra [][2]any
	if series == nil {
		series, extra = h["user"], h["system"]
	}
	var out []float64
	for i, p := range series {
		v, ok := p[1].(float64)
		if !ok {
			continue
		}
		if i < len(extra) {
			if e, ok := extra[i][1].(float64); ok {
				v += e
			}
		}
		out = append(out, v)
	}
	var span time.Duration
	if len(series) > 1 {
		t0, _ := series[0][0].(string)
		t1, _ := series[len(series)-1][0].(string)
		a, e1 := time.Parse("2006-01-02T15:04:05.999999", t0)
		b, e2 := time.Parse("2006-01-02T15:04:05.999999", t1)
		if e1 == nil && e2 == nil {
			span = b.Sub(a)
		}
	}
	return out, span
}
