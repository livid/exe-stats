package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dayStats is the package over a file of its own the way a host opens
// one — WAL, a busy timeout, one writer — and, with reader, a read-only
// pool beside it for the reports.
func dayStats(t *testing.T, loc *time.Location, reader bool) *Stats {
	t.Helper()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	o := Options{Location: loc, Log: func(f string, a ...any) { t.Logf(f, a...) }}
	if reader {
		rd, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
		if err != nil {
			t.Fatal(err)
		}
		rd.SetMaxOpenConns(4)
		t.Cleanup(func() { rd.Close() })
		o.Reader = rd
	}
	s, err := New(db, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// seedDays writes hits the way the collector makes them — a visitor's
// id is one day's in the zone, a session is one visitor's and ends by
// midnight — over the n days before now and today up to now: a few
// readers a day, some days none, a crawler, campaigns, a long tail of
// pages (past topMax over a year), and hits on a day's first and last
// millisecond.
func seedDays(t *testing.T, d *DB, loc *time.Location, now time.Time, n int) {
	t.Helper()
	r := rand.New(rand.NewPCG(uint64(n), 7))
	pick := func(xs ...string) string { return xs[r.IntN(len(xs))] }
	nl := now.In(loc)
	var hits []Hit
	for i := n; i >= 0; i-- {
		start := midnight(nl.Year(), nl.Month(), nl.Day()-i, loc)
		end := midnight(nl.Year(), nl.Month(), nl.Day()-i+1, loc)
		if end.After(now) {
			end = now
		}
		day := start.In(loc).Format("2006-01-02")
		if r.IntN(9) == 0 {
			continue // nobody came
		}
		for v := range 1 + r.IntN(6) {
			vid := fmt.Sprintf("%s-%d", day, v)
			at := start.Add(time.Duration(r.Int64N(int64(end.Sub(start)))))
			country, device, browser, os, lang := pick("JP", "US", "CN", ""), pick("desktop", "mobile"), pick("Chrome", "Safari"), pick("macOS", "iOS", "Windows"), pick("en", "zh-CN", "ja")
			for sn := range 1 + r.IntN(3) {
				sid := fmt.Sprintf("%s-%d", vid, sn)
				ref, ch := pick("", "google", "v2ex.com"), ""
				switch ref {
				case "":
					ch = "direct"
				case "google":
					ch = "search"
				default:
					ch = "referral"
				}
				camp := ""
				if r.IntN(5) == 0 {
					camp = pick("fall", "launch")
				}
				for h := range 1 + r.IntN(4) {
					if !at.Before(end) {
						break
					}
					hits = append(hits, Hit{TS: at.UnixMilli(), VID: vid, SID: sid, Entry: h == 0,
						Path: fmt.Sprintf("/p/%d", r.IntN(700)), Kind: "page", Ref: ref, Channel: ch,
						Country: country, Region: pick("", "Tokyo"), City: pick("", "Shibuya"), Device: device,
						Browser: browser, OS: os, Lang: lang, UTMCampaign: camp})
					at = at.Add(time.Duration(r.Int64N(int64(20 * time.Minute))))
				}
				at = at.Add(31*time.Minute + time.Duration(r.Int64N(int64(3*time.Hour))))
			}
		}
		if r.IntN(3) == 0 {
			bot := pick("Googlebot", "Bingbot")
			hits = append(hits, Hit{TS: start.Add(time.Hour).UnixMilli(), VID: day + "-" + bot, SID: day + "-" + bot, Entry: true,
				Path: fmt.Sprintf("/p/%d", r.IntN(40)), Kind: "page", Device: "bot", Browser: bot, Bot: true})
		}
		if end.Equal(now) {
			continue
		}
		edge := day + "-edge"
		hits = append(hits,
			Hit{TS: start.UnixMilli(), VID: edge, SID: edge + "-a", Entry: true, Path: "/", Kind: "home", Channel: "direct", Country: "JP"},
			Hit{TS: end.UnixMilli() - 1, VID: edge, SID: edge + "-b", Entry: true, Path: "/", Kind: "home", Channel: "direct", Country: "JP"})
	}
	if err := d.Add(hits); err != nil {
		t.Fatal(err)
	}
}

// reportJSON is a report as the JSON says it.
func reportJSON(t *testing.T, s *Stats, key string, f Filter, lists []string, now time.Time) string {
	t.Helper()
	r, err := s.build(spanOf(key, now, s.coll.Location()), f, lists, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// rawly is a report read from the hits alone, as before the days were kept.
func rawly(t *testing.T, s *Stats, key string, f Filter, lists []string, now time.Time) string {
	t.Helper()
	s.seal = 100 * 365 * 24 * time.Hour
	defer func() { s.seal = sealDelay }()
	return reportJSON(t, s, key, f, lists, now)
}

var dayFilters = []Filter{{}, {Country: "JP"}, {Bot: "all"}, {Path: "/"}, {Channel: "search", Device: "mobile"}}

// TestDaysMatchHits: every range, under every filter, in zones with a
// clock change (Santiago's skips 00:00, Havana's shows it twice) and
// with half and three-quarter hours, reads the same from kept days as
// from the hits — once when the days are counted, again when they are
// only read — page lists and JSON lists alike.
func TestDaysMatchHits(t *testing.T) {
	for _, zone := range []string{"UTC", "America/Los_Angeles", "America/Santiago", "America/Havana", "Asia/Kolkata", "Asia/Kathmandu"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Skip(err)
			}
			s := dayStats(t, loc, zone != "UTC")
			now := time.Now()
			seedDays(t, s.db, loc, now, 420)
			page := []string{"sources", "pages", "countries", "devices", "crawlers"}
			for _, f := range dayFilters {
				for _, rg := range ranges {
					for _, lists := range [][]string{page, nil} {
						want := rawly(t, s, rg.key, f, lists, now)
						for pass := range 2 {
							if got := reportJSON(t, s, rg.key, f, lists, now); got != want {
								t.Fatalf("%s %+v lists %v pass %d:\n kept %s\n hits %s", rg.key, f, lists, pass, got, want)
							}
						}
					}
				}
			}
		})
	}
}

// TestDaysLateHit: a hit written for a day already kept — a seeder's,
// or one held up past sealDelay — is in the next report.
func TestDaysLateHit(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 20)
	before := reportJSON(t, s, "30d", Filter{}, nil, now)
	late := now.AddDate(0, 0, -3).UnixMilli()
	if err := s.db.Add([]Hit{{TS: late, VID: "late", SID: "late", Entry: true, Path: "/late", Kind: "page", Channel: "direct"}}); err != nil {
		t.Fatal(err)
	}
	got, want := reportJSON(t, s, "30d", Filter{}, nil, now), rawly(t, s, "30d", Filter{}, nil, now)
	if got != want || got == before {
		t.Fatalf("after a late hit:\n kept %s\n hits %s", got, want)
	}
}

