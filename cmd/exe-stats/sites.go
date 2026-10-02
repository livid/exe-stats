package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	stats "github.com/livid/exe-stats"
)

// acct is the working copy of an account: its record, its sites in the
// order they count, and the month's views. The store has the same on
// disk; every change goes through App under a.mu and is written there.
type acct struct {
	Account
	sites     []Site
	cur, prev Usage
	dirty     bool // cur has views the store has not seen
}

// siteEntry is a site and, once something asked for it, its own
// analytics: a stats.Stats over the site's own SQLite file.
type siteEntry struct {
	site Site
	st   *stats.Stats
	db   *sql.DB

	saved      int64   // the site.Seen the store has
	backfilled bool    // site.Seen has been read from the site's own hits
	refused    refusal // the last beacon refused for this site, since the server started
	badge      siteBadge
}

func (e *siteEntry) close() {
	if e.st != nil {
		e.st.Stop()
		e.db.Close()
		e.st, e.db = nil, nil
	}
}

// acctOf is the working copy, read from the store the first time. The
// caller holds a.mu.
func (a *App) acctOf(addr string, now time.Time) (*acct, error) {
	if ac := a.accts[addr]; ac != nil {
		a.turnMonth(ac, now)
		return ac, nil
	}
	rec, err := a.store.account(addr, now)
	if err != nil {
		return nil, err
	}
	sites, err := a.store.sitesOf(addr)
	if err != nil {
		return nil, err
	}
	ac := &acct{Account: rec, sites: sites, cur: a.store.usage(addr, monthOf(now)), prev: a.store.usage(addr, prevMonthOf(now))}
	a.accts[addr] = ac
	return ac, nil
}

// turnMonth starts a new month's count when the calendar has moved on.
func (a *App) turnMonth(ac *acct, now time.Time) {
	if m := monthOf(now); ac.cur.Month != m {
		if ac.dirty {
			a.store.saveUsage(ac.Addr, ac.cur)
			ac.dirty = false
		}
		ac.cur, ac.prev = a.store.usage(ac.Addr, m), a.store.usage(ac.Addr, prevMonthOf(now))
	}
}

// counting says whether a site is among those its account's tier
// counts: the first Sites of them, in the account's order.
func (a *App) counting(ac *acct, id string) bool {
	if ac.Tier < 0 || ac.Tier >= len(a.rules.Tiers) {
		return false
	}
	for i, s := range ac.sites {
		if s.ID == id {
			return i < a.rules.Tiers[ac.Tier].Sites
		}
	}
	return false
}

func (a *App) flushUsage() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, ac := range a.accts {
		if ac.dirty {
			if err := a.store.saveUsage(ac.Addr, ac.cur); err != nil {
				a.log("usage: %v", err)
				continue
			}
			ac.dirty = false
		}
	}
}

// statsOf opens a site's analytics. The caller holds a.mu.
func (a *App) statsOf(e *siteEntry) (*stats.Stats, error) {
	if e.st != nil {
		return e.st, nil
	}
	db, err := openDB(filepath.Join(a.cfg.Data, "sites", e.site.ID+".db"))
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(e.site.TZ)
	if err != nil {
		loc = time.UTC
	}
	st, err := stats.New(db, stats.Options{Location: loc, Title: e.site.Domain, Log: a.log,
		Path: "/sites/" + e.site.ID, JSONPath: "/v1/sites/" + e.site.ID + "/stats", HomeURL: "/", HomeLabel: "Sites"})
	if err != nil {
		db.Close()
		return nil, err
	}
	go st.Run()
	e.st, e.db = st, db
	return st, nil
}

// ---- the collect address ----

// hostOf is a host as sites are named: lower case, no port, no www.
func hostOf(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimPrefix(strings.TrimSuffix(h, "."), "www.")
}

var domainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// domainOf reads what the owner typed — a name, or a whole address
// pasted from the browser — as the site's domain.
func domainOf(in string) (string, bool) {
	in = strings.TrimSpace(in)
	if strings.Contains(in, "://") {
		if u, err := url.Parse(in); err == nil {
			in = u.Host
		}
	} else if i := strings.IndexAny(in, "/?#"); i >= 0 {
		in = in[:i]
	}
	d := hostOf(in)
	return d, len(d) <= 253 && domainRE.MatchString(d)
}

func clientIP(r *http.Request) string { return stats.ClientIP(r) }

// The outcomes of a beacon, said in the X-Exe-Stats header so an owner
// checking a snippet can see in the browser's network panel why a page
// did or did not count.
const (
	beaconCounted  = "counted"
	beaconBad      = "unreadable"
	beaconUnknown  = "unknown site"
	beaconForeign  = "not this site's page"
	beaconOrigin   = "sent from another origin"
	beaconFast     = "slow down"
	beaconResting  = "site not counting"
	beaconFull     = "month full"
	beaconCrawler  = "crawler"
	beaconInternal = "not counted"
)

