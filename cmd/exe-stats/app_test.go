package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	stats "github.com/livid/exe-stats"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// balances is the chain for a test: what each address holds, or an RPC
// that does not answer.
type balances struct {
	mu   sync.Mutex
	m    map[string]int64
	down bool
}

func (b *balances) Balance(_ context.Context, addr string) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return 0, errRPC
	}
	return b.m[addr], nil
}

func (b *balances) set(addr string, tokens int64) {
	b.mu.Lock()
	b.m[addr] = tokens * tok
	b.mu.Unlock()
}

const chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"

type harness struct {
	t   *testing.T
	app *App
	srv http.Handler
	bal *balances
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	bal := &balances{m: map[string]int64{}}
	app, err := newApp(Config{Data: dir, Origin: "https://stats.example"}, db, bal)
	if err != nil {
		t.Fatal(err)
	}
	app.log = t.Logf
	if err := mkdirSites(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.close(); db.Close() })
	return &harness{t, app, app.routes(), bal}
}

// wallet is a key that signs in the way a Solana wallet does.
type wallet struct {
	addr   string
	key    ed25519.PrivateKey
	cookie *http.Cookie
}

func newWallet() *wallet {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	return &wallet{addr: base58Encode(pub), key: key}
}

func (h *harness) do(w *wallet, method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("User-Agent", chromeUA)
	if method == "POST" {
		r.Header.Set("Origin", "https://stats.example")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	if w != nil && w.cookie != nil {
		r.AddCookie(w.cookie)
	}
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, r)
	return rec
}

func (h *harness) form(w *wallet, target string, v url.Values) string {
	rec := h.do(w, "POST", target, v.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if rec.Code != http.StatusSeeOther {
		h.t.Fatalf("POST %s = %d %s", target, rec.Code, rec.Body)
	}
	return rec.Header().Get("Location")
}

func (h *harness) signIn(w *wallet) {
	h.t.Helper()
	rec := h.do(nil, "GET", "/v1/challenge?address="+w.addr, "", nil)
	var c struct{ Message string }
	json.Unmarshal(rec.Body.Bytes(), &c)
	if rec.Code != 200 || !strings.HasPrefix(c.Message, "stats.example wants you to sign in with your Solana account:\n"+w.addr+"\n") {
		h.t.Fatalf("challenge = %d %q", rec.Code, rec.Body)
	}
	body, _ := json.Marshal(map[string]string{"address": w.addr, "message": c.Message,
		"signature": base64.StdEncoding.EncodeToString(ed25519.Sign(w.key, []byte(c.Message)))})
	rec = h.do(nil, "POST", "/v1/signin", string(body), nil)
	if rec.Code != 200 || len(rec.Result().Cookies()) != 1 {
		h.t.Fatalf("sign in = %d %s", rec.Code, rec.Body)
	}
	w.cookie = rec.Result().Cookies()[0]
	if !w.cookie.HttpOnly || !w.cookie.Secure || w.cookie.SameSite != http.SameSiteLaxMode {
		h.t.Fatalf("cookie = %+v", w.cookie)
	}
}

// beacon sends what the script sends from a page, and says what became of it.
func (h *harness) beacon(site, page, ref, origin string) string {
	body, _ := json.Marshal(map[string]string{"s": site, "u": page, "r": ref})
	hdr := map[string]string{"Content-Type": "text/plain;charset=UTF-8", "Origin": origin}
	rec := h.do(nil, "POST", "/e", string(body), hdr)
	if rec.Code != http.StatusAccepted || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		h.t.Fatalf("beacon = %d %v", rec.Code, rec.Header())
	}
	return rec.Header().Get("X-Exe-Stats")
}

func (h *harness) siteID(w *wallet, domain string) string {
	h.t.Helper()
	sites, _ := h.app.store.sitesOf(w.addr)
	for _, s := range sites {
		if s.Domain == domain {
			return s.ID
		}
	}
	h.t.Fatalf("no site %s", domain)
	return ""
}

func TestSignIn(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.signIn(w)
	if got := h.do(w, "GET", "/", "", nil).Body.String(); !strings.Contains(got, `id="account"`) || strings.Contains(got, `id="signin"`) {
		t.Fatal("signed in, the page is not the account's")
	}
	if got := h.do(nil, "GET", "/", "", nil).Body.String(); !strings.Contains(got, `id="signin"`) || strings.Contains(got, `id="account"`) {
		t.Fatal("signed out, the page is not the welcome")
	}

	challenge := func(addr string) string {
		var c struct{ Message string }
		json.Unmarshal(h.do(nil, "GET", "/v1/challenge?address="+addr, "", nil).Body.Bytes(), &c)
		return c.Message
	}
	post := func(addr, msg string, sig []byte, hdr map[string]string) int {
		body, _ := json.Marshal(map[string]string{"address": addr, "message": msg, "signature": base64.StdEncoding.EncodeToString(sig)})
		return h.do(nil, "POST", "/v1/signin", string(body), hdr).Code
	}
	other := newWallet()
	// another key's signature over this address's challenge
	msg := challenge(w.addr)
	if code := post(w.addr, msg, ed25519.Sign(other.key, []byte(msg)), nil); code != 401 {
		t.Fatalf("a stranger's signature = %d", code)
	}
	// the challenge was spent by that try, good signature or not
	if code := post(w.addr, msg, ed25519.Sign(w.key, []byte(msg)), nil); code != 401 {
		t.Fatalf("a spent challenge = %d", code)
	}
	// a challenge issued for one address, signed and presented by another
	msg = challenge(w.addr)
	if code := post(other.addr, msg, ed25519.Sign(other.key, []byte(msg)), nil); code != 401 {
		t.Fatalf("another address's challenge = %d", code)
	}
	// a message the server never issued
	forged := strings.Replace(challenge(w.addr), "Nonce: ", "Nonce: 00", 1)
	if code := post(w.addr, forged, ed25519.Sign(w.key, []byte(forged)), nil); code != 401 {
		t.Fatalf("a forged message = %d", code)
	}
	// a good signature posted from another site's page
	msg = challenge(w.addr)
	if code := post(w.addr, msg, ed25519.Sign(w.key, []byte(msg)), map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("from another origin = %d", code)
	}
	if rec := h.do(nil, "GET", "/v1/challenge?address=not-an-address", "", nil); rec.Code != 400 {
		t.Fatalf("a bad address = %d", rec.Code)
	}

	// signing out ends the session on the server, not only in the browser
	old := *w.cookie
	h.form(w, "/v1/signout", nil)
	w.cookie = &old
	if got := h.do(w, "GET", "/", "", nil).Body.String(); !strings.Contains(got, `id="signin"`) {
		t.Fatal("the old cookie still opens the account")
	}
}

