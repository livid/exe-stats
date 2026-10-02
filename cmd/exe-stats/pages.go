package main

import (
	_ "embed"

	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	stats "github.com/livid/exe-stats"
)

//go:embed pages.html
var pagesHTML string

//go:embed s.js
var scriptJS []byte

// GET /js/s.js — the script a site carries. It is the same for every
// site (the tag names the site), so an edge may keep it for a day.
func (a *App) handleScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.Write(scriptJS)
}

// What the pages say after a press: the form answers with a redirect
// home, and the word in the query picks the line.
var notes = map[string]string{
	"removed": "The site and its numbers are removed.",
	"picked":  "That site counts now.",
	"checked": "The holding was read again.",
	// a site's settings
	"hostadded":   "The host counts now.",
	"hostremoved": "That host no longer counts.",
	"badgeon":     "The badge is on: any page may show it.",
	"badgeoff":    "The badge is off. A page that still links it shows no picture there.",
	"ignored":     "That host is ignored: its views are no longer noted here.",
	"unignored":   "That host's views are noted again.",
}

var faults = map[string]string{
	"domain":  "That does not read as a domain: type it like example.com.",
	"exists":  "That site is already here.",
	"tier":    "This tier holds no more sites.",
	"short":   "No new sites while the balance is under the tier.",
	"hold":    "Adding a site needs a holding that covers a tier.",
	"rpc":     "The balance could not be read just now. Nothing has changed.",
	"wait":    "The holding was read a moment ago: try again in half a minute.",
	"gone":    "That site is not here.",
	"failed":  "That did not work. Nothing has changed.",
	"foreign": "Press the button on this site's own page.",
	// a site's settings
	"host":           "That does not read as a host: type it like blog.example.com, or *.example.com.",
	"counts":         "That host counts already.",
	"hostmax":        "A site lists at most 20 hosts.",
	"nohost":         "Choose a host in the list first.",
	"style":          "That is not one of the badge's styles.",
	"badgefailed":    "That did not work. Nothing has changed.",
	"ignorehost":     "That does not read as a host.",
	"ignorecounts":   "That host counts: its pages are the site's own.",
	"ignoremax":      "A site ignores at most 20 hosts. Take one off the list below first.",
	"ignorefailed":   "That did not work. Nothing has changed.",
	"noignored":      "Choose a host in the list first.",
	"unignorefailed": "That did not work. Nothing has changed.",
}

// saidIn is the settings section a word answers, by its word: the
// badge's own, the setup's for the note's Ignore, the ignored list's,
// and the hosts' for the rest.
func saidIn(word string) string {
	switch {
	case strings.HasPrefix(word, "badge") || word == "style":
		return "badge"
	case strings.HasPrefix(word, "unignore") || word == "noignored":
		return "ignored"
	case strings.HasPrefix(word, "ignore"):
		return "setup"
	}
	return "hosts"
}

type tierRow struct {
	Name, Hold, Sites, Views string
	On                       bool
}

type siteRow struct {
	ID, Domain string
	Counting   bool
	Icon       bool   // the site's favicon is kept (siteicon.go)
	Stamp      string // and its hash, in its URL
	Bars       []barView
	Today      string // the day's views, in words
	st         *stats.Stats
}

// barView is an hour of a site's day on its row: a box chartH high at
// the busiest hour, the hour still filling lighter, those to come empty.
// The live stream (homelive.go) sends the same.
type barView struct {
	H     int    `json:"h"`
	Class string `json:"c,omitempty"` // "" | "now" | "to"
	Title string `json:"t,omitempty"`
}

const chartH = 24

// today draws a site's day from its chart.
func (row *siteRow) today(now time.Time) error {
	pts, err := row.st.Today(now)
	if err != nil {
		return err
	}
	bars, views := drawDay(pts)
	row.Bars, row.Today = bars, dayLine(views, row.st.Online())
	return nil
}

// dayLine is the words beside a site's day: its views and who is on it
// now (a page in the last five minutes, as the desk's Live has it).
func dayLine(views string, online int) string {
	return views + " · " + commas(int64(online)) + " online"
}

