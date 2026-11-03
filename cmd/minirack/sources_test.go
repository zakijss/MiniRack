package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrimUptime(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"3 days, 4:05:06", "3d 4h"},
		{"1 day, 0:00:00", "1d 0h"},
		{"4:05:06", "4h 05m"},
		{"0:07", "0h 07m"},
		{"12 days", "12d 0h"},
		{"30000 days, 1:00:00", "30000d 1h"}, // overflows int32 if summed as int
		{"", "0h 00m"},
		{"soon", "soon"}, // unknown: shown as given
		{"day", "day"},
	} {
		if got := trimUptime(c.in); got != c.want {
			t.Errorf("trimUptime(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestGlancesHistory(t *testing.T) {
	h := map[string][][2]any{
		"user":   {{"2026-01-01T10:00:00.000000", 10.0}, {"2026-01-01T10:01:30.000000", 20.0}, {"bad", "x"}},
		"system": {{"2026-01-01T10:00:00.000000", 5.0}, {nil, nil}},
	}
	got, span := glancesHistory(h)
	if len(got) != 2 || got[0] != 15 || got[1] != 20 {
		t.Errorf("values %v, want [15 20]", got)
	}
	if span != 0 { // the last sample's time doesn't parse
		t.Errorf("span %v, want 0", span)
	}
	got, span = glancesHistory(map[string][][2]any{"total": {{"2026-01-01T10:00:00", 1.0}, {"2026-01-01T10:02:00", 2.0}}})
	if len(got) != 2 || span != 2*time.Minute {
		t.Errorf("total: %v over %v", got, span)
	}
	if got, _ := glancesHistory(nil); got != nil {
		t.Errorf("empty history gave %v", got)
	}
}

// Sources with broken or unexpected replies show the machine as
// unreachable instead of failing (or panicking) the whole dashboard.
func TestMalformedReplies(t *testing.T) {
	for _, body := range []string{"", "not json", "[]", `{"host": 5}`, `{"items": "x"}`, "null"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/collections/users/auth-with-password" {
				w.Write([]byte(`{"token": "T"}`))
				return
			}
			w.Write([]byte(body))
		}))
		client = srv.Client()
		for _, f := range []fetcher{
			&glances{c: SourceConf{URL: srv.URL}},
			&beszel{c: SourceConf{URL: srv.URL, User: "u"}, password: "p"},
			&vantage{c: SourceConf{URL: srv.URL}},
		} {
			for _, s := range f.Fetch() {
				if s.Name == "" {
					t.Errorf("%T with reply %q: a system without a name", f, body)
				}
			}
		}
		srv.Close()
	}
}