func TestSitesAndBeacons(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)

	// a wallet that holds nothing may sign in and add nothing
	poor := newWallet()
	h.signIn(poor)
	if loc := h.form(poor, "/v1/sites", url.Values{"domain": {"poor.example"}}); loc != "/?e=hold" {
		t.Fatalf("no holding: %s", loc)
	}

	loc := h.form(w, "/v1/sites", url.Values{"domain": {"https://WWW.Shop.Example/tea?x=1"}, "tz": {"Asia/Tokyo"}})
	if shop := h.siteID(w, "shop.example"); loc != "/sites/"+shop+"/settings" {
		t.Fatalf("add: %s, want the new site's settings", loc)
	}
	if loc := h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}}); loc != "/?e=exists" {
		t.Fatalf("twice: %s", loc)
	}
	if loc := h.form(w, "/v1/sites", url.Values{"domain": {"not a domain"}}); loc != "/?e=domain" {
		t.Fatalf("bad domain: %s", loc)
	}
	h.form(w, "/v1/sites", url.Values{"domain": {"blog.example"}})
	if loc := h.form(w, "/v1/sites", url.Values{"domain": {"third.example"}}); loc != "/?e=tier" {
		t.Fatalf("a third site on the base tier: %s", loc)
	}
	shop := h.siteID(w, "shop.example")
	snippet := `&lt;script defer data-site=&#34;` + shop + `&#34; src=&#34;https://stats.example/js/s.js&#34;&gt;&lt;/script&gt;`
	if !strings.Contains(h.do(w, "GET", "/sites/"+shop+"/settings", "", nil).Body.String(), snippet) {
		t.Fatal("the settings do not carry the snippet")
	}
	if home := h.do(w, "GET", "/", "", nil).Body.String(); strings.Contains(home, "data-site=") || strings.Count(home, `<a class="bars" href="/sites/`) != 2 {
		t.Fatal("the Sites window carries a snippet, or not a day's bars a site")
	}

	for _, c := range []struct{ name, site, page, origin, want string }{
		{"the site's own page", shop, "https://shop.example/tea?utm_campaign=spring", "https://shop.example", beaconCounted},
		{"its www name", shop, "https://www.shop.example/", "https://www.shop.example", beaconCounted},
		{"another site's page under this id", shop, "https://evil.example/", "https://evil.example", beaconForeign},
		{"the right page claimed from another origin", shop, "https://shop.example/", "https://evil.example", beaconOrigin},
		{"no such site", "000000000000", "https://shop.example/", "https://shop.example", beaconUnknown},
		{"not an address", shop, "javascript:alert(1)", "https://shop.example", beaconBad},
	} {
		if got := h.beacon(c.site, c.page, "https://www.google.com/", c.origin); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// a crawler that runs the script is kept apart and is not a view of the month
	body, _ := json.Marshal(map[string]string{"s": shop, "u": "https://shop.example/"})
	if got := h.do(nil, "POST", "/e", string(body), map[string]string{"Origin": "https://shop.example", "User-Agent": "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"}).Header().Get("X-Exe-Stats"); got != beaconCrawler {
		t.Fatalf("a crawler: %q", got)
	}
	if rec := h.do(nil, "OPTIONS", "/e", "", nil); rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Methods") != "POST" {
		t.Fatalf("preflight = %d", rec.Code)
	}

	// the desk is its owner's alone
	h.app.mu.Lock()
	st := h.app.sites[shop].st
	ac := h.app.accts[w.addr]
	views := ac.cur.Views
	h.app.mu.Unlock()
	if views != 2 {
		t.Fatalf("views this month = %d, want 2", views)
	}
	time.Sleep(1200 * time.Millisecond) // the site's writer lands its batch once a second
	st.ResetCache()
	desk := h.do(w, "GET", "/sites/"+shop+"?range=24h", "", nil)
	if desk.Code != 200 || !strings.Contains(desk.Body.String(), "shop.example") || !strings.Contains(desk.Body.String(), `href="/sites/`+shop+`?range=7d"`) {
		t.Fatalf("desk = %d", desk.Code)
	}
	var rep struct {
		Zone    string
		Summary struct{ Pageviews, Visitors int }
		Lists   map[string][]struct {
			Key string
			N   int
		}
	}
	rec := h.do(w, "GET", "/v1/sites/"+shop+"/stats?range=24h", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil || rep.Summary.Pageviews != 2 || rep.Summary.Visitors != 1 || rep.Zone != "Asia/Tokyo" {
		t.Fatalf("report = %s (%v)", rec.Body, err)
	}
	if rec := h.do(poor, "GET", "/sites/"+shop, "", nil); rec.Code != 404 {
		t.Fatalf("a stranger's read of the desk = %d", rec.Code)
	}
	if rec := h.do(nil, "GET", "/v1/sites/"+shop+"/stats", "", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("a signed-out read of the JSON = %d", rec.Code)
	}
	// and a stranger cannot remove it or press its buttons
	if loc := h.form(poor, "/v1/sites/"+shop+"/remove", nil); loc != "/?e=gone" {
		t.Fatalf("a stranger's remove: %s", loc)
	}
	if rec := h.do(w, "POST", "/v1/sites/"+shop+"/remove", "", map[string]string{"Origin": "https://evil.example"}); rec.Header().Get("Location") != "/?e=foreign" {
		t.Fatalf("a remove posted from another site = %s", rec.Header().Get("Location"))
	}

	// the usage is written down, and a new process reads it back
	h.app.flushUsage()
	if u := h.app.store.usage(w.addr, monthOf(time.Now())); u.Views != 2 {
		t.Fatalf("stored usage = %+v", u)
	}

	// Remove Site… is at the foot of the site's settings, and asks
	// first; the Sites window has none
	if home := h.do(w, "GET", "/?remove="+shop, "", nil).Body.String(); strings.Contains(home, "Remove") {
		t.Fatal("the Sites window still has Remove")
	}
	set := h.do(w, "GET", "/sites/"+shop+"/settings", "", nil).Body.String()
	if i := strings.Index(set, `<section id="danger">`); i < 0 || strings.Contains(set[i+1:], "<section") ||
		!strings.Contains(set[i:], `href="/sites/`+shop+`/settings?remove=1#danger">Remove Site…</a>`) ||
		strings.Contains(set, `action="/v1/sites/`+shop+`/remove"`) {
		t.Fatalf("the settings' Danger Zone:\n%s", set)
	}
	ask := h.do(w, "GET", "/sites/"+shop+"/settings?remove=1", "", nil).Body.String()
	if !strings.Contains(ask, `<span class="say">Remove shop.example and its numbers? This cannot be undone.</span>`) ||
		!strings.Contains(ask, `<a class="btn" href="/sites/`+shop+`/settings#danger">Cancel</a>`) ||
		!strings.Contains(ask, `<form method="post" action="/v1/sites/`+shop+`/remove"><button class="btn">Remove</button></form>`) {
		t.Fatalf("Remove Site… does not ask:\n%s", ask)
	}

	// removing the site takes its file with it
	if loc := h.form(w, "/v1/sites/"+shop+"/remove", nil); loc != "/?m=removed" {
		t.Fatalf("remove: %s", loc)
	}
	if got := h.beacon(shop, "https://shop.example/", "", "https://shop.example"); got != beaconUnknown {
		t.Fatalf("a beacon for a removed site: %q", got)
	}
	if m, _ := filepath.Glob(filepath.Join(h.app.cfg.Data, "sites", shop+".db*")); len(m) != 0 {
		t.Fatalf("files left: %v", m)
	}
}

