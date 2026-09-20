package stats

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testStats is the package over a database of its own, its days in UTC.
func testStats(t *testing.T) *Stats {
	t.Helper()
	d := openTestDB(t)
	s, err := New(d.db, Options{Location: time.UTC, Title: "Stats", Log: func(string, ...any) {},
		AnyClientKinds: []string{"skill"}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// drain takes the queued hits through the session step and into the
// store, which is what Run does once a second.
func drain(t *testing.T, s *Stats) {
	t.Helper()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
}

const testBrowser = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"

func get(t *testing.T, h http.Handler, path string, hdr ...string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", "http://site.example"+path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// TestCounted: a browser's navigation to a page that answered 200 is a
// page view; a handler that answered 404, a fetch(), the page's own
// refetch and a POST are not; a crawler is kept apart as a crawl. The
// kind and the path come from the request, whatever the host's routes.
func TestCounted(t *testing.T) {
	s := testStats(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.Counted("home", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("home")) }))
	mux.HandleFunc("GET /gone", s.Counted("page", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	}))
	mux.HandleFunc("GET /skill.md", s.Counted("skill", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("# skill")) }))
	nav := []string{"User-Agent", testBrowser, "Sec-Fetch-Dest", "document", "Accept", "text/html",
		"CF-IPCountry", "JP", "CF-Connecting-IP", "203.0.113.5"}
	get(t, mux, "/", nav...)
	get(t, mux, "/gone", nav...)                                           // answered 404
	get(t, mux, "/", append(nav, "X-Hub-Live", "1")...)                    // the page refetching itself
	get(t, mux, "/", "User-Agent", testBrowser, "Sec-Fetch-Dest", "empty") // a fetch()
	get(t, mux, "/", "User-Agent", "Googlebot/2.1", "Sec-Fetch-Dest", "document", "Accept", "text/html")
	get(t, mux, "/skill.md", "User-Agent", "curl/8.5.0", "Accept", "*/*") // an agent reads the guide
	drain(t, s)

	to := Filter{To: time.Now().UnixMilli() + 1000}
	sum, err := s.db.Summary(to)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pageviews != 2 {
		t.Errorf("page views = %d, want 2 (the home page and the skill guide)", sum.Pageviews)
	}
	if sum.Visitors != 2 || sum.Sessions != 2 {
		t.Errorf("visitors %d sessions %d, want 2 and 2 (the browser, and curl)", sum.Visitors, sum.Sessions)
	}
	bots, _ := s.db.Top(to, "crawler")
	if len(bots) != 1 || bots[0].Key != "Googlebot" || bots[0].N != 1 {
		t.Errorf("crawlers %v, want Googlebot once", bots)
	}
	rows, _ := s.db.Top(to, "path")
	paths := map[string]int{}
	for _, r := range rows {
		paths[r.Key] = r.N
	}
	if paths["/"] != 1 || paths["/skill.md"] != 1 || paths["/gone"] != 0 {
		t.Errorf("paths %v", paths)
	}
	if got, _ := s.db.Top(to, "country"); len(got) != 1 || got[0].Key != "JP" {
		t.Errorf("countries %v, want JP", got)
	}
}

