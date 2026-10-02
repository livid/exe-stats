package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	stats "github.com/livid/exe-stats"
)

// The homepage, live. GET /v1/home/live is a stream of server-sent
// events for the signed-in owner: "home", each site's day — its bars and
// its views — and the month's count, sent when they change, looked at
// every liveTick; and "ping" every livePing, which keeps the stream open
// through the edge and tells the page it still lives (it opens another
// after a minute without one, as the hub's pages do). A site's day is
// read once a tick however many pages are open on it.

var (
	liveTick = 2 * time.Second
	livePing = 25 * time.Second
)

// liveHome is one "home" event.
type liveHome struct {
	Sites []liveSite `json:"sites"`
	Used  string     `json:"used"` // the month's counted views
	Pct   int        `json:"pct"`  // of the tier's
}

type liveSite struct {
	ID    string    `json:"id"`
	Today string    `json:"today"`
	Views int       `json:"v"` // today's, the number in Today: the page counts up to it as its dots land
	Bars  []barView `json:"bars"`
}

// siteDay is a site's day as last drawn — its bars, and its views and
// who is online in words — for every stream and page that asks within a
// tick; and the day before, which the bars stand over.
type siteDay struct {
	mu     sync.Mutex
	at     time.Time
	bars   []barView
	today  string
	views  int
	ago    []stats.Point // yesterday's chart
	agoAt  time.Time     // read then
	agoFor string        // for the day that began then (today's first hour)
}

// dayOf is a site's day, drawn afresh when the one held is older than
// most of a tick. Yesterday is over, so it is read again only a minute
// on (a hit may still be on its way past midnight) or on a new day.
func dayOf(d *siteDay, st *stats.Stats, now time.Time) ([]barView, string, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.at.IsZero() && now.Sub(d.at) < liveTick*3/4 && now.After(d.at) {
		return d.bars, d.today, d.views, nil
	}
	pts, err := st.Today(now)
	if err != nil {
		return nil, "", 0, err
	}
	day := ""
	if len(pts) > 0 {
		day = pts[0].T
	}
	if d.ago == nil || d.agoFor != day || now.Sub(d.agoAt) > time.Minute || !now.After(d.agoAt) {
		ago, err := st.Yesterday(now)
		if err != nil {
			return nil, "", 0, err
		}
		d.ago, d.agoAt, d.agoFor = ago, now, day
	}
	bars, views := drawDay(pts, d.ago)
	d.bars, d.today, d.views = bars, dayLine(viewWords(views)+" today", st.Online()), views
	d.at = now
	return d.bars, d.today, d.views, nil
}

// homeNow is what the stream says of an owner's account now.
func (a *App) homeNow(addr string, now time.Time) (liveHome, error) {
	type one struct {
		id  string
		day *siteDay
		st  *stats.Stats
	}
	var h liveHome
	var all []one
	a.mu.Lock()
	ac, err := a.acctOf(addr, now)
	if err != nil {
		a.mu.Unlock()
		return h, err
	}
	h.Used = commas(ac.cur.Views)
	if ac.Tier >= 0 && ac.Tier < len(a.rules.Tiers) {
		h.Pct = int(min(100, ac.cur.Views*100/a.rules.Tiers[ac.Tier].Views))
	}
	for _, s := range ac.sites {
		if e := a.sites[s.ID]; e != nil {
			if st, err := a.statsOf(e); err == nil {
				all = append(all, one{s.ID, &e.day, st})
			}
		}
	}
	a.mu.Unlock()
	h.Sites = []liveSite{}
	for _, o := range all { // with the app let go
		bars, today, views, err := dayOf(o.day, o.st, now)
		if err != nil {
			continue // removed meanwhile, its file closed
		}
		h.Sites = append(h.Sites, liveSite{ID: o.id, Today: today, Views: views, Bars: bars})
	}
	return h, nil
}

// GET /v1/home/live — the homepage's numbers as they change.
func (a *App) handleHomeLive(w http.ResponseWriter, r *http.Request) {
	addr := a.signedIn(r)
	if addr == "" {
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // the server's write timeout is for pages, not a stream
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	io.WriteString(w, "retry: 3000\n\n")
	var last []byte
	send := func() bool {
		h, err := a.homeNow(addr, time.Now())
		if err != nil {
			return true
		}
		b, _ := json.Marshal(h)
		if bytes.Equal(b, last) {
			return true
		}
		last = b
		fmt.Fprintf(w, "event: home\ndata: %s\n\n", b)
		return rc.Flush() == nil
	}
	if !send() {
		return
	}
	tick, ping := time.NewTicker(liveTick), time.NewTicker(livePing)
	defer tick.Stop()
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.done:
			return
		case <-tick.C:
			if !send() {
				return
			}
		case <-ping.C:
			io.WriteString(w, "event: ping\ndata: {}\n\n")
			if rc.Flush() != nil {
				return
			}
		}
	}
}
