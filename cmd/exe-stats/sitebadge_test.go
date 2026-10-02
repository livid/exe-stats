package main

import (
	"bytes"
	"database/sql"
	"image/gif"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	stats "github.com/livid/exe-stats"
)

// hitsAt writes people's page views at the given times, and a crawler's
// at the first, into a site's file.
func hitsAt(t *testing.T, db *sql.DB, at ...time.Time) {
	t.Helper()
	st, err := stats.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	var hs []stats.Hit
	for i, ts := range at {
		hs = append(hs, stats.Hit{TS: ts.UnixMilli(), VID: "v", SID: "s", Path: "/", Kind: "page"})
		if i == 0 {
			hs = append(hs, stats.Hit{TS: ts.UnixMilli(), VID: "b", SID: "b", Path: "/", Kind: "page", Bot: true})
		}
	}
	if err := st.Add(hs); err != nil {
		t.Fatal(err)
	}
}

// TestCounterOf: the wheels count every view since the site was added,
// crawlers aside, and each drawing reads only the rows since the last;
// the bars are today's two-hour counts from midnight where the site's
// days begin, scaled to the busiest two hours of today and yesterday.
func TestCounterOf(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "site.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, tokyo) }
	now := at(2, 13, 30)                                                                // the seventh bar, 12:00 to 14:00
	hitsAt(t, db, at(1, 0, 0), at(1, 9, 0), at(1, 9, 1), at(1, 9, 59), at(1, 10, 0))    // yesterday: 8:00 to 10:00 holds three
	hitsAt(t, db, at(2, 0, 0), at(2, 1, 59), at(2, 2, 0), at(2, 13, 29), at(2, 13, 29)) // today
	hitsAt(t, db, at(3, 0, 0))                                                          // and one ahead of the clock, not on today's bars
	var tl tally
	d, err := counterOf(db, tokyo, now, &tl)
	if err != nil {
		t.Fatal(err)
	}
	want := counterDay{Total: 11, Bars: [12]int64{2, 1, 0, 0, 0, 0, 2}, Now: 6, Top: 3}
	if d != want {
		t.Fatalf("counterOf = %+v\nwant %+v", d, want)
	}
	hitsAt(t, db, at(2, 13, 30))
	if d, _ = counterOf(db, tokyo, now.Add(time.Minute), &tl); d.Total != 12 || d.Bars[6] != 3 || tl.N != 12 {
		t.Fatalf("after one more view: %+v, tally %+v", d, tl)
	}
	// the same instant in UTC begins its days nine hours later
	var fresh tally
	if d, _ = counterOf(db, time.UTC, now, &fresh); d.Total != 12 || d.Now != 2 || d.Bars[2] != 3 {
		t.Fatalf("in UTC: %+v", d)
	}
}