// TestDaysSweep: the days a sweep took hits from are counted again from
// what is left.
func TestDaysSweep(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 40)
	reportJSON(t, s, "3m", Filter{}, nil, now)
	if _, err := s.Sweep(now.AddDate(0, 0, -10).Add(7 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, want := reportJSON(t, s, "3m", Filter{}, nil, now), rawly(t, s, "3m", Filter{}, nil, now); got != want {
		t.Fatalf("after a sweep:\n kept %s\n hits %s", got, want)
	}
}

// TestDaysViews: past maxViews filtered views, the ones read longest ago
// give their days back; the view without filters keeps its.
func TestDaysViews(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 10)
	reportJSON(t, s, "7d", Filter{}, nil, now)
	for i := range maxViews + 3 {
		reportJSON(t, s, "7d", Filter{Path: fmt.Sprintf("/p/%d", i)}, nil, now.Add(time.Duration(i)*markEvery))
	}
	vs, err := s.db.filteredViews(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != maxViews || vs[0].filter != viewOf(Filter{Path: fmt.Sprintf("/p/%d", maxViews+2)}) {
		t.Fatalf("%d filtered views kept, newest %q", len(vs), vs[0].filter)
	}
	var orphans, plain int
	s.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days WHERE view NOT IN (SELECT id FROM hits_views)`).Scan(&orphans)
	s.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days d JOIN hits_views v ON v.id = d.view WHERE v.filter = ''`).Scan(&plain)
	if orphans != 0 || plain == 0 {
		t.Fatalf("%d rows of dropped views, %d parts of the view without filters", orphans, plain)
	}
}

