package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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

	if loc := h.form(w, "/v1/sites", url.Values{"domain": {"https://WWW.Shop.Example/tea?x=1"}, "tz": {"Asia/Tokyo"}}); loc != "/?m=added" {
		t.Fatalf("add: %s", loc)
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
	home := h.do(w, "GET", "/", "", nil).Body.String()
	if !strings.Contains(home, `&lt;script defer data-site=&#34;`+shop+`&#34; src=&#34;https://stats.example/js/s.js&#34;&gt;&lt;/script&gt;`) {
		t.Fatal("the page does not carry the snippet")
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
	h.app.accts[w.addr].cur.Views = 19999
	h.app.mu.Unlock()
	h.app.limit = newLimiter()
	if got := h.beacon(a, "https://a.example/", "", "https://a.example"); got != beaconCounted {
		t.Fatalf("view 20,000: %q", got)
	}
	if got := h.beacon(a, "https://a.example/", "", "https://a.example"); got != beaconFull {
		t.Fatalf("view 20,001: %q", got)
	}
	if page := h.do(w, "GET", "/", "", nil).Body.String(); !strings.Contains(page, "Over the month&#39;s 10K views. Counting goes on up to 20K this month; the Middle tier would fit.") {
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
