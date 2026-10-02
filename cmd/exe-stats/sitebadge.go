package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A site's own badge: /badge/{id}.gif, drawn from the site's numbers in
// the style its settings choose (counter.go draws the first). It is off
// until the owner chooses one, since anyone can read it: a site's
// numbers are its owner's to show.

// badgeStyles are the settings' choices, by the number sites.badge keeps.
var badgeStyles = []string{"None", "Hit Counter"}

// siteBadge is a site's drawn badge and the running count it is drawn
// from, under its own lock: one drawing at a time a site, and none of
// them holding the server's.
type siteBadge struct {
	mu    sync.Mutex
	tally tally
	style int
	at    time.Time
	gif   []byte
	etag  string
}

// badgeFresh is how long a drawing stands before a request draws again.
const badgeFresh = 10 * time.Second

// tally is a running count of a site's page views: N of them up to row
// ID, so each drawing counts only the rows since the last.
type tally struct{ N, ID int64 }

// counterOf reads a Hit Counter's numbers from a site's hits: every
// view since the site was added (crawlers kept out, as on the desk), and
// today's and yesterday's two-hour bars, from midnight in loc.
func counterOf(db *sql.DB, loc *time.Location, now time.Time, t *tally) (counterDay, error) {
	var n, id int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM hits WHERE id > ? AND bot = 0`, t.ID).Scan(&n, &id); err != nil {
		return counterDay{}, err
	}
	if id > 0 {
		t.N, t.ID = t.N+n, id
	}
	// the bars' edges, yesterday's twelve then today's, each on a whole
	// minute in every time zone; the views come by the minute
	now = now.In(loc)
	y, m, d := now.Date()
	var edges [25]int64
	for k := range edges {
		edges[k] = time.Date(y, m, d-1, 2*k, 0, 0, 0, loc).UnixMilli()
	}
	rows, err := db.Query(`SELECT ts / 60000, COUNT(*) FROM hits WHERE ts >= ? AND ts < ? AND bot = 0 GROUP BY 1`, edges[0], edges[24])
	if err != nil {
		return counterDay{}, err
	}
	defer rows.Close()
	var bars [24]int64
	for rows.Next() {
		var minute, c int64
		if err := rows.Scan(&minute, &c); err != nil {
			return counterDay{}, err
		}
		if k := sort.Search(25, func(k int) bool { return edges[k] > minute*60000 }) - 1; k >= 0 && k < 24 {
			bars[k] += c
		}
	}
	if err := rows.Err(); err != nil {
		return counterDay{}, err
	}
	day := counterDay{Total: t.N, Now: sort.Search(25, func(k int) bool { return edges[k] > now.UnixMilli() }) - 13}
	day.Now = min(max(day.Now, 0), 11)
	copy(day.Bars[:], bars[12:])
	for _, c := range bars {
		day.Top = max(day.Top, c)
	}
	return day, nil
}

// GET /badge/{id}.gif — a site's own badge while its owner has one on.
// A counter is read for being current, so nothing keeps it: a browser
// asks again on every page and gets a 304 while the drawing is the same,
// and the edge passes it through (stats.v2core.com has a Cache Rule that
// bypasses /badge/*.gif, since the zone's Browser Cache TTL would
// otherwise stretch any max-age to four hours). It is drawn afresh at
// most every ten seconds, which a burst of readers shares. A site
// without one is not found, and that is not kept either.
func (a *App) handleSiteBadge(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("file"), ".gif")
	a.mu.Lock()
	e := a.sites[id]
	var db *sql.DB
	var loc *time.Location
	style := 0
	if ok && e != nil && e.site.Badge > 0 {
		if st, err := a.statsOf(e); err == nil {
			db, loc, style = e.db, st.Collector().Location(), e.site.Badge
		} else {
			a.log("badge: %v", err)
		}
	}
	a.mu.Unlock()
	if db == nil {
		w.Header().Set("Cache-Control", "no-store")
		http.NotFound(w, r)
		return
	}
	b := &e.badge
	b.mu.Lock()
	now := time.Now()
	if b.gif == nil || b.style != style || now.Sub(b.at) >= badgeFresh {
		day, err := counterOf(db, loc, now, &b.tally)
		if err != nil {
			b.mu.Unlock()
			a.log("badge %s: %v", id, err)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "the badge could not be drawn", http.StatusInternalServerError)
			return
		}
		b.gif, b.style, b.at = drawCounter(day), style, now
		sum := sha256.Sum256(b.gif)
		b.etag = fmt.Sprintf(`"%x"`, sum[:8])
	}
	body, etag := b.gif, b.etag
	b.mu.Unlock()
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.Header().Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// POST /v1/sites/{id}/badge — the site's badge style; 0 takes it down.
func (a *App) handleBadgeStyle(w http.ResponseWriter, r *http.Request) {
	addr := a.owner(w, r)
	if addr == "" {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	style, err := strconv.Atoi(r.FormValue("style"))
	id := r.PathValue("id")
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.ownSite(addr, id)
	switch {
	case e == nil:
		back(w, r, "e", "gone")
		return
	case err != nil || style < 0 || style >= len(badgeStyles):
		backTo(w, r, id, "e", "style", "badge")
		return
	}
	if err := a.store.setBadge(id, style); err != nil {
		a.log("badge: %v", err)
		backTo(w, r, id, "e", "badgefailed", "badge")
		return
	}
	e.site.Badge = style
	if ac := a.accts[addr]; ac != nil {
		ac.sites, _ = a.store.sitesOf(addr)
	}
	word := "badgeoff"
	if style > 0 {
		word = "badgeon"
	}
	backTo(w, r, id, "m", word, "badge")
}

type styleRow struct {
	Value int
	Name  string
	On    bool
}

// badgeRows are the settings' pop-up, the site's style chosen.
func badgeRows(on int) []styleRow {
	out := make([]styleRow, len(badgeStyles))
	for i, s := range badgeStyles {
		out[i] = styleRow{i, s, i == on}
	}
	return out
}

// badgeLinks are the HTML and the Markdown that put a site's badge on a
// page, linking here: to the site's desk while it is public, else home.
// The link is tagged — the site, a badge, its style — so a click on it
// is a campaign of its own where it lands.
func (a *App) badgeLinks(r *http.Request, s Site) (src, html, md, alt string) {
	o := a.originOf(r)
	href := o + "/"
	if s.Public {
		href = o + "/sites/" + s.ID
	}
	style := badgeStyles[min(max(s.Badge, 0), len(badgeStyles)-1)]
	// a plain & in the HTML too: the code is read in a field before it is
	// pasted, and a browser leaves &utm_… as it is
	href += "?utm_source=" + url.QueryEscape(s.Domain) + "&utm_medium=badge&utm_campaign=" +
		url.QueryEscape(strings.ToLower(strings.ReplaceAll(style, " ", "-")))
	src = o + "/badge/" + s.ID + ".gif"
	alt = s.Domain + " page views, counted by exe-stats"
	html = `<a href="` + href + `"><img src="` + src + `" width="88" height="31" alt="` + alt + `" style="image-rendering: pixelated;"></a>`
	md = `[![` + alt + `](` + src + `)](` + href + `)`
	return
}