// drawDay is a day's chart as its row draws it: its bars, and its views
// in words.
func drawDay(pts []stats.Point) ([]barView, string) {
	top, total, last := 0, 0, -1
	for i, p := range pts {
		top, total = max(top, p.Pageviews), total+p.Pageviews
		// an hour is placed by its middle, so in a zone off by half an
		// hour the views of the last half hour stand in the next bar:
		// that one is still filling, not to come
		if !p.Future || p.Pageviews > 0 {
			last = i
		}
	}
	bars := make([]barView, 0, len(pts))
	for i, p := range pts {
		b := barView{}
		if t, err := time.Parse(time.RFC3339, p.T); err == nil {
			b.Title = t.Format("15:04") + " · " + viewWords(p.Pageviews)
		}
		switch {
		case i == last:
			b.Class = "now"
		case i > last:
			b.Class, b.Title = "to", ""
		}
		if top > 0 && p.Pageviews > 0 {
			b.H = (p.Pageviews*chartH + top - 1) / top
		}
		bars = append(bars, b)
	}
	return bars, viewWords(total) + " today"
}

// viewWords is a count of page views in words: 1 view, 12,345 views.
func viewWords(n int) string {
	if n == 1 {
		return "1 view"
	}
	return commas(int64(n)) + " views"
}

type homeData struct {
	Host, Token, Mint string
	Tiers             []tierRow
	Signed            bool
	Addr, AddrShort   string
	Note, Fault       string

	Holding, TierName, TierLine string
	Used, Cap                   string
	Pct                         int
	Lines                       []string // what the account's state says, in the desk's words
	Checked                     string
	Sites                       []siteRow
	SitesLine                   string
	CanAdd                      bool
	AddWhy                      string
	NoTier                      bool
	GraceDays, OverFactor       int
}