// POST /e — the script's beacon: {"s": site id, "u": the page's URL,
// "r": where the reader came from}. The body is sent as text/plain, so a
// browser asks no permission first, and the answer is the same 202
// whatever became of it.
func (a *App) handleCollect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "POST a beacon here", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("X-Exe-Stats", a.collect(r))
	w.WriteHeader(http.StatusAccepted)
}

func (a *App) collect(r *http.Request) string {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	var in struct{ S, U, R string }
	if err != nil || len(body) > 4096 || json.Unmarshal(body, &in) != nil || in.S == "" {
		return beaconBad
	}
	page, err := url.Parse(in.U)
	if err != nil || (page.Scheme != "https" && page.Scheme != "http") {
		return beaconBad
	}
	if !a.limit.allow("e:"+clientIP(r), 60, 1) {
		return beaconFast
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.sites[in.S]
	if e == nil {
		return beaconUnknown
	}
	// the page must be on one of the site's hosts, and a browser's
	// beacon comes from that page: a hit for somebody else's site id is
	// refused here
	host := hostOf(page.Host)
	if !e.site.counts(host) {
		e.refused = refusal{At: now.UnixMilli(), Why: beaconForeign, From: host}
		return beaconForeign
	}
	if o := r.Header.Get("Origin"); o != "" {
		if ou, err := url.Parse(o); err != nil || hostOf(ou.Host) != host {
			from := ""
			if err == nil {
				from = hostOf(ou.Host)
			}
			e.refused = refusal{At: now.UnixMilli(), Why: beaconOrigin, From: from, Page: host}
			return beaconOrigin
		}
	}
	// a view from the site's own page: the snippet is in place, whether
	// or not the view is counted
	e.site.Seen = now.UnixMilli()
	ac, err := a.acctOf(e.site.Addr, now)
	if err != nil {
		a.log("collect: %v", err)
		return beaconInternal
	}
	if !a.counting(ac, e.site.ID) {
		return beaconResting
	}
	st, err := a.statsOf(e)
	if err != nil {
		a.log("collect: %v", err)
		return beaconInternal
	}
	ref := in.R
	if e.site.ownRef(ref) {
		ref = ""
	}
	view := stats.PageView{Host: e.site.Domain, Path: e.site.pathOf(host, page.Path), Query: page.Query(), Referrer: ref}
	// a crawler that runs scripts is kept apart, as the desk keeps it,
	// and is not a view of the month
	if _, _, _, bot := stats.Classify(r.UserAgent()); bot {
		st.Collector().RecordPageView(r, "page", view)
		return beaconCrawler
	}
	cur, ok := a.rules.count(ac.Tier, ac.cur, ac.prev)
	if cur != ac.cur {
		ac.cur, ac.dirty = cur, true
	}
	if !ok {
		return beaconFull
	}
	st.Collector().RecordPageView(r, "page", view)
	return beaconCounted
}

// ---- balance checks ----

// checkNow reads the wallet's holding and applies it. A check the RPC
// did not answer changes nothing but when the next one is tried.
func (a *App) checkNow(addr string, scheduled bool) error {
	a.checkMu.Lock()
	defer a.checkMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := a.rpc.Balance(ctx, addr)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	ac, aerr := a.acctOf(addr, now)
	if aerr != nil {
		return aerr
	}
	day := (24 * time.Hour).Milliseconds()
	switch {
	case err != nil:
		if scheduled {
			ac.NextCheck = now.Add(time.Hour).UnixMilli()
		}
	default:
		ac.Account = a.rules.check(ac.Account, raw, now)
		if ac.NextCheck == 0 {
			ac.NextCheck = now.UnixMilli() + day // the hour of its own: the hour it first arrived
		}
	}
	if err == nil && scheduled {
		for ac.NextCheck <= now.UnixMilli() {
			ac.NextCheck += day
		}
	}
	if serr := a.store.saveAccount(ac.Account); serr != nil {
		a.log("account: %v", serr)
	}
	return err
}

// checkDue runs the daily checks that have come due.
func (a *App) checkDue(ctx context.Context, now time.Time) {
	due, err := a.store.due(now, 50)
	if err != nil {
		a.log("due: %v", err)
		return
	}
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		if err := a.checkNow(d.Addr, true); err != nil {
			a.log("check %s…: %v", d.Addr[:4], err)
		}
		time.Sleep(250 * time.Millisecond) // a public RPC counts requests too
	}
}

// removeFiles deletes a removed site's file and its WAL companions.
func (a *App) removeFiles(id string) {
	base := filepath.Join(a.cfg.Data, "sites", id+".db")
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		os.Remove(p)
	}
}

// ---- a small token bucket, by key ----

type limiter struct {
	mu sync.Mutex
	m  map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter() *limiter { return &limiter{m: map[string]*bucket{}} }

// allow takes one token from key's bucket of burst tokens refilled at
// rate a second.
func (l *limiter) allow(key string, burst, rate float64) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.m[key]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		l.m[key] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *limiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, b := range l.m {
		if now.Sub(b.last) > 10*time.Minute {
			delete(l.m, k)
		}
	}
}
