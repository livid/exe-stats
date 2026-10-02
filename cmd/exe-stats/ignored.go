package main

import (
	"net/http"
	"slices"
	"strings"
)

// Ignored hosts. A view refused for a site is noted under its setup
// (settings.go), with an Ignore button when the note names a host: a
// mirror, a scraper or a translator that carries the site's snippet is
// nothing its owner can fix, and its note would come back with each of
// its views. An ignored host's views are refused as before, and no
// longer noted; its settings list the ignored hosts, where one can be
// taken off.

// maxIgnored is how many hosts a site may ignore.
const maxIgnored = 20

// ignores says whether views from host (as hostOf reads it) go unnoted.
func (s Site) ignores(host string) bool { return slices.Contains(s.Ignored, host) }

// ignorable is the host a refusal names, when it is one the owner may
// ignore: not one that counts, since those pages are the site's own.
func (s Site) ignorable(rf refusal) string {
	if rf.From == "" || s.counts(rf.From) {
		return ""
	}
	return rf.From
}

// editIgnored runs one change to a site's ignored hosts for its owner,
// and writes it down. change returns the new list, or a fault word; the
// answer, done or failed when the store refused, comes back at anchor.
func (a *App) editIgnored(w http.ResponseWriter, r *http.Request, done, failed, anchor string, change func(Site, string) ([]string, string)) {
	addr := a.owner(w, r)
	if addr == "" {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	in := r.FormValue("host")
	id := r.PathValue("id")
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.ownSite(addr, id)
	if e == nil {
		back(w, r, "e", "gone")
		return
	}
	ignored, fault := change(e.site, in)
	if fault != "" {
		backTo(w, r, id, "e", fault, anchor)
		return
	}
	if err := a.store.setIgnored(id, ignored); err != nil {
		a.log("ignored: %v", err)
		backTo(w, r, id, "e", failed, anchor)
		return
	}
	e.site.Ignored = ignored
	if ac := a.accts[addr]; ac != nil {
		ac.sites, _ = a.store.sitesOf(addr)
	}
	backTo(w, r, id, "m", done, anchor)
}

// POST /v1/sites/{id}/ignore — the note's Ignore: stop noting a host.
func (a *App) handleIgnore(w http.ResponseWriter, r *http.Request) {
	a.editIgnored(w, r, "ignored", "ignorefailed", "setup-section", func(s Site, in string) ([]string, string) {
		h := hostOf(in)
		switch {
		case h == "" || len(h) > 253 || strings.ContainsAny(h, " \t\r\n/"):
			return nil, "ignorehost"
		case s.counts(h):
			return nil, "ignorecounts"
		case s.ignores(h):
			return s.Ignored, ""
		case len(s.Ignored) >= maxIgnored:
			return nil, "ignoremax"
		}
		return append(slices.Clone(s.Ignored), h), ""
	})
}

// POST /v1/sites/{id}/ignored/remove — note a host's views again.
func (a *App) handleUnignore(w http.ResponseWriter, r *http.Request) {
	a.editIgnored(w, r, "unignored", "unignorefailed", "ignored", func(s Site, in string) ([]string, string) {
		i := slices.Index(s.Ignored, in)
		if i < 0 {
			return nil, "noignored"
		}
		return slices.Delete(slices.Clone(s.Ignored), i, i+1), ""
	})
}