func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// short is a count the way the tiers say it: 20K, 50M.
func short(n int64) string {
	switch {
	case n >= 1000000 && n%1000000 == 0:
		return strconv.FormatInt(n/1000000, 10) + "M"
	case n >= 1000 && n%1000 == 0:
		return strconv.FormatInt(n/1000, 10) + "K"
	}
	return commas(n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func (a *App) originOf(r *http.Request) string {
	if a.cfg.Origin != "" {
		return strings.TrimSuffix(a.cfg.Origin, "/")
	}
	return "http://" + r.Host
}

func (a *App) snippet(r *http.Request, id string) string {
	return `<script defer data-site="` + id + `" src="` + a.originOf(r) + `/js/s.js"></script>`
}

// GET / — signed out, what this is and the tiers; signed in, the
// account and its sites.
func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	d := homeData{Host: a.host(r), Token: a.cfg.Token, Mint: a.cfg.Mint, GraceDays: a.cfg.GraceDays, OverFactor: a.cfg.OverFactor}
	q := r.URL.Query()
	d.Note, d.Fault = notes[q.Get("m")], faults[q.Get("e")]
	addr := a.signedIn(r)
	tier := -1
	if addr != "" {
		a.mu.Lock()
		ac, err := a.acctOf(addr, now)
		if err != nil {
			a.mu.Unlock()
			a.log("home: %v", err)
			http.Error(w, "the account could not be read", http.StatusInternalServerError)
			return
		}
		tier = ac.Tier
		d.Signed, d.Addr, d.AddrShort = true, addr, addr[:4]+"…"+addr[len(addr)-4:]
		a.fill(&d, ac, r, now)
		a.mu.Unlock()
		for i := range d.Sites { // each site's day, read with the app unheld
			if row := &d.Sites[i]; row.st != nil {
				if err := row.today(now); err != nil {
					a.log("home %s: %v", row.ID, err)
				}
			}
		}
	}
	for i, t := range a.rules.Tiers {
		d.Tiers = append(d.Tiers, tierRow{Name: t.Name, Hold: commas(t.Hold), Sites: strconv.Itoa(t.Sites), Views: short(t.Views), On: i == tier})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := a.tmpl.ExecuteTemplate(w, "home", d); err != nil {
		a.log("home: %v", err)
	}
}

// fill writes the account's half of the page. The caller holds a.mu.
func (a *App) fill(d *homeData, ac *acct, r *http.Request, now time.Time) {
	ru := a.rules
	d.Holding = commas(ac.Balance/ru.Unit) + " " + a.cfg.Token
	if ac.Checked == 0 {
		d.Holding, d.Checked = "not read yet", "The holding has not been read yet: press Check Now."
	} else {
		d.Checked = "Holding read " + ago(now.Sub(time.UnixMilli(ac.Checked))) + ", and once a day"
	}
	slots := 0
	if ac.Tier >= 0 {
		t := ru.Tiers[ac.Tier]
		slots = t.Sites
		d.TierName = t.Name
		d.TierLine = plural(t.Sites, "site", "sites") + ", " + short(t.Views) + " views a month"
		limit, capped := ru.limit(ac.Tier, ac.prev)
		d.Used, d.Cap = commas(ac.cur.Views), commas(t.Views)
		d.Pct = int(min(100, ac.cur.Views*100/t.Views))
		switch ru.views(ac.Tier, ac.cur, ac.prev) {
		case StateOver:
			line := "Over the month's " + short(t.Views) + " views. Counting goes on up to " + short(limit) + " this month"
			if ac.Tier+1 < len(ru.Tiers) {
				line += "; the " + ru.Tiers[ac.Tier+1].Name + " tier would fit"
			}
			d.Lines = append(d.Lines, line+".")
		case StateCapped:
			d.Lines = append(d.Lines, "A second month over the cap: counting has stopped at "+short(t.Views)+
				" views until the month turns. A month under the cap, or a higher tier, lifts it.")
		default:
			if capped {
				d.Lines = append(d.Lines, "Last month went over the cap, so this month counting stops at "+short(t.Views)+" views.")
			} else if ac.cur.Views*100 >= t.Views*80 {
				d.Lines = append(d.Lines, "Past 80% of the month's views.")
			}
		}
	} else {
		d.NoTier = true
		d.TierName = "None"
		d.TierLine = "hold " + commas(ru.Tiers[0].Hold) + " " + a.cfg.Token + " for the " + ru.Tiers[0].Name + " tier"
		d.Used, d.Cap = commas(ac.cur.Views), "0"
	}
	switch holding(ac.Account, len(ac.sites)) {
	case StateShort:
		ends := ru.graceEnds(ac.Account)
		after := "counting stops"
		if ru.tierOf(ac.Balance) >= 0 {
			after = "the account moves to the " + ru.Tiers[ru.tierOf(ac.Balance)].Name + " tier"
		}
		d.Lines = append(d.Lines, fmt.Sprintf("The balance is %s %s under the %s tier. Everything counts until %s; then %s. Top up and press Check Now.",
			commas((ru.missing(ac.Account)+ru.Unit-1)/ru.Unit), a.cfg.Token, ru.Tiers[ac.Tier].Name, ends.UTC().Format("January 2, 15:04 UTC"), after))
	case StatePaused:
		d.Lines = append(d.Lines, "Paused: the balance covers no tier, so nothing is counted. Everything counted so far is still here to read. Top up and press Check Now.")
	}
	for i, s := range ac.sites {
		row := siteRow{ID: s.ID, Domain: s.Domain, Counting: i < slots}
		if e := a.sites[s.ID]; e != nil {
			row.Icon, row.Stamp = a.iconOf(e, now)
			if st, err := a.statsOf(e); err == nil {
				row.st = st
			} else {
				a.log("home %s: %v", s.ID, err)
			}
		}
		d.Sites = append(d.Sites, row)
	}
	if ac.Tier >= 0 && len(ac.sites) > slots {
		d.Lines = append(d.Lines, "This tier counts "+plural(slots, "site", "sites")+": the others rest. Press Count This One to choose.")
	}
	d.SitesLine = fmt.Sprintf("%d of %s", len(ac.sites), plural(slots, "site", "sites"))
	switch {
	case ac.Tier < 0:
		d.AddWhy = faults["hold"]
	case ac.ShortSince != 0:
		d.AddWhy = faults["short"]
	case len(ac.sites) >= slots:
		d.AddWhy = faults["tier"]
	default:
		d.CanAdd = true
	}
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	}
	return strconv.Itoa(int(d.Hours()/24)) + " days ago"
}

// back answers a form: home again, with the word that says what happened.
func back(w http.ResponseWriter, r *http.Request, key, word string) {
	http.Redirect(w, r, "/?"+key+"="+url.QueryEscape(word), http.StatusSeeOther)
}

// owner is the signed-in address of a request that may change
// something, or "" having answered it.
func (a *App) owner(w http.ResponseWriter, r *http.Request) string {
	if !a.sameOrigin(r) {
		back(w, r, "e", "foreign")
		return ""
	}
	addr := a.signedIn(r)
	if addr == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
	return addr
}

// POST /v1/check — Check Now, for someone who has just topped up.
func (a *App) handleCheck(w http.ResponseWriter, r *http.Request) {
	addr := a.owner(w, r)
	if addr == "" {
		return
	}
	now := time.Now()
	a.mu.Lock()
	recent := now.Sub(a.manual[addr]) < 30*time.Second
	if !recent {
		a.manual[addr] = now
	}
	a.mu.Unlock()
	if recent {
		back(w, r, "e", "wait")
		return
	}
	if err := a.checkNow(addr, false); err != nil {
		back(w, r, "e", "rpc")
		return
	}
	back(w, r, "m", "checked")
}

// POST /v1/sites — add a site: domain, and the owner's time zone for
// where its days begin.
func (a *App) handleAddSite(w http.ResponseWriter, r *http.Request) {
	addr := a.owner(w, r)
	if addr == "" {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	domain, ok := domainOf(r.FormValue("domain"))
	if !ok {
		back(w, r, "e", "domain")
		return
	}
	tz := r.FormValue("tz")
	if _, err := time.LoadLocation(tz); err != nil || tz == "" || tz == "Local" {
		tz = "UTC"
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	ac, err := a.acctOf(addr, now)
	if err != nil {
		back(w, r, "e", "failed")
		return
	}
	switch {
	case ac.Tier < 0:
		back(w, r, "e", "hold")
		return
	case ac.ShortSince != 0:
		back(w, r, "e", "short")
		return
	case len(ac.sites) >= a.rules.Tiers[ac.Tier].Sites:
		back(w, r, "e", "tier")
		return
	}
	s, err := a.store.addSite(addr, domain, tz, now)
	if err == errSiteExists {
		back(w, r, "e", "exists")
		return
	}
	if err != nil {
		a.log("add site: %v", err)
		back(w, r, "e", "failed")
		return
	}
	a.sites[s.ID] = &siteEntry{site: s}
	a.iconOf(a.sites[s.ID], now) // its icon, while its owner pastes the snippet
	if ac.sites, err = a.store.sitesOf(addr); err != nil {
		a.log("add site: %v", err)
	}
	// on to its settings: the snippet, and the check that waits for it
	http.Redirect(w, r, "/sites/"+s.ID+"/settings", http.StatusSeeOther)
}

// POST /v1/sites/{id}/remove — the site and its numbers, gone.
func (a *App) handleRemoveSite(w http.ResponseWriter, r *http.Request) {
	addr := a.owner(w, r)
	if addr == "" {
		return
	}
	id := r.PathValue("id")
	a.mu.Lock()
	defer a.mu.Unlock()
	ac, err := a.acctOf(addr, time.Now())
	if err != nil {
		back(w, r, "e", "failed")
		return
	}
	ok, err := a.store.removeSite(addr, id)
	if err != nil || !ok {
		back(w, r, "e", "gone")
		return
	}
	if e := a.sites[id]; e != nil {
		e.close()
		delete(a.sites, id)
	}
	a.removeFiles(id)
	ac.sites, _ = a.store.sitesOf(addr)
	back(w, r, "m", "removed")
}

// POST /v1/sites/{id}/pick — count this one, when the tier holds fewer
// sites than the account has.
func (a *App) handlePickSite(w http.ResponseWriter, r *http.Request) {
	addr := a.owner(w, r)
	if addr == "" {
		return
	}
	id := r.PathValue("id")
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	ac, err := a.acctOf(addr, now)
	if err != nil {
		back(w, r, "e", "failed")
		return
	}
	e := a.sites[id]
	if e == nil || e.site.Addr != addr {
		back(w, r, "e", "gone")
		return
	}
	if err := a.store.pickSite(addr, id, now); err != nil {
		back(w, r, "e", "failed")
		return
	}
	ac.sites, _ = a.store.sitesOf(addr)
	back(w, r, "m", "picked")
}

// GET /sites/{id} and /v1/sites/{id}/stats — a site's desk and its
// JSON, for its owner alone.
func (a *App) handleDesk(w http.ResponseWriter, r *http.Request) {
	addr := a.signedIn(r)
	if addr == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.mu.Lock()
	e := a.sites[r.PathValue("id")]
	if e == nil || e.site.Addr != addr {
		a.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	st, err := a.statsOf(e)
	a.mu.Unlock()
	if err != nil {
		a.log("desk: %v", err)
		http.Error(w, "the site's numbers could not be opened", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(r.URL.Path, "/v1/") {
		st.ServeJSON(w, r)
		return
	}
	st.ServePage(w, r)
}