// The setup check: a site waits for its first view, says why a view
// was refused, turns OK on one from its own pages, and keeps that over a
// restart; a site that counted before the check reads it from its hits.
func TestSetupCheck(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}})
	shop := h.siteID(w, "shop.example")
	if home := h.do(w, "GET", "/", "", nil).Body.String(); !strings.Contains(home, `href="/sites/`+shop+`/settings"`) {
		t.Fatal("the home page does not link the site's settings")
	}
	check := func() setupView {
		t.Helper()
		rec := h.do(w, "GET", "/v1/sites/"+shop+"/setup", "", nil)
		var v setupView
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &v) != nil {
			t.Fatalf("setup = %d %s", rec.Code, rec.Body)
		}
		return v
	}
	settings := func() string {
		t.Helper()
		rec := h.do(w, "GET", "/sites/"+shop+"/settings", "", nil)
		if rec.Code != 200 {
			t.Fatalf("settings = %d", rec.Code)
		}
		return rec.Body.String()
	}

	if v := check(); v.OK || !strings.HasPrefix(v.Line, "Waiting for the first page view.") {
		t.Fatalf("before any view: %+v", v)
	}
	if page := settings(); !strings.Contains(page, `<div class="setup" id="setup"`) || !strings.Contains(page, "Waiting for the first page view.") ||
		!strings.Contains(page, `data-site=&#34;`+shop+`&#34;`) || !strings.Contains(page, "Days begin in UTC") {
		t.Fatalf("the page before any view:\n%s", page)
	}
	// a page on another host carrying this site's snippet
	h.beacon(shop, "https://staging.example/tea", "", "https://staging.example")
	if v := check(); v.OK || v.Line != "A view came from staging.example just now and was not counted: it is not one of the hosts below." {
		t.Fatalf("a foreign view: %+v", v)
	}
	h.beacon(shop, "https://shop.example/", "", "https://evil.example")
	if v := check(); v.OK || v.Line != "A view of shop.example was sent from evil.example just now, and was not counted." {
		t.Fatalf("another origin: %+v", v)
	}
	// one from its own page: the snippet works, and the refusal stays a note
	if got := h.beacon(shop, "https://www.shop.example/", "", "https://www.shop.example"); got != beaconCounted {
		t.Fatalf("own page: %q", got)
	}
	if v := check(); !v.OK || v.Line != "The snippet works: the last page view came just now." {
		t.Fatalf("after a view: %+v", v)
	}
	if page := settings(); !strings.Contains(page, `<div class="setup ok" id="setup"`) ||
		!strings.Contains(page, `<span>A view of shop.example was sent from evil.example just now, and was not counted.</span><input type="hidden" name="host" value="evil.example">`) {
		t.Fatalf("the page after a view:\n%s", page)
	}

	// another wallet, or none, gets nothing
	poor := newWallet()
	h.signIn(poor)
	for _, p := range []string{"/sites/" + shop + "/settings", "/v1/sites/" + shop + "/setup"} {
		if rec := h.do(poor, "GET", p, "", nil); rec.Code != 404 {
			t.Fatalf("a stranger's %s = %d", p, rec.Code)
		}
		if rec := h.do(nil, "GET", p, "", nil); rec.Code != http.StatusSeeOther {
			t.Fatalf("signed out %s = %d", p, rec.Code)
		}
	}

	// written down: the store has it after a flush
	h.app.flushSeen(false)
	sites, _ := h.app.store.sitesOf(w.addr)
	if len(sites) != 1 || sites[0].Seen == 0 {
		t.Fatalf("stored = %+v", sites)
	}

	// a site that counted before the server kept this: read from its hits
	time.Sleep(1200 * time.Millisecond) // the site's writer lands its batch once a second
	h.app.store.saveSeen(shop, 0)
	h.app.mu.Lock()
	e := h.app.sites[shop]
	e.site.Seen, e.saved, e.backfilled, e.refused = 0, 0, false, refusal{}
	h.app.mu.Unlock()
	if v := check(); !v.OK {
		t.Fatalf("from the hits: %+v", v)
	}
}

