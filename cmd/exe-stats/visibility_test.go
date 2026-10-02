package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestVisibility: a new site's desk is private — its owner's, and an
// admin's — until the owner makes it public in its settings; then anyone
// reads the desk and its JSON, signed in or not, while its settings and
// buttons stay the owner's. Private again, it closes again.
func TestVisibility(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}, "tz": {"Asia/Tokyo"}})
	id := h.siteID(w, "shop.example")
	desk, json, settings := "/sites/"+id, "/v1/sites/"+id+"/stats", "/sites/"+id+"/settings"
	stranger := newWallet()
	h.signIn(stranger)

	closed := func(why string) {
		t.Helper()
		for _, p := range []string{desk, json} {
			if rec := h.do(nil, "GET", p, "", nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
				t.Errorf("%s: a signed-out read of %s = %d %s", why, p, rec.Code, rec.Header().Get("Location"))
			}
			if rec := h.do(stranger, "GET", p, "", nil); rec.Code != http.StatusNotFound {
				t.Errorf("%s: a stranger's read of %s = %d", why, p, rec.Code)
			}
		}
		if rec := h.do(w, "GET", desk, "", nil); rec.Code != 200 {
			t.Errorf("%s: the owner's read of the desk = %d", why, rec.Code)
		}
	}
	closed("a new site")
	page := h.do(w, "GET", settings, "", nil).Body.String()
	if !strings.Contains(page, `<option value="private" selected>Private</option><option value="public">Public</option>`) ||
		strings.Contains(page, `id="desk-link"`) {
		t.Fatal("a new site's settings do not say its desk is private")
	}

	// only the owner, from this site's own page, with a choice there is
	if loc := h.form(stranger, "/v1/sites/"+id+"/visibility", url.Values{"visibility": {"public"}}); loc != "/?e=gone" {
		t.Fatalf("a stranger's choice: %s", loc)
	}
	if rec := h.do(w, "POST", "/v1/sites/"+id+"/visibility", "visibility=public", map[string]string{"Origin": "https://evil.example",
		"Content-Type": "application/x-www-form-urlencoded"}); rec.Header().Get("Location") != "/?e=foreign" {
		t.Fatalf("a choice posted from another site: %s", rec.Header().Get("Location"))
	}
	for _, bad := range []string{"", "Public", "1", "unlisted"} {
		if loc := h.form(w, "/v1/sites/"+id+"/visibility", url.Values{"visibility": {bad}}); loc != settings+"?e=visibility#visibility" {
			t.Fatalf("visibility %q: %s", bad, loc)
		}
	}
	closed("refused choices")

	if loc := h.form(w, "/v1/sites/"+id+"/visibility", url.Values{"visibility": {"public"}}); loc != settings+"?m=public#visibility" {
		t.Fatalf("Public: %s", loc)
	}
	page = h.do(w, "GET", settings+"?m=public", "", nil).Body.String()
	for _, want := range []string{
		`<option value="private">Private</option><option value="public" selected>Public</option>`,
		`id="desk-link" readonly value="https://stats.example/sites/` + id + `"`,
		`id="visibility-said" role="status">The desk is public: anyone with its address can read it.</p>`,
		`id="badge-said" role="status"></p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings do not carry %s", want)
		}
	}
	if sites, _ := h.app.store.sitesOf(w.addr); len(sites) != 1 || !sites[0].Public {
		t.Fatalf("the choice is not written down: %+v", sites)
	}
	for _, who := range []*wallet{nil, stranger, w} {
		for _, p := range []string{desk + "?range=24h", json + "?range=24h"} {
			if rec := h.do(who, "GET", p, "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "shop.example") && !strings.HasPrefix(p, "/v1/") {
				t.Errorf("a public desk read at %s = %d", p, rec.Code)
			}
		}
	}
	// what changes something, or is the owner's own, stays the owner's
	for _, p := range []string{settings, "/v1/sites/" + id + "/setup"} {
		if rec := h.do(stranger, "GET", p, "", nil); rec.Code != http.StatusNotFound {
			t.Errorf("a stranger's read of a public site's %s = %d", p, rec.Code)
		}
	}
	if loc := h.form(stranger, "/v1/sites/"+id+"/visibility", url.Values{"visibility": {"private"}}); loc != "/?e=gone" {
		t.Fatalf("a stranger closing a public desk: %s", loc)
	}

	if loc := h.form(w, "/v1/sites/"+id+"/visibility", url.Values{"visibility": {"private"}}); loc != settings+"?m=private#visibility" {
		t.Fatalf("Private: %s", loc)
	}
	closed("private again")
	if page = h.do(w, "GET", settings+"?m=private", "", nil).Body.String(); strings.Contains(page, `id="desk-link"`) ||
		!strings.Contains(page, `id="visibility-said" role="status">The desk is private: only you can read it.</p>`) {
		t.Fatal("the settings still hand out a private desk's address")
	}
}