// A first /system that fails is asked again on the next refresh, so the
// host name and OS show once Glances answers.
func TestGlancesSystemRetried(t *testing.T) {
	var systemCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/4/quicklook":
			w.Write([]byte(`{"cpu": 5}`))
		case "/api/4/system":
			if systemCalls.Add(1) == 1 {
				http.Error(w, "busy", http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`{"hostname": "homeserver", "linux_distro": "Debian 13"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	client = srv.Client()
	g := &glances{c: SourceConf{URL: srv.URL}}
	if s := g.Fetch()[0]; s.Host != "" || !s.Up {
		t.Fatalf("first refresh: %+v", s)
	}
	s := g.Fetch()[0]
	if s.Host != "homeserver" || s.OS != "Debian 13" || s.Name != "homeserver" {
		t.Errorf("second refresh: host %q, OS %q, name %q", s.Host, s.OS, s.Name)
	}
	g.Fetch()
	if n := systemCalls.Load(); n != 2 {
		t.Errorf("/system asked %d times, want 2 (until it answers, then never)", n)
	}
}

// beszelHub serves a Beszel hub with an hour of per-minute stats for n
// systems, paging the way PocketBase does with a cap on perPage. Each
// reading's CPU is its minute (0 = an hour ago, 60 = now).
func beszelHub(t *testing.T, n, maxPerPage int) (*httptest.Server, *atomic.Int32) {
	type rec struct {
		System  string         `json:"system"`
		Created string         `json:"created"`
		Stats   map[string]any `json:"stats"`
	}
	now := time.Now().UTC()
	var recs []rec
	for m := 0; m <= 60; m++ {
		at := now.Add(time.Duration(m-60) * time.Minute).Format("2006-01-02 15:04:05.000Z")
		for i := range n {
			recs = append(recs, rec{fmt.Sprintf("sys%02d", i), at, map[string]any{"cpu": float64(m), "m": 8.0, "mu": float64(m) / 10}})
		}
	}
	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/collections/users/auth-with-password":
			w.Write([]byte(`{"token": "T"}`))
		case "/api/collections/systems/records":
			var items []map[string]any
			for i := range n {
				items = append(items, map[string]any{"id": fmt.Sprintf("sys%02d", i), "name": fmt.Sprintf("box %d", i), "status": "up"})
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		case "/api/collections/system_stats/records":
			pages.Add(1)
			sorted := append([]rec(nil), recs...)
			desc := strings.HasPrefix(q.Get("sort"), "-")
			sort.SliceStable(sorted, func(i, j int) bool {
				if desc {
					return sorted[i].Created > sorted[j].Created
				}
				return sorted[i].Created < sorted[j].Created
			})
			per, _ := strconv.Atoi(q.Get("perPage"))
			per = min(per, maxPerPage)
			page, _ := strconv.Atoi(q.Get("page"))
			page = max(page, 1)
			lo, hi := min(len(sorted), (page-1)*per), min(len(sorted), page*per)
			json.NewEncoder(w).Encode(map[string]any{"page": page, "perPage": per, "items": sorted[lo:hi]})
		default:
			http.NotFound(w, r)
		}
	}))
	return srv, &pages
}

// With more systems than one page of stats holds, every system's latest
// reading and whole hour still arrive, oldest first.
func TestBeszelStatsPages(t *testing.T) {
	for _, c := range []struct{ systems, maxPerPage int }{{30, 1000}, {5, 100}, {3, 1000}} {
		srv, pages := beszelHub(t, c.systems, c.maxPerPage)
		client = srv.Client()
		b := &beszel{c: SourceConf{URL: srv.URL, User: "u"}, password: "p"}
		got := b.Fetch()
		srv.Close()
		if len(got) != c.systems {
			t.Fatalf("%+v: %d systems", c, len(got))
		}
		for _, s := range got {
			if len(s.History) != 61 || s.History[0] != 0 || s.History[60] != 60 {
				t.Errorf("%+v: %s: history %v, want minutes 0 to 60", c, s.Name, s.History)
				break
			}
			if s.MemUsed != 6*gib {
				t.Errorf("%+v: %s: memory used %.1f GB, want the latest reading's 6.0", c, s.Name, s.MemUsed/gib)
				break
			}
		}
		want := (c.systems*61)/min(1000, c.maxPerPage) + 1
		if int(pages.Load()) != want {
			t.Errorf("%+v: read %d pages, want %d", c, pages.Load(), want)
		}
	}
}

func TestNewSecretKey(t *testing.T) {
	for _, c := range []struct {
		sec     map[string]string
		sources []SourceConf
		want    string
	}{
		{nil, nil, "beszel-1"},
		{map[string]string{"beszel-1": "pw"}, nil, "beszel-2"},
		// secrets.enc was reset: the sources still name their keys
		{map[string]string{}, []SourceConf{{Type: "beszel", Secret: "beszel-1"}, {Type: "glances"}}, "beszel-2"},
		{map[string]string{"beszel-2": ""}, []SourceConf{{Secret: "beszel-1"}, {Secret: "beszel-3"}}, "beszel-4"},
		{map[string]string{"other": "x"}, []SourceConf{{Secret: "beszel-2"}}, "beszel-1"},
	} {
		if got := newSecretKey(c.sec, c.sources); got != c.want {
			t.Errorf("newSecretKey(%v, %v) = %q, want %q", c.sec, c.sources, got, c.want)
		}
	}
}

func TestClampLines(t *testing.T) {
	in := []string{"one", "two", "three and", "more"}
	got := clampLines(in, 3, func(l string) bool { return len([]rune(l)) <= 8 })
	if len(got) != 3 || got[2] != "three…" {
		t.Errorf("got %q", got)
	}
	if got := clampLines(in[:2], 3, func(string) bool { return true }); len(got) != 2 || got[1] != "two" {
		t.Errorf("short input changed: %q", got)
	}
}