// TestDaysCountedBy: another zone, or another version of the counting,
// starts the kept days over.
func TestDaysCountedBy(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 10)
	reportJSON(t, s, "7d", Filter{}, nil, now)
	var n int
	if s.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days`).Scan(&n); n == 0 {
		t.Fatal("nothing kept")
	}
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	if _, err := New(s.db.db, Options{Location: tokyo}); err != nil {
		t.Fatal(err)
	}
	if s.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days`).Scan(&n); n != 0 {
		t.Fatalf("%d parts kept from UTC's midnights", n)
	}
}

// TestSplit: a day is kept once it ended sealDelay ago; before that it
// is read from the hits with today, and a span that begins inside a day
// reads that piece from the hits too.
func TestSplit(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	mid := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	days, raw := s.split(mid.AddDate(0, 0, -2), mid.Add(time.Minute), mid.Add(time.Minute))
	if len(days) != 1 || days[0].start != ms(mid.AddDate(0, 0, -2)) || len(raw) != 1 || raw[0] != [2]int64{ms(mid.AddDate(0, 0, -1)), ms(mid.Add(time.Minute))} {
		t.Fatalf("a minute after midnight: days %v raw %v", days, raw)
	}
	days, _ = s.split(mid.AddDate(0, 0, -2), mid.Add(10*time.Minute), mid.Add(10*time.Minute))
	if len(days) != 2 {
		t.Fatalf("ten minutes after midnight: %d days kept, want 2", len(days))
	}
	from := mid.Add(-30 * time.Hour)
	days, raw = s.split(from, mid.Add(time.Hour), mid.Add(time.Hour))
	if len(days) != 1 || len(raw) != 2 || raw[0] != [2]int64{ms(from), ms(mid.AddDate(0, 0, -1))} {
		t.Fatalf("from inside a day: days %v raw %v", days, raw)
	}
}

// TestKeeper: Run counts the days of the view without filters ahead of
// any report, and Stop ends it.
func TestKeeper(t *testing.T) {
	s := dayStats(t, time.UTC, true)
	seedDays(t, s.db, time.UTC, time.Now(), 30)
	go s.Run()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		s.db.db.QueryRow(`SELECT COUNT(DISTINCT day) FROM hits_days`).Scan(&n)
		if n >= 25 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d days kept after 10 s", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("Stop did not return")
	}
}

// TestMidnight: a date begins at its first instant — the one before it
// is the day before — in zones whose clocks skip 00:00, show it twice,
// move by half an hour or make a day of 26 hours, every day of three
// years.
func TestMidnight(t *testing.T) {
	for _, zone := range []string{"UTC", "America/Santiago", "America/Havana", "Antarctica/Troll", "Australia/Lord_Howe",
		"Asia/Kathmandu", "Pacific/Chatham", "America/Los_Angeles", "Europe/London", "Asia/Tehran"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skip(err)
		}
		date := func(u time.Time) string { return u.In(loc).Format("2006-01-02") }
		for d := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC); d.Year() < 2028; d = d.AddDate(0, 0, 1) {
			m := midnight(d.Year(), d.Month(), d.Day(), loc)
			if date(m) != d.Format("2006-01-02") || date(m.Add(-time.Millisecond)) != d.AddDate(0, 0, -1).Format("2006-01-02") {
				t.Fatalf("%s %s: midnight %v (%s, the moment before %s)", zone, d.Format("2006-01-02"), m, date(m), date(m.Add(-time.Millisecond)))
			}
		}
	}
}