// Hosts: a site counts the pages of the hosts its owner lists besides
// its domain, a name or every name under one; another host's pages are
// listed by host on the desk, and a step between the site's own hosts
// is no source.
func TestHosts(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}})
	shop := h.siteID(w, "shop.example")
	settings := "/sites/" + shop + "/settings"
	add := func(host string) string { return h.form(w, "/v1/sites/"+shop+"/hosts", url.Values{"host": {host}}) }
	at := func(key, word string) string { return settings + "?" + key + "=" + word + "#hosts" }

	if got := h.beacon(shop, "https://blog.shop.example/post", "", "https://blog.shop.example"); got != beaconForeign {
		t.Fatalf("an unlisted host: %q", got)
	}
	setup := func() setupView {
		var v setupView
		json.Unmarshal(h.do(w, "GET", "/v1/sites/"+shop+"/setup", "", nil).Body.Bytes(), &v)
		return v
	}
	if v := setup(); !strings.HasPrefix(v.Line, "A view came from blog.shop.example") {
		t.Fatalf("the refusal: %+v", v)
	}
	for _, c := range []struct{ in, want string }{
		{"https://www.Blog.Shop.Example/post?x=1", at("m", "hostadded")}, // read like the domain is: blog.shop.example
		{"blog.shop.example", at("e", "counts")},
		{"shop.example", at("e", "counts")},
		{"www.shop.example", at("e", "counts")},
		{"not a host", at("e", "host")},
		{"*.", at("e", "host")},
		{"*.docs.example", at("m", "hostadded")},
		{"api.docs.example", at("e", "counts")}, // under *.docs.example
		{"*.v2.docs.example", at("e", "counts")},
		{"mirror.example.net", at("m", "hostadded")},
	} {
		if got := add(c.in); got != c.want {
			t.Errorf("add %q: %s, want %s", c.in, got, c.want)
		}
	}
	if v := setup(); !strings.HasPrefix(v.Line, "Waiting for the first page view.") {
		t.Fatalf("the refusal outlived its host's listing: %+v", v)
	}
	page := h.do(w, "GET", settings+"?m=hostadded", "", nil).Body.String()
	for _, want := range []string{`value="blog.shop.example"`, `value="*.docs.example"`, "Every subdomain", `value="mirror.example.net"`, "The host counts now."} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page lacks %s", want)
		}
	}

	for i, c := range []struct{ name, page, origin, ref, want string }{
		{"a listed host", "https://blog.shop.example/post", "https://blog.shop.example", "https://shop.example/", beaconCounted},
		{"its www name", "https://www.blog.shop.example/", "https://www.blog.shop.example", "", beaconCounted},
		{"a name under a *. host", "https://api.v2.docs.example/ref", "https://api.v2.docs.example", "https://news.ycombinator.com/", beaconCounted},
		{"the *. host's own name", "https://docs.example/", "https://docs.example", "", beaconCounted},
		{"the domain", "https://shop.example/tea", "https://shop.example", "https://mirror.example.net/", beaconCounted},
		{"a listed page sent from another listed host", "https://blog.shop.example/", "https://shop.example", "", beaconOrigin},
		{"a host not listed", "https://shop.example.evil/", "https://shop.example.evil", "", beaconForeign},
		{"a name ending like a host", "https://evildocs.example/", "https://evildocs.example", "", beaconForeign},
	} {
		// each a reader of its own, so each view starts a visit with its source
		body, _ := json.Marshal(map[string]string{"s": shop, "u": c.page, "r": c.ref})
		hdr := map[string]string{"Origin": c.origin, "CF-Connecting-IP": fmt.Sprintf("198.51.100.%d", i+1)}
		if got := h.do(nil, "POST", "/e", string(body), hdr).Header().Get("X-Exe-Stats"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}

	// the desk: another host's pages by host and path, and the steps
	// between the site's hosts no source
	time.Sleep(1200 * time.Millisecond) // the site's writer lands its batch once a second
	h.app.mu.Lock()
	h.app.sites[shop].st.ResetCache()
	h.app.mu.Unlock()
	var rep struct {
		Lists map[string][]struct {
			Key string
			N   int
		}
	}
	rec := h.do(w, "GET", "/v1/sites/"+shop+"/stats?range=24h", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatalf("report = %s", rec.Body)
	}
	keys := func(list string) []string {
		var out []string
		for _, r := range rep.Lists[list] {
			out = append(out, r.Key)
		}
		return out
	}
	for _, want := range []string{"blog.shop.example/post", "blog.shop.example/", "api.v2.docs.example/ref", "docs.example/", "/tea"} {
		if !slices.Contains(keys("pages"), want) {
			t.Errorf("pages %v lack %s", keys("pages"), want)
		}
	}
	if src := keys("sources"); slices.Contains(src, "shop.example") || slices.Contains(src, "mirror.example.net") || !slices.Contains(src, "Hacker News") {
		t.Errorf("sources = %v", src)
	}

	// removing one: it stops counting, and the others stay
	if got := h.form(w, "/v1/sites/"+shop+"/hosts/remove", url.Values{"host": {"blog.shop.example"}}); got != at("m", "hostremoved") {
		t.Fatalf("remove: %s", got)
	}
	if got := h.form(w, "/v1/sites/"+shop+"/hosts/remove", url.Values{"host": {"blog.shop.example"}}); got != at("e", "nohost") {
		t.Fatalf("remove again: %s", got)
	}
	if got := h.beacon(shop, "https://blog.shop.example/post", "", "https://blog.shop.example"); got != beaconForeign {
		t.Fatalf("a removed host: %q", got)
	}

	// another wallet cannot touch them
	poor := newWallet()
	h.bal.set(poor.addr, 10000)
	h.signIn(poor)
	if got := h.form(poor, "/v1/sites/"+shop+"/hosts", url.Values{"host": {"evil.example"}}); got != "/?e=gone" {
		t.Fatalf("a stranger's add: %s", got)
	}

	// a site lists at most maxHosts
	for i := len(h.app.sites[shop].site.Hosts); i < maxHosts; i++ {
		add(fmt.Sprintf("h%d.example", i))
	}
	if got := add("one-more.example"); got != at("e", "hostmax") {
		t.Fatalf("past the most: %s", got)
	}

	// written down: a new process reads them back
	app2, err := newApp(h.app.cfg, h.app.store.db, h.bal)
	if err != nil {
		t.Fatal(err)
	}
	if got := app2.sites[shop].site.Hosts; len(got) != maxHosts || got[0] != "*.docs.example" || got[1] != "mirror.example.net" {
		t.Fatalf("read back: %v", got)
	}
}

