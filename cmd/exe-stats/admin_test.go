package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdmins: an admin reads every account — its page, its live numbers,
// its sites' desks and icons — and changes none of it; a wallet that is
// not one reads its own alone.
func TestAdmins(t *testing.T) {
	h := newHarness(t)
	owner, admin, other := newWallet(), newWallet(), newWallet()
	h.bal.set(owner.addr, 10000)
	for _, w := range []*wallet{owner, admin, other} {
		h.signIn(w)
	}
	h.form(owner, "/v1/sites", url.Values{"domain": {"shop.example"}})
	shop := h.siteID(owner, "shop.example")
	h.app.admins[admin.addr] = true

	// the owner's page, read as an admin: the account and its sites, and
	// no control that changes anything
	rec := h.do(admin, "GET", "/?as="+owner.addr, "", nil)
	page := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(page, ">shop.example</a>") || !strings.Contains(page, "Read as an admin") {
		t.Fatalf("an admin reading the owner's page: %d", rec.Code)
	}
	for _, control := range []string{`action="/v1/`, `/settings">Settings…`, `name="domain"`} {
		if strings.Contains(page, control) {
			t.Errorf("the page read as an admin carries %s", control)
		}
	}
	if !strings.Contains(page, `data-live="/v1/home/live?as=`+owner.addr+`"`) {
		t.Error("the page read as an admin follows its own stream, not the owner's account's")
	}
	// an address that never signed in is not made an account by reading it
	stranger := newWallet()
	if rec := h.do(admin, "GET", "/?as="+stranger.addr, "", nil); rec.Code != 404 || h.app.store.hasAccount(stranger.addr) {
		t.Fatalf("reading a stranger: %d, account made %v", rec.Code, h.app.store.hasAccount(stranger.addr))
	}
	// a wallet that is no admin reads its own page, whatever it asks
	if page := h.do(other, "GET", "/?as="+owner.addr, "", nil).Body.String(); strings.Contains(page, "shop.example") || strings.Contains(page, "Read as an admin") {
		t.Fatal("a wallet that is no admin read another's page")
	}
	// the desk: the owner's and the admin's, nobody else's
	for _, c := range []struct {
		w    *wallet
		want int
	}{{owner, 200}, {admin, 200}, {other, 404}} {
		if got := h.do(c.w, "GET", "/sites/"+shop, "", nil).Code; got != c.want {
			t.Errorf("the desk for %s: %d, want %d", c.w.addr[:4], got, c.want)
		}
	}
	// what changes something stays the owner's: an admin's press reaches no other account
	if loc := h.form(admin, "/v1/sites/"+shop+"/pick", url.Values{}); loc != "/?e=gone" {
		t.Fatalf("an admin's Count This One on the owner's site: %s", loc)
	}
	if rec := h.do(admin, "GET", "/sites/"+shop+"/settings", "", nil); rec.Code != 404 {
		t.Fatalf("the settings for an admin: %d", rec.Code)
	}
	// the admin's own page lists the accounts, each a link to read it
	if page := h.do(admin, "GET", "/", "", nil).Body.String(); !strings.Contains(page, `id="accounts"`) || !strings.Contains(page, `href="/?as=`+owner.addr+`"`) {
		t.Fatal("the admin's own page lists no accounts")
	}
	if page := h.do(owner, "GET", "/", "", nil).Body.String(); strings.Contains(page, `id="accounts"`) {
		t.Fatal("an owner's page lists the accounts")
	}

	// the owner's live numbers, followed by the admin
	srv := httptest.NewServer(h.srv)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/home/live?as="+owner.addr, nil)
	req.AddCookie(admin.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var first liveHome
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &first)
			break
		}
	}
	resp.Body.Close()
	if len(first.Sites) != 1 || first.Sites[0].ID != shop {
		t.Fatalf("the stream read as an admin: %+v", first)
	}

	// the config names admins by address, and refuses anything else
	db, err := openDB(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := newApp(Config{Data: t.TempDir(), Admins: []string{"not an address"}}, db, h.bal); err == nil {
		t.Fatal("a config with an admin that is no address was taken")
	}
	if app, err := newApp(Config{Data: t.TempDir(), Admins: []string{owner.addr}}, db, h.bal); err != nil || !app.isAdmin(owner.addr) || app.isAdmin(other.addr) || app.isAdmin("") {
		t.Fatalf("a config naming an admin: %v", err)
	}
}