// TestSiteBadge: a site's badge is off until its owner picks a style in
// its settings; then /badge/{id}.gif is its Hit Counter, asked again
// on every page and answered 304 while it is the same, and the settings hand out the HTML and the Markdown that show
// it. Off again, it is gone, and that is not kept.
func TestSiteBadge(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}, "tz": {"Asia/Tokyo"}})
	id := h.siteID(w, "shop.example")
	gifAt := "/badge/" + id + ".gif"
	settings := "/sites/" + id + "/settings"

	off := func(why string) {
		t.Helper()
		rec := h.do(nil, "GET", gifAt, "", nil)
		if rec.Code != http.StatusNotFound || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %s answered %d, kept as %q", why, gifAt, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	off("a new site")
	page := h.do(w, "GET", settings, "", nil).Body.String()
	if !strings.Contains(page, `<option value="0" selected>None</option><option value="1">Hit Counter</option>`) || strings.Contains(page, `id="badge-html"`) {
		t.Fatal("a new site's settings do not say the badge is off")
	}

	// only the owner, from this site's own page, with a style there is
	stranger := newWallet()
	h.signIn(stranger)
	if loc := h.form(stranger, "/v1/sites/"+id+"/badge", url.Values{"style": {"1"}}); loc != "/?e=gone" {
		t.Fatalf("a stranger's style: %s", loc)
	}
	if rec := h.do(w, "POST", "/v1/sites/"+id+"/badge", "style=1", map[string]string{"Origin": "https://evil.example",
		"Content-Type": "application/x-www-form-urlencoded"}); rec.Header().Get("Location") != "/?e=foreign" {
		t.Fatalf("a style posted from another site: %s", rec.Header().Get("Location"))
	}
	for _, bad := range []string{"2", "-1", "x"} {
		if loc := h.form(w, "/v1/sites/"+id+"/badge", url.Values{"style": {bad}}); loc != settings+"?e=style#badge" {
			t.Fatalf("style %q: %s", bad, loc)
		}
	}
	off("refused styles")

	if loc := h.form(w, "/v1/sites/"+id+"/badge", url.Values{"style": {"1"}}); loc != settings+"?m=badgeon#badge" {
		t.Fatalf("Hit Counter: %s", loc)
	}
	page = h.do(w, "GET", settings+"?m=badgeon", "", nil).Body.String()
	src := "https://stats.example/badge/" + id + ".gif"
	alt := "shop.example page views, counted by exe-stats"
	for _, want := range []string{
		`<option value="1" selected>Hit Counter</option>`,
		`id="badge-html" readonly value="&lt;a href=&#34;https://stats.example/&#34;&gt;&lt;img src=&#34;` + src + `&#34; width=&#34;88&#34; height=&#34;31&#34; alt=&#34;` + alt + `&#34;&gt;&lt;/a&gt;"`,
		`id="badge-md" readonly value="[![` + alt + `](` + src + `)](https://stats.example/)"`,
		`id="badge-said" role="status">The badge is on: any page may show it.</p>`,
		`id="hosts-said" role="status"></p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings do not carry %s", want)
		}
	}

	// two views today, and the badge stands on them, its lamp breathing
	h.app.mu.Lock()
	e := h.app.sites[id]
	h.app.statsOf(e)
	db := e.db
	h.app.mu.Unlock()
	hitsAt(t, db, time.Now().Add(-time.Minute), time.Now())
	rec := h.do(nil, "GET", gifAt, "", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/gif" || rec.Header().Get("Cache-Control") != "no-cache" ||
		rec.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Fatalf("%s answered %d, %v", gifAt, rec.Code, rec.Header())
	}
	g, err := gif.DecodeAll(bytes.NewReader(rec.Body.Bytes()))
	if err != nil || g.Config.Width != 88 || g.Config.Height != 31 || len(g.Image) < 2 || g.LoopCount != 0 {
		t.Fatalf("the badge: %v, %d frames", err, len(g.Image))
	}
	if e.badge.tally.N != 2 {
		t.Errorf("the wheels count %d, want 2", e.badge.tally.N)
	}
	// asked again with what it has, it is the same badge
	if again := h.do(nil, "GET", gifAt, "", map[string]string{"If-None-Match": rec.Header().Get("ETag")}); again.Code != http.StatusNotModified {
		t.Errorf("asked again with its ETag: %d", again.Code)
	}
	if rec := h.do(nil, "GET", "/badge/"+id+".png", "", nil); rec.Code != 404 {
		t.Errorf("the badge by another name: %d", rec.Code)
	}

	if loc := h.form(w, "/v1/sites/"+id+"/badge", url.Values{"style": {"0"}}); loc != settings+"?m=badgeoff#badge" {
		t.Fatalf("None: %s", loc)
	}
	off("taken down")
	// and the choice is written down, for the next start to read
	h.form(w, "/v1/sites/"+id+"/badge", url.Values{"style": {"1"}})
	if all, _ := h.app.store.allSites(); len(all) != 1 || all[0].Badge != 1 {
		t.Errorf("the store has %+v", all)
	}
}