// Ignore: a note that names a host has Ignore, and an ignored host's
// views are refused without a note from then on, until it is taken off
// the list or listed as a host.
func TestIgnore(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}})
	shop := h.siteID(w, "shop.example")
	settings := "/sites/" + shop + "/settings"
	page := func() string { return h.do(w, "GET", settings, "", nil).Body.String() }
	ignore := func(host string) string { return h.form(w, "/v1/sites/"+shop+"/ignore", url.Values{"host": {host}}) }
	unignore := func(host string) string {
		return h.form(w, "/v1/sites/"+shop+"/ignored/remove", url.Values{"host": {host}})
	}
	const button = `<button class="btn" id="ignore">Ignore</button>`
	h.beacon(shop, "https://shop.example/", "", "https://shop.example")

	h.beacon(shop, "https://sunp.example.org/tea", "", "https://sunp.example.org")
	if p := page(); !strings.Contains(p, `<span>A view came from sunp.example.org just now and was not counted: it is not one of the hosts below.</span>`+
		`<input type="hidden" name="host" value="sunp.example.org">`+button) || strings.Contains(p, `id="ignored"`) {
		t.Fatalf("the note has no Ignore:\n%s", p)
	}
	if got := ignore("sunp.example.org"); got != settings+"?m=ignored#setup-section" {
		t.Fatalf("ignore: %s", got)
	}
	if p := h.do(w, "GET", settings+"?m=ignored", "", nil).Body.String(); strings.Contains(p, `class="notice`) ||
		!strings.Contains(p, `id="setup-said" role="status">That host is ignored: its views are no longer noted here.</p>`) ||
		!strings.Contains(p, `<section id="ignored">`) || !strings.Contains(p, `<input type="radio" name="host" value="sunp.example.org">`) {
		t.Fatalf("after Ignore:\n%s", p)
	}
	// refused as before, and not noted
	if got := h.beacon(shop, "https://www.sunp.example.org/more", "", "https://www.sunp.example.org"); got != beaconForeign {
		t.Fatalf("an ignored host's view: %q", got)
	}
	if strings.Contains(page(), `class="notice`) {
		t.Fatal("an ignored host is noted again")
	}
	// as the host of another origin, too; and a host not ignored still is
	h.beacon(shop, "https://shop.example/", "", "https://sunp.example.org")
	if strings.Contains(page(), `class="notice`) {
		t.Fatal("an ignored origin is noted")
	}
	h.beacon(shop, "https://staging.example/", "", "https://staging.example")
	if !strings.Contains(page(), `value="staging.example">`+button) {
		t.Fatal("another host is not noted")
	}

	for in, want := range map[string]string{
		"www.shop.example": "?e=ignorecounts#setup-section", // the site's own
		"a b":              "?e=ignorehost#setup-section",
		"":                 "?e=ignorehost#setup-section",
		"Sunp.Example.org": "?m=ignored#setup-section", // already: nothing changes
	} {
		if got := ignore(in); got != settings+want {
			t.Errorf("ignore %q: %s, want %s", in, got, want)
		}
	}
	// no Ignore where a note names no host, or one that counts
	h.beacon(shop, "https://shop.example/", "", "null")
	if p := page(); !strings.Contains(p, `<p class="notice">A view of shop.example was sent from another origin just now, and was not counted.</p>`) {
		t.Fatalf("an unreadable origin:\n%s", p)
	}
	h.form(w, "/v1/sites/"+shop+"/hosts", url.Values{"host": {"blog.shop.example"}})
	h.beacon(shop, "https://blog.shop.example/", "", "https://shop.example")
	if p := page(); !strings.Contains(p, `<p class="notice">A view of blog.shop.example was sent from shop.example just now, and was not counted.</p>`) {
		t.Fatalf("an origin that counts:\n%s", p)
	}

	// taken off: noted again
	if got := unignore("nobody.example"); got != settings+"?e=noignored#ignored" {
		t.Fatalf("unignore a stranger: %s", got)
	}
	if got := unignore("sunp.example.org"); got != settings+"?m=unignored#ignored" {
		t.Fatalf("unignore: %s", got)
	}
	if p := h.do(w, "GET", settings+"?m=unignored", "", nil).Body.String(); !strings.Contains(p, `<section id="ignored">`) ||
		strings.Contains(p, `name="host" value="sunp.example.org"`) || !strings.Contains(p, `id="ignored-said" role="status">That host&#39;s views are noted again.</p>`) {
		t.Fatalf("after unignore:\n%s", p)
	}
	if strings.Contains(page(), `<section id="ignored">`) {
		t.Fatal("an empty list stays without its word")
	}
	h.beacon(shop, "https://sunp.example.org/", "", "https://sunp.example.org")
	if !strings.Contains(page(), `value="sunp.example.org">`+button) {
		t.Fatal("a host taken off is not noted")
	}

	// listing an ignored host takes it off the list: it is the site's own
	ignore("sunp.example.org")
	ignore("mirror.example")
	h.form(w, "/v1/sites/"+shop+"/hosts", url.Values{"host": {"*.example.org"}})
	if got := h.app.sites[shop].site.Ignored; len(got) != 1 || got[0] != "mirror.example" {
		t.Fatalf("after listing *.example.org: %v", got)
	}

	// another wallet cannot touch them
	poor := newWallet()
	h.signIn(poor)
	if got := h.form(poor, "/v1/sites/"+shop+"/ignore", url.Values{"host": {"evil.example"}}); got != "/?e=gone" {
		t.Fatalf("a stranger's ignore: %s", got)
	}

	// a site ignores at most maxIgnored
	for i := 1; i < maxIgnored; i++ {
		ignore(fmt.Sprintf("m%d.example", i))
	}
	if got := ignore("one-more.example"); got != settings+"?e=ignoremax#setup-section" {
		t.Fatalf("past the most: %s", got)
	}

	// written down: a new process reads them back
	app2, err := newApp(h.app.cfg, h.app.store.db, h.bal)
	if err != nil {
		t.Fatal(err)
	}
	if got := app2.sites[shop].site.Ignored; len(got) != maxIgnored || got[0] != "mirror.example" || got[1] != "m1.example" {
		t.Fatalf("read back: %v", got)
	}
}