// TestDaysZoneMoved: a zone that moved keeps no day from before the
// move — those visitors were salted at the old zone's midnights, and a
// session that crossed the new one's is one session — so a report
// reads them from the hits, and reads the same as the hits.
func TestDaysZoneMoved(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 20)
	reportJSON(t, s, "30d", Filter{}, nil, now)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	s2, err := New(s.db.db, Options{Location: tokyo, Log: func(string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	if s2.since < now.UnixMilli() {
		t.Fatalf("since %d, want the move (%d on)", s2.since, now.UnixMilli())
	}
	for _, rg := range []string{"7d", "30d"} {
		if got, want := reportJSON(t, s2, rg, Filter{}, nil, now), rawly(t, s2, rg, Filter{}, nil, now); got != want {
			t.Fatalf("%s after the move:\n kept %s\n hits %s", rg, got, want)
		}
	}
	var n int
	if s2.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days`).Scan(&n); n != 0 {
		t.Fatalf("%d parts kept from before the move", n)
	}
	// reopened in the same zone, it is the same since
	s3, err := New(s.db.db, Options{Location: tokyo})
	if err != nil || s3.since != s2.since {
		t.Fatalf("reopened: since %d, want %d (%v)", s3.since, s2.since, err)
	}
}

// TestDaysLongDay: a late hit in a day of 26 hours (Troll, 2025-10-26)
// drops that day too.
func TestDaysLongDay(t *testing.T) {
	troll, err := time.LoadLocation("Antarctica/Troll")
	if err != nil {
		t.Skip(err)
	}
	day := midnight(2025, 10, 26, troll)
	next := midnight(2025, 10, 27, troll)
	if next.Sub(day) <= 25*time.Hour {
		t.Skipf("this tzdata's Troll day is %v", next.Sub(day))
	}
	s := dayStats(t, troll, false)
	now := time.Now()
	seedDays(t, s.db, troll, now, int(now.Sub(day).Hours()/24)+3)
	reportJSON(t, s, "12m", Filter{}, nil, now)
	late := day.Add(25*time.Hour + 30*time.Minute)
	if err := s.db.Add([]Hit{{TS: late.UnixMilli(), VID: "late", SID: "late", Entry: true, Path: "/late", Kind: "page", Channel: "direct"}}); err != nil {
		t.Fatal(err)
	}
	if got, want := reportJSON(t, s, "12m", Filter{}, nil, now), rawly(t, s, "12m", Filter{}, nil, now); got != want {
		t.Fatalf("a late hit in the long day:\n kept %s\n hits %s", got, want)
	}
}

// TestDaysDropping: while a write that drops kept days is on its way, a
// day counted is not kept, and a report that cannot read its days whole
// says so rather than leave them out; once it is done, the days are
// counted and the report is the hits'.
func TestDaysDropping(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 10)
	done := s.db.dropping()
	if _, err := s.build(spanOf("7d", now, time.UTC), Filter{}, nil, now); !errors.Is(err, errChanged) {
		t.Fatalf("a report while days drop: %v, want errChanged", err)
	}
	var n int
	if s.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days`).Scan(&n); n != 0 {
		t.Fatalf("%d parts kept while days dropped", n)
	}
	done()
	if got, want := reportJSON(t, s, "7d", Filter{}, nil, now), rawly(t, s, "7d", Filter{}, nil, now); got != want {
		t.Fatalf("after:\n kept %s\n hits %s", got, want)
	}
}

// TestDropViewChecksFilter: a view is dropped by its id and its filters
// both, so a trim that read the list late drops nothing.
func TestDropViewChecksFilter(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 5)
	reportJSON(t, s, "7d", Filter{Country: "JP"}, nil, now)
	vs, _ := s.db.filteredViews(context.Background())
	if len(vs) != 1 {
		t.Fatalf("%d filtered views", len(vs))
	}
	if err := s.db.dropView(context.Background(), vs[0].id, viewOf(Filter{Country: "US"})); err != nil {
		t.Fatal(err)
	}
	var n int
	if s.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days WHERE view = ?`, vs[0].id).Scan(&n); n == 0 {
		t.Fatal("another filter's drop took the view's days")
	}
}

// TestDaysRetryClean: a day gone between keep's look and its read is
// counted again, and the days the first read did find are not counted
// twice.
func TestDaysRetryClean(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 10)
	want := rawly(t, s, "7d", Filter{}, nil, now)
	reportJSON(t, s, "7d", Filter{}, nil, now)
	once := false
	beforeRead = func() {
		if !once {
			once = true
			s.db.db.Exec(`DELETE FROM hits_days WHERE day = (SELECT MAX(day) FROM hits_days)`)
		}
	}
	defer func() { beforeRead = nil }()
	if got := reportJSON(t, s, "7d", Filter{}, nil, now); !once || got != want {
		t.Fatalf("after a retry:\n kept %s\n hits %s", got, want)
	}
}

// TestLateJudgedUnderLock: a hit older than half of sealDelay, however
// it waited, drops the kept days it could fall in.
func TestLateJudgedUnderLock(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	g := s.db.gen.Load()
	if err := s.db.Add([]Hit{{TS: time.Now().Add(-sealDelay * 2 / 3).UnixMilli(), VID: "v", SID: "s", Path: "/", Kind: "page"}}); err != nil {
		t.Fatal(err)
	}
	if s.db.gen.Load() == g || s.db.busy.Load() != 0 {
		t.Fatalf("gen %d → %d, busy %d", g, s.db.gen.Load(), s.db.busy.Load())
	}
	g = s.db.gen.Load()
	if err := s.db.Add([]Hit{{TS: time.Now().UnixMilli(), VID: "v", SID: "s", Path: "/", Kind: "page"}}); err != nil {
		t.Fatal(err)
	}
	if s.db.gen.Load() != g {
		t.Fatal("a hit on time dropped kept days")
	}
}

// TestLocalMoved: two zones both called Local, alike in their offsets
// of a year long ago but not in their clock changes since, are two
// zones.
func TestLocalMoved(t *testing.T) {
	read := func(name string) *time.Location {
		b, err := os.ReadFile("/usr/share/zoneinfo/" + name)
		if err != nil {
			t.Skip(err)
		}
		loc, err := time.LoadLocationFromTZData("Local", b)
		if err != nil {
			t.Fatal(err)
		}
		return loc
	}
	chicago, mexico := read("America/Chicago"), read("America/Mexico_City")
	s := dayStats(t, chicago, false)
	if s.since != 0 {
		t.Fatalf("a first look: since %d", s.since)
	}
	again, err := New(s.db.db, Options{Location: read("America/Chicago")})
	if err != nil || again.since != 0 {
		t.Fatalf("the same Local again: since %d (%v)", again.since, err)
	}
	moved, err := New(s.db.db, Options{Location: mexico})
	if err != nil || moved.since == 0 {
		t.Fatalf("Local moved to Mexico City: since %d (%v)", moved.since, err)
	}
}

// TestStopWaitsForKeeper: Stop cancels a keeper that waits on a view's
// lock and returns only once it has returned.
func TestStopWaitsForKeeper(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	seedDays(t, s.db, time.UTC, time.Now(), 5)
	unlock, err := s.lockView(context.Background(), viewOf(Filter{}))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	go s.Run()
	time.Sleep(200 * time.Millisecond) // the keeper is waiting on the lock now
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return")
	}
	select {
	case <-s.kept:
	default:
		t.Fatal("Stop returned before the keeper did")
	}
}

// TestTidyWithoutRun: without Run, a report forgets the days before the
// horizon once a day.
func TestTidyWithoutRun(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 5)
	old := horizon(now, time.UTC).AddDate(0, 0, -10).UnixMilli()
	if _, err := s.db.db.Exec(`INSERT INTO hits_days (view, part, day, data) VALUES (1, 'summary', ?, x'')`, old); err != nil {
		t.Fatal(err)
	}
	reportJSON(t, s, "7d", Filter{}, nil, now)
	var n int
	if s.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days WHERE day = ?`, old).Scan(&n); n != 0 {
		t.Fatal("a day before the horizon is still kept")
	}
}