// TestPage: the desk renders its tiles, chart and lists from the hits,
// the host's PathLabel names a page, and the JSON carries the same
// report under the same query.
func TestPage(t *testing.T) {
	s := testStats(t)
	s.opt.PathLabel = func(p string) string {
		if p == "/p/42" {
			return "Ann: hello stats world"
		}
		return p
	}
	now := time.Now().UnixMilli()
	if err := s.db.Add([]Hit{
		{TS: now - 60_000, VID: "a", SID: "s1", Entry: true, Path: "/", Kind: "home", Ref: "Google", Channel: "search",
			Country: "DE", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "de"},
		{TS: now - 30_000, VID: "a", SID: "s1", Path: "/p/42", Kind: "page", Ref: "Google", Channel: "search",
			Country: "DE", Device: "desktop", Browser: "Chrome", OS: "Windows", Lang: "de"},
		{TS: now - 20_000, VID: "b", SID: "s2", Entry: true, Path: "/p/42", Kind: "page", Channel: "direct",
			Country: "CN", Device: "mobile", Browser: "Safari", OS: "iOS", Lang: "zh-CN"},
	}); err != nil {
		t.Fatal(err)
	}
	code, body := get(t, s.Handler(), "/stats?range=24h")
	if code != http.StatusOK {
		t.Fatalf("the page answered %d", code)
	}
	for _, want := range []string{"Stats", "Visitors", "Page views", "Google", "Germany", "Ann: hello stats world",
		"desktop", "<svg", "Live"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not carry %q", want)
		}
	}
	if strings.Contains(body, "{{") {
		t.Error("the page has an unrendered action in it")
	}
	// every state is a URL: a row is a link that filters the view
	if !strings.Contains(body, "country=DE") {
		t.Error("a country row does not filter the view")
	}

	code, body = get(t, s.Handler(), "/v1/stats?range=24h")
	if code != http.StatusOK {
		t.Fatalf("the JSON answered %d", code)
	}
	var rep struct {
		Range   string           `json:"range"`
		Summary Summary          `json:"summary"`
		Lists   map[string][]Row `json:"lists"`
		Live    struct {
			Online int `json:"online"`
		} `json:"live"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Range != "24h" || rep.Summary.Pageviews != 3 || rep.Summary.Visitors != 2 {
		t.Errorf("the report says %+v", rep.Summary)
	}
	for _, l := range []string{"pages", "sources", "countries", "devices"} {
		if len(rep.Lists[l]) == 0 {
			t.Errorf("the JSON has no %s list", l)
		}
	}
}

func TestStatsLanes(t *testing.T) {
	for _, c := range []struct {
		rows []int
		want string
	}{
		{[]int{10, 12, 12, 4, 9}, "0 1|2 3 4"},   // 668 beside 842: a third window left would be 1022 beside 488
		{[]int{7, 12, 12, 4, 12}, "0 1|2 3 4"},   // the public hub's page on the day this was written
		{[]int{12, 12, 12, 12, 12}, "0 1 2|3 4"}, // a tie: the left lane is the taller
		{[]int{12, 0, 0, 0, 12}, "0 1 2|3 4"},    // 626 beside 490 either way round: the tie goes left
		{[]int{0, 0, 0, 0, 0}, "0 1 2|3 4"},
		{[]int{3}, "0|"},
		{[]int{0, 12}, "0|1"}, // two windows never share a lane
	} {
		var lists []List
		for i, n := range c.rows {
			lists = append(lists, List{Anchor: strconv.Itoa(i), Rows: make([]RowView, n)})
		}
		var got []string
		for _, lane := range lanes(lists) {
			var ids []string
			for _, l := range lane {
				ids = append(ids, l.Anchor)
			}
			got = append(got, strings.Join(ids, " "))
		}
		if g := strings.Join(got, "|"); g != c.want {
			t.Errorf("rows %v stand as %q, want %q", c.rows, g, c.want)
		}
	}
}

// TestStatsSpan: the ranges' bounds and buckets in a zone.
func TestStatsSpan(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 9, 16, 14, 30, 0, 0, loc)
	for _, c := range []struct {
		key        string
		from, prev string
		buckets    int
		step       string
	}{
		{"today", "2026-09-16T00:00", "2026-09-15T00:00", 24, "hour"},
		{"yesterday", "2026-09-15T00:00", "2026-09-14T00:00", 24, "hour"},
		{"24h", "2026-09-15T14:30", "2026-09-14T14:30", 25, "hour"},
		{"7d", "2026-09-10T00:00", "2026-09-03T00:00", 7, "day"},
		{"30d", "2026-08-18T00:00", "2026-07-19T00:00", 30, "day"},
		{"3m", "2026-07-01T00:00", "2026-04-01T00:00", 78, "day"},
		{"6m", "2026-04-01T00:00", "2025-10-01T00:00", 6, "month"},
		{"12m", "2025-10-01T00:00", "2024-10-01T00:00", 12, "month"},
		{"bogus", "2026-09-10T00:00", "2026-09-03T00:00", 7, "day"},
	} {
		sp := spanOf(c.key, now, loc)
		if got := sp.From.Format("2006-01-02T15:04"); got != c.from {
			t.Errorf("%s from %s, want %s", c.key, got, c.from)
		}
		if got := sp.PrevFrom.Format("2006-01-02T15:04"); got != c.prev {
			t.Errorf("%s prev from %s, want %s", c.key, got, c.prev)
		}
		if len(sp.Buckets) != c.buckets || sp.Step != c.step {
			t.Errorf("%s: %d %s buckets, want %d %s", c.key, len(sp.Buckets), sp.Step, c.buckets, c.step)
		}
		if !sp.PrevTo.Equal(sp.From) && c.key != "today" {
			t.Errorf("%s: the span before ends where this begins", c.key)
		}
		if sp.IncompleteLast == (c.key == "yesterday") {
			t.Errorf("%s: incomplete %v", c.key, sp.IncompleteLast)
		}
	}
	if ceil(3) != 4 || ceil(37) != 40 || ceil(130) != 160 || ceil(1000) != 1200 {
		t.Error("ceilings")
	}
	if num(1234567) != "1,234,567" || num(999) != "999" {
		t.Error("numbers")
	}
	if dur(75.4) != "1m 15s" || dur(9) != "9s" {
		t.Error("durations")
	}
	if signed(12.34, "%") != "+12.3%" || signed(-5, "%") != "−5.0%" || signed(0, " pp") != "±0 pp" {
		t.Error("signed")
	}
}

var _ = http.StatusOK

// Stop writes what the collector still held, so a host can close its
// database behind it.
func TestStopDrains(t *testing.T) {
	s := testStats(t)
	go s.Run()
	r := httptest.NewRequest("GET", "http://site.example/late", nil)
	r.Header.Set("User-Agent", testBrowser)
	r.Header.Set("Sec-Fetch-Dest", "document")
	s.Collector().Record(r, "page")
	s.Stop()
	s.ResetCache()
	sum, err := s.db.Summary(Filter{To: time.Now().UnixMilli() + 1000})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pageviews != 1 {
		t.Errorf("page views after Stop = %d, want the one that was queued", sum.Pageviews)
	}
}