func TestHostEntry(t *testing.T) {
	for in, want := range map[string]string{
		"blog.example.com": "blog.example.com", "WWW.Example.com": "example.com", "https://a.example.com:8080/x": "a.example.com",
		"*.example.com": "*.example.com", "*.www.example.com": "*.example.com", "*": "", "*.com": "", "a.*.example.com": "", "example": "",
	} {
		got, ok := hostEntry(in)
		if (want == "") == ok || (ok && got != want) {
			t.Errorf("hostEntry(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}

// The grace table against the server: a short balance keeps counting,
// refuses new sites, and after the grace the lower tier rests the sites
// it does not hold; no tier pauses everything and deletes nothing.
func TestGraceOnTheServer(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 50000)
	h.signIn(w)
	for _, d := range []string{"a.example", "b.example", "c.example"} {
		h.form(w, "/v1/sites", url.Values{"domain": {d}})
	}
	a, c := h.siteID(w, "a.example"), h.siteID(w, "c.example")
	hit := func(id, domain string) string {
		return h.beacon(id, "https://"+domain+"/", "", "https://"+domain)
	}
	if hit(c, "c.example") != beaconCounted {
		t.Fatal("the third site of the middle tier does not count")
	}

	// an RPC that does not answer changes nothing
	h.bal.down = true
	h.bal.set(w.addr, 0)
	if loc := h.form(w, "/v1/check", nil); loc != "/?e=rpc" {
		t.Fatalf("check with the RPC down: %s", loc)
	}
	if hit(c, "c.example") != beaconCounted {
		t.Fatal("an unanswered check stopped the counting")
	}
	h.bal.down = false

	// short: everything counts, no new sites, and the page says until when
	h.bal.set(w.addr, 10000)
	h.app.manual = map[string]time.Time{}
	if loc := h.form(w, "/v1/check", nil); loc != "/?m=checked" {
		t.Fatalf("check: %s", loc)
	}
	if loc := h.form(w, "/v1/check", nil); loc != "/?e=wait" {
		t.Fatalf("a second check at once: %s", loc)
	}
	if hit(c, "c.example") != beaconCounted {
		t.Fatal("inside the grace the third site stopped counting")
	}
	if loc := h.form(w, "/v1/sites", url.Values{"domain": {"d.example"}}); loc != "/?e=short" {
		t.Fatalf("a new site while short: %s", loc)
	}
	if page := h.do(w, "GET", "/", "", nil).Body.String(); !strings.Contains(page, "The balance is 40,000 $V2EX under the Middle tier.") || !strings.Contains(page, "the account moves to the Base tier") {
		t.Fatal("the page does not say the account is short")
	}

	// the grace over: the base tier counts two sites, the third rests
	h.app.mu.Lock()
	ac := h.app.accts[w.addr]
	ac.ShortSince -= (8 * 24 * time.Hour).Milliseconds()
	h.app.mu.Unlock()
	h.app.checkNow(w.addr, true)
	if hit(a, "a.example") != beaconCounted || hit(c, "c.example") != beaconResting {
		t.Fatal("after the grace: the oldest two count and the third rests")
	}
	// the owner picks the third: it counts, and the second oldest rests
	if loc := h.form(w, "/v1/sites/"+c+"/pick", nil); loc != "/?m=picked" {
		t.Fatalf("pick: %s", loc)
	}
	if hit(c, "c.example") != beaconCounted || hit(h.siteID(w, "b.example"), "b.example") != beaconResting {
		t.Fatal("the picked site does not count in the other's place")
	}

	// under the lowest tier inside the same thirty days: paused at once
	h.bal.set(w.addr, 9999)
	h.app.checkNow(w.addr, true)
	if hit(c, "c.example") != beaconResting {
		t.Fatal("a paused account still counts")
	}
	page := h.do(w, "GET", "/", "", nil).Body.String()
	if !strings.Contains(page, "Paused: the balance covers no tier") || strings.Count(page, `class="site"`) != 3 {
		t.Fatal("paused: the page does not say so, or a site is gone")
	}
	if rec := h.do(w, "GET", "/sites/"+c, "", nil); rec.Code != 200 {
		t.Fatalf("paused: the desk is not readable, %d", rec.Code)
	}
	// topped up: Good at the next check
	h.bal.set(w.addr, 100000)
	h.app.checkNow(w.addr, true)
	if hit(h.siteID(w, "b.example"), "b.example") != beaconCounted {
		t.Fatal("topped up, a site still rests")
	}
}

// A month over the cap counts to twice the cap and then stops.
func TestMonthFullOnTheServer(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"a.example"}})
	a := h.siteID(w, "a.example")
	h.app.mu.Lock()
	h.app.accts[w.addr].cur.Views = 39999
	h.app.mu.Unlock()
	h.app.limit = newLimiter()
	if got := h.beacon(a, "https://a.example/", "", "https://a.example"); got != beaconCounted {
		t.Fatalf("view 40,000: %q", got)
	}
	if got := h.beacon(a, "https://a.example/", "", "https://a.example"); got != beaconFull {
		t.Fatalf("view 40,001: %q", got)
	}
	if page := h.do(w, "GET", "/", "", nil).Body.String(); !strings.Contains(page, "Over the month&#39;s 20K views. Counting goes on up to 40K this month; the Middle tier would fit.") {
		t.Fatal("the page does not say the month is over the cap")
	}
}