// TestDaysSweepEmpty: a sweep that leaves days kept empty before the
// first hit still standing does not stop a report.
func TestDaysSweepEmpty(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	day := func(i int) time.Time { n := now.UTC(); return midnight(n.Year(), n.Month(), n.Day()-i, time.UTC) }
	if err := s.db.Add([]Hit{
		{TS: day(10).Add(time.Hour).UnixMilli(), VID: "a", SID: "a", Entry: true, Path: "/", Kind: "page"},
		{TS: day(2).Add(time.Hour).UnixMilli(), VID: "b", SID: "b", Entry: true, Path: "/", Kind: "page"},
	}); err != nil {
		t.Fatal(err)
	}
	reportJSON(t, s, "30d", Filter{}, nil, now)
	if _, err := s.Sweep(day(5)); err != nil {
		t.Fatal(err)
	}
	if got, want := reportJSON(t, s, "30d", Filter{}, nil, now), rawly(t, s, "30d", Filter{}, nil, now); got != want {
		t.Fatalf("after the sweep:\n kept %s\n hits %s", got, want)
	}
}

// TestRulesAhead: the clock changes a zone had coming are kept, and a
// start that finds one of them since gone another way is a zone moved;
// a start that finds them as they were is not.
func TestRulesAhead(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip(err)
	}
	s := dayStats(t, la, false)
	now := time.Now()
	var stored string
	s.db.db.QueryRow(`SELECT v FROM hits_meta WHERE k = 'rules'`).Scan(&stored)
	if stored != rules(la, now.Add(rulesAhead)) && stored != rules(la, now.Add(rulesAhead-time.Minute)) {
		t.Fatalf("rules kept %q", stored)
	}
	if s2, err := New(s.db.db, Options{Location: la}); err != nil || s2.since != 0 {
		t.Fatalf("the same zone again: since %d (%v)", s2.since, err)
	}
	// the last change before now, as if it had been foreseen otherwise
	parts := strings.Fields(rules(la, now.Add(rulesAhead)))
	last := 0
	for i, p := range parts[1:] {
		at, _, _ := strings.Cut(p, ":")
		if n, _ := strconv.ParseInt(at, 10, 64); time.Unix(n, 0).Before(now) {
			last = i + 1
		}
	}
	// kept long ago, reaching only a year back: the changes since are not
	// the zone's moving
	cut := now.AddDate(-1, 0, 0)
	s.db.db.Exec(`UPDATE hits_meta SET v = ? WHERE k = 'rules'`, before(strings.Join(parts, " "), cut))
	s.db.db.Exec(`UPDATE hits_meta SET v = ? WHERE k = 'ruled'`, strconv.FormatInt(cut.UnixMilli(), 10))
	if s4, err := New(s.db.db, Options{Location: la}); err != nil || s4.since != 0 {
		t.Fatalf("rules kept to a year ago: since %d (%v)", s4.since, err)
	}
	at, _, _ := strings.Cut(parts[last], ":")
	parts[last] = at + ":12345"
	s.db.db.Exec(`UPDATE hits_meta SET v = ? WHERE k = 'rules'`, strings.Join(parts, " "))
	if s3, err := New(s.db.db, Options{Location: la}); err != nil || s3.since == 0 {
		t.Fatalf("a change gone another way: since %d (%v)", s3.since, err)
	}
}

// TestTidyFromToday: a host that never runs Run and only ever reads
// today still forgets the days before the horizon, once a day.
func TestTidyFromToday(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	old := horizon(now, time.UTC).AddDate(0, 0, -10).UnixMilli()
	if _, err := s.db.db.Exec(`INSERT INTO hits_days (view, part, day, data) VALUES (1, 'summary', ?, x'')`, old); err != nil {
		t.Fatal(err)
	}
	reportJSON(t, s, "today", Filter{}, nil, now)
	var n int
	if s.db.db.QueryRow(`SELECT COUNT(*) FROM hits_days WHERE day = ?`, old).Scan(&n); n != 0 {
		t.Fatal("a day before the horizon is still kept")
	}
}
