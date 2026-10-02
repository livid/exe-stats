package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// A site's settings page, /sites/{id}/settings, and the first thing on
// it: the setup check — whether the snippet has reported a view from the
// site's own pages, and if a view was refused, why.

// refusal is a beacon refused for a site, kept in memory to say why on
// the site's settings page.
type refusal struct {
	At   int64  // ms; 0: none since the server started
	Why  string // beaconForeign or beaconOrigin
	From string // the host the view came from; "" when the origin was unreadable
	Page string // for another origin: the host of the page the view claimed
}

// setupView is the setup check in a line: OK once the snippet has
// reported from the site's own pages, counted or not.
type setupView struct {
	OK   bool   `json:"ok"`
	Line string `json:"line"`
}

// seenOf is when the site's snippet last reported. A site that counted
// before the server kept this has it read once from its own hits. The
// caller holds a.mu.
func (a *App) seenOf(e *siteEntry) int64 {
	if e.site.Seen == 0 && !e.backfilled {
		e.backfilled = true
		if _, err := os.Stat(filepath.Join(a.cfg.Data, "sites", e.site.ID+".db")); err == nil {
			if _, err := a.statsOf(e); err == nil {
				var ms int64
				if err := e.db.QueryRow(`SELECT COALESCE(MAX(ts), 0) FROM hits`).Scan(&ms); err != nil {
					a.log("seen: %v", err)
				}
				e.site.Seen = max(e.site.Seen, ms)
			}
		}
	}
	return e.site.Seen
}

// setupNote is a view refused while the snippet works, in a line, and
// the host it names when the owner may ignore it.
type setupNote struct{ Line, Host string }

// setupOf is the check, and a note when the snippet works but a view
// was refused in the last day. The caller holds a.mu.
func (a *App) setupOf(e *siteEntry, now time.Time) (setupView, setupNote) {
	seen, rf := a.seenOf(e), e.refused
	switch {
	case rf.Why == beaconForeign && e.site.counts(rf.From):
		rf = refusal{} // its host has been listed since: no reason any more
	case rf.From != "" && e.site.ignores(rf.From):
		rf = refusal{} // its host has been ignored since
	}
	why := ""
	if rf.At != 0 {
		when := ago(now.Sub(time.UnixMilli(rf.At)))
		switch {
		case rf.Why == beaconForeign:
			why = fmt.Sprintf("A view came from %s %s and was not counted: it is not one of the hosts below.", rf.From, when)
		case rf.From != "":
			why = fmt.Sprintf("A view of %s was sent from %s %s, and was not counted.", rf.Page, rf.From, when)
		default:
			why = fmt.Sprintf("A view of %s was sent from another origin %s, and was not counted.", rf.Page, when)
		}
	}
	if seen == 0 {
		if why != "" {
			return setupView{Line: why}, setupNote{}
		}
		return setupView{Line: "Waiting for the first page view. Paste the snippet into the site's head, then open a page."}, setupNote{}
	}
	v := setupView{OK: true, Line: "The snippet works: the last page view came " + ago(now.Sub(time.UnixMilli(seen))) + "."}
	if rf.At == 0 || now.Sub(time.UnixMilli(rf.At)) > 24*time.Hour {
		return v, setupNote{}
	}
	return v, setupNote{Line: why, Host: e.site.ignorable(rf)}
}

// flushSeen writes down the sites whose snippet has reported since the
// store last heard: a first report at once, later ones to the minute
// (the page says "3 min ago"), and everything when all is set.
func (a *App) flushSeen(all bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.sites {
		d := e.site.Seen - e.saved
		if d <= 0 || !(all || e.saved == 0 || d >= time.Minute.Milliseconds()) {
			continue
		}
		if err := a.store.saveSeen(e.site.ID, e.site.Seen); err != nil {
			a.log("seen: %v", err)
			continue
		}
		e.saved = e.site.Seen
	}
}

// ownSite is the signed-in owner's site by id, or nil. The caller holds
// a.mu.
func (a *App) ownSite(addr, id string) *siteEntry {
	if e := a.sites[id]; e != nil && e.site.Addr == addr {
		return e
	}
	return nil
}

type settingsData struct {
	Host, ID, Domain, Snippet, TZ string
	Counting                      bool
	Setup                         setupView
	Note                          setupNote // a view refused while the snippet works
	Hosts                         []hostRow // besides the domain
	Ignored                       []string  // hosts whose refused views go unnoted
	Said, Fault                   string    // what the last press did
	SaidIn                        string    // and the section it says it in
	Badge                         int       // the badge's style; 0 none
	Styles                        []styleRow
	BadgeSrc, BadgeHTML, BadgeMD  string
	BadgeAlt                      string
}

// GET /sites/{id}/settings — a site's settings, for its owner alone.
func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	addr := a.signedIn(r)
	if addr == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	now := time.Now()
	a.mu.Lock()
	e := a.ownSite(addr, r.PathValue("id"))
	if e == nil {
		a.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	ac, err := a.acctOf(addr, now)
	if err != nil {
		a.mu.Unlock()
		a.log("settings: %v", err)
		http.Error(w, "the account could not be read", http.StatusInternalServerError)
		return
	}
	d := settingsData{Host: a.host(r), ID: e.site.ID, Domain: e.site.Domain, Snippet: a.snippet(r, e.site.ID),
		TZ: e.site.TZ, Counting: a.counting(ac, e.site.ID)}
	d.Setup, d.Note = a.setupOf(e, now)
	d.Hosts, d.Ignored = hostRows(e.site), e.site.Ignored
	d.Badge, d.Styles = e.site.Badge, badgeRows(e.site.Badge)
	d.BadgeSrc, d.BadgeHTML, d.BadgeMD, d.BadgeAlt = a.badgeLinks(r, e.site)
	a.mu.Unlock()
	q := r.URL.Query()
	d.Said, d.Fault = notes[q.Get("m")], faults[q.Get("e")]
	d.SaidIn = saidIn(q.Get("m") + q.Get("e"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := a.tmpl.ExecuteTemplate(w, "settings", d); err != nil {
		a.log("settings: %v", err)
	}
}

// GET /v1/sites/{id}/setup — the check alone, which the settings page
// asks again while it waits for the first view.
func (a *App) handleSetup(w http.ResponseWriter, r *http.Request) {
	addr := a.signedIn(r)
	if addr == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.mu.Lock()
	e := a.ownSite(addr, r.PathValue("id"))
	if e == nil {
		a.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	v, _ := a.setupOf(e, time.Now())
	a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