func TestDomainOf(t *testing.T) {
	for in, want := range map[string]string{
		"example.com": "example.com", " Example.COM ": "example.com", "https://www.example.com/a?b#c": "example.com",
		"www.example.com/path": "example.com", "sub.example.co.uk:8443": "sub.example.co.uk", "example.com.": "example.com",
	} {
		if got, ok := domainOf(in); !ok || got != want {
			t.Errorf("domainOf(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "localhost", "exa mple.com", "-bad.example", "a..b", "http://", "<script>.example"} {
		if got, ok := domainOf(in); ok {
			t.Errorf("domainOf(%q) = %q, want a refusal", in, got)
		}
	}
}

func TestBase58(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	for _, b := range [][]byte{pub, append([]byte{0, 0}, pub[2:]...)} {
		addr := base58Encode(b)
		back, ok := addressKey(addr)
		if !ok || string(back) != string(b) {
			t.Fatalf("round trip of %x through %s = %x %v", b, addr, back, ok)
		}
	}
	// the mint is an address too: a known one decodes to 32 bytes
	if k, ok := addressKey("9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump"); !ok || len(k) != 32 {
		t.Fatal("the mint does not decode")
	}
	if _, ok := addressKey("0OIl"); ok {
		t.Fatal("letters outside the alphabet decoded")
	}
}

// A tier added to the config is an account's at once when the balance
// last read covers it — the next check's word, without waiting for it —
// and a balance under its tier is not lowered before a check.
func TestTierAddedApplies(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	for _, c := range []struct {
		addr       string
		tier       int
		bal        int64
		want       int
		checkedOne bool
	}{
		{"rich", 2, 2000000 * tok, 4, true},      // holds two million: the new Max
		{"short", 2, 20000 * tok, 2, true},       // under High: a check, and its grace, decide
		{"unread", -1, 2000000 * tok, -1, false}, // never checked: nothing to go by
	} {
		rec, err := h.app.store.account(c.addr, now)
		if err != nil {
			t.Fatal(err)
		}
		rec.Tier, rec.Balance = c.tier, c.bal
		if c.checkedOne {
			rec.Checked = now.UnixMilli()
		}
		if err := h.app.store.saveAccount(rec); err != nil {
			t.Fatal(err)
		}
		h.app.mu.Lock()
		ac, err := h.app.acctOf(c.addr, now)
		h.app.mu.Unlock()
		if err != nil || ac.Tier != c.want {
			t.Fatalf("%s: tier %d, want %d (%v)", c.addr, ac.Tier, c.want, err)
		}
		if again, _ := h.app.store.account(c.addr, now); again.Tier != c.want {
			t.Fatalf("%s: the store has tier %d", c.addr, again.Tier)
		}
	}
}

// In a zone off by half an hour, the views of an hour's last half stand
// in the next bar: that bar is the one still filling, never one to come
// hiding views the day's total counts.
func TestTodayHalfHour(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip(err)
	}
	db, err := openDB(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	st, err := stats.New(db, stats.Options{Location: kolkata, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 2, 10, 45, 0, 0, kolkata)
	if err := st.DB().Add([]stats.Hit{{TS: now.Add(-5 * time.Minute).UnixMilli(), VID: "v", SID: "s", Entry: true, Path: "/", Kind: "page"}}); err != nil {
		t.Fatal(err)
	}
	row := siteRow{st: st}
	if err := row.today(now); err != nil {
		t.Fatal(err)
	}
	shown := 0
	for _, b := range row.Bars {
		if b.Class != "to" && b.H > 0 {
			shown++
		}
		if b.Class == "to" && b.H > 0 {
			t.Fatalf("a bar to come holds views: %+v", row.Bars)
		}
	}
	if shown != 1 || !strings.HasPrefix(row.Today, "1 view today · ") {
		t.Fatalf("%d bars hold the view, %q", shown, row.Today)
	}
}

// TestDrawDayOverYesterday: a site's bars stand over yesterday's same
// hours on one scale, the busiest hour of the two chartH high in whole
// 2px steps; the hour under way shows its cap from the first minute, and
// the hours to come still show yesterday's.
func TestDrawDayOverYesterday(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Shanghai")
	day := time.Date(2026, 10, 2, 0, 0, 0, 0, loc)
	pts := func(at time.Time, views []int, now int) []stats.Point {
		var out []stats.Point
		for h, n := range views {
			out = append(out, stats.Point{T: at.Add(time.Duration(h) * time.Hour).Format(time.RFC3339), Pageviews: n, Future: h > now})
		}
		return out
	}
	today := make([]int, 24)
	today[9], today[10] = 50, 0 // the busiest of today, and the hour under way, empty yet
	ago := make([]int, 24)
	ago[9], ago[14] = 25, 100 // yesterday's busiest is the scale
	bars, views := drawDay(pts(day, today, 10), pts(day.AddDate(0, 0, -1), ago, 23))
	if len(bars) != 24 || views != 50 {
		t.Fatalf("%d bars, %d views", len(bars), views)
	}
	for i, want := range map[int]barView{
		9:  {H: 20, Y: 10, Title: "09:00 · 50 views, 25 yesterday"},
		10: {H: 2, Y: 0, Class: "now", Title: "10:00 · 0 views so far, 0 yesterday"},
		14: {H: 0, Y: chartH, Class: "to", Title: "14:00 · 100 views yesterday"},
		3:  {H: 0, Y: 0, Title: "03:00 · 0 views, 0 yesterday"},
	} {
		if bars[i] != want {
			t.Errorf("bar %d = %+v, want %+v", i, bars[i], want)
		}
	}
	for _, b := range bars {
		if b.H%2 != 0 || b.Y%2 != 0 || b.H > chartH || b.Y > chartH {
			t.Fatalf("a bar off the 2px steps or over the top: %+v", b)
		}
	}
	// one view in a quiet hour still stands a step high
	today[3] = 1
	if bars, _ := drawDay(pts(day, today, 10), pts(day.AddDate(0, 0, -1), ago, 23)); bars[3].H != 2 {
		t.Fatalf("one view stands %dpx", bars[3].H)
	}
}

// TestTrouble: a site's row says nothing while it counts and is well,
// and otherwise what keeps it from counting or what is wrong with it.
func TestTrouble(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000) // the Base tier: two sites
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}})
	h.form(w, "/v1/sites", url.Values{"domain": {"blog.example"}})
	shop := h.siteID(w, "shop.example")
	says := func(i int, change func(ac *acct)) string {
		t.Helper()
		now := time.Now()
		h.app.mu.Lock()
		defer h.app.mu.Unlock()
		ac, err := h.app.acctOf(w.addr, now)
		if err != nil {
			t.Fatal(err)
		}
		cp := *ac
		if change != nil {
			change(&cp)
		}
		return h.app.trouble(&cp, i, h.app.sites[shop], now)
	}
	if got := says(0, nil); got != "No page view yet: its snippet, in Settings…, goes in the site's head." {
		t.Fatalf("a new site: %q", got)
	}
	if home := h.do(w, "GET", "/", "", nil).Body.String(); strings.Count(home, `<span class="say trouble" role="status">No page view yet`) != 2 || strings.Contains(home, ">Counting<") {
		t.Fatal("the rows do not say their snippets are silent, or still say Counting")
	}
	if got := h.beacon(shop, "https://shop.example/", "", "https://shop.example"); got != beaconCounted {
		t.Fatalf("beacon: %q", got)
	}
	if got := says(0, nil); got != "" {
		t.Fatalf("a site that counts and is well says %q", got)
	}
	if got := h.beacon(shop, "https://evil.example/", "", "https://evil.example"); got != beaconForeign {
		t.Fatalf("beacon: %q", got)
	}
	if got := says(0, nil); got != "A view from evil.example was not counted: see Settings…" {
		t.Fatalf("a refused view: %q", got)
	}
	if got := says(2, nil); got != "Resting: not one of the 2 sites this tier counts." {
		t.Fatalf("a third site on the Base tier: %q", got)
	}
	if got := says(0, func(ac *acct) {
		ac.prev = Usage{Full: true, FullTier: ac.Tier}
		ac.cur.Views = 20000
	}); got != "Not counted: the month's views are at the cap." {
		t.Fatalf("a second month at the cap: %q", got)
	}
	if got := says(0, func(ac *acct) { ac.Tier = -1 }); got != "Not counted: the holding covers no tier." {
		t.Fatalf("no tier: %q", got)
	}
}
