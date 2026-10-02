package main

import (
	"net/http"
	"strings"
)

// A site's visibility: who may read its desk. Private, the default,
// keeps it to its owner (and the admins); Public opens /sites/{id} and
// its JSON to anyone with the address, signed in or not. Nothing else
// opens with it — the settings, the icon and the home page's live
// numbers stay the owner's — and robots.txt still asks crawlers to keep
// out of /sites/.

// visibilities are the settings' pop-up, Private first: the order a
// site's choice is read in.
var visibilities = []string{"Private", "Public"}

type visibilityRow struct {
	Value, Name string
	On          bool
}

// visibilityRows are the pop-up, the site's choice chosen.
func visibilityRows(public bool) []visibilityRow {
	out := make([]visibilityRow, len(visibilities))
	for i, v := range visibilities {
		out[i] = visibilityRow{strings.ToLower(v), v, (i == 1) == public}
	}
	return out
}

// POST /v1/sites/{id}/visibility — private or public.
func (a *App) handleVisibility(w http.ResponseWriter, r *http.Request) {
	addr := a.owner(w, r)
	if addr == "" {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	v := r.FormValue("visibility")
	id := r.PathValue("id")
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.ownSite(addr, id)
	switch {
	case e == nil:
		back(w, r, "e", "gone")
		return
	case v != "private" && v != "public":
		backTo(w, r, id, "e", "visibility", "visibility")
		return
	}
	public := v == "public"
	if err := a.store.setPublic(id, public); err != nil {
		a.log("visibility: %v", err)
		backTo(w, r, id, "e", "visibilityfailed", "visibility")
		return
	}
	e.site.Public = public
	if ac := a.accts[addr]; ac != nil {
		ac.sites, _ = a.store.sitesOf(addr)
	}
	backTo(w, r, id, "m", v, "visibility")
}
