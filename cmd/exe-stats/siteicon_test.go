package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

var (
	testPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 40)...)
	testICO = append([]byte{0, 0, 1, 0}, make([]byte, 40)...)
)

// The addresses a site's icon may come from: public ones, never this
// machine, the tailnet, a home network or NAT64 into either.
func TestPublicIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "104.16.0.1": true, "2606:4700::1": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "192.168.0.10": false, "172.16.0.1": false,
		"100.100.1.1": false, "169.254.169.254": false, "fe80::1": false, "fd7a:115c:a1e0::1": false,
		"0.0.0.0": false, "::": false, "64:ff9b::7f00:1": false, "::ffff:127.0.0.1": false, "224.0.0.1": false,
		"2002:7f00:1::1": false, "2001:0:4136:e378:8000:63bf:3fff:fdd2": false, "::7f00:1": false, "fec0::1": false,
		"::ffff:0:7f00:1": false, "2001:4860:4860::8888": true,
	} {
		if got := publicIP(net.ParseIP(ip)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

// A page's icons, the likeliest first: one of 32 pixels or more, then
// one of no size, then the apple touch icon, then /favicon.ico; a
// vector one passed over.
func TestIconLinks(t *testing.T) {
	base, _ := url.Parse("https://v2ex.com/")
	page := []byte(`<head><link rel="icon" sizes="192x192" href="/static/icon-192.png">
<link rel="apple-touch-icon" sizes="180x180" href="/static/apple-touch-icon-180.png?v=9">
<link rel="mask-icon" href="/pin.svg" color="#000"><link rel='icon' type='image/svg+xml' href='/i.svg'>
<LINK REL="shortcut icon" HREF="/static/favicon.ico" type="image/png"></head>`)
	got := iconLinks(page, base)
	want := []string{"https://v2ex.com/static/icon-192.png", "https://v2ex.com/static/favicon.ico",
		"https://v2ex.com/static/apple-touch-icon-180.png?v=9", "https://v2ex.com/favicon.ico"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("v2ex.com: %v", got)
	}
	base, _ = url.Parse("https://x.example/")
	page = []byte(`<link rel="icon" href="/i?a=1&amp;b=2"><link rel="icon" href="/2.png"><link rel="icon" href="/3.png"><link rel="icon" href="/4.png"><link rel="icon" href="/5.png">`)
	if got := iconLinks(page, base); len(got) != 5 || got[0] != "https://x.example/i?a=1&b=2" || got[4] != "https://x.example/favicon.ico" {
		t.Fatalf("escaped, and past four: %v", got)
	}
	base, _ = url.Parse("https://v2core.com/")
	if got := iconLinks([]byte(`<link rel="icon" href="/favicon.ico" sizes="16x16 24x24 32x32">`), base); len(got) != 1 || got[0] != "https://v2core.com/favicon.ico" {
		t.Fatalf("v2core.com: %v", got)
	}
}

// fetchIcon over a server of the test's own: the page's icon; an SVG
// page falls to /favicon.ico; a site with no raster has none; and with
// the real address check, the loopback test server is refused.
func TestFetchIcon(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<link rel="icon" sizes="32x32" href="/a/i.png">`))
	})
	mux.HandleFunc("/a/i.png", func(w http.ResponseWriter, r *http.Request) { w.Write(testPNG) })
	mux.HandleFunc("/b/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<link rel="icon" href="/b/i.svg">`))
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.Write(testICO) })
	mux.HandleFunc("/c/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<link rel="icon" href="/c/not-an-image">`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer func(p func(net.IP) bool, h func(string) []string) { iconPublic, iconHomes = p, h }(iconPublic, iconHomes)
	iconHomes = func(domain string) []string { return []string{srv.URL + "/" + domain + "/"} }
	iconPublic = func(net.IP) bool { return true }
	ctx := context.Background()
	if b, typ, err := fetchIcon(ctx, "a"); err != nil || typ != "image/png" || !bytes.Equal(b, testPNG) {
		t.Fatalf("a page's icon: %q %v", typ, err)
	}
	if _, typ, err := fetchIcon(ctx, "b"); err != nil || typ != "image/x-icon" {
		t.Fatalf("past a vector one to /favicon.ico: %q %v", typ, err)
	}
	mux.HandleFunc("/c/favicon.ico", http.NotFound)
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Write([]byte("<html>not an icon</html>"))
			return
		}
		mux.ServeHTTP(w, r)
	})
	if b, typ, err := fetchIcon(ctx, "c"); err != nil || typ != "" || b != nil {
		t.Fatalf("no raster: %q %d bytes %v", typ, len(b), err)
	}
	iconPublic = publicIP
	if _, _, err := fetchIcon(ctx, "a"); err == nil || !errors.Is(err, errNotPublic) {
		t.Fatalf("a loopback site: %v, want refused", err)
	}
}

// The icon is its owner's to see, served as an image and nothing more,
// and its row on the Sites window shows it.
func TestSiteIconRoute(t *testing.T) {
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}})
	id := h.siteID(w, "shop.example")
	if rec := h.do(w, "GET", "/sites/"+id+"/icon", "", nil); rec.Code != 404 {
		t.Fatalf("before it is fetched: %d", rec.Code)
	}
	if err := h.app.store.saveIcon(id, testPNG, "image/png", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	h.app.mu.Lock()
	h.app.sites[id].icon = iconState{}
	h.app.mu.Unlock()
	rec := h.do(w, "GET", "/sites/"+id+"/icon", "", nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(rec.Body.Bytes(), testPNG) {
		t.Fatalf("the owner's icon: %d %v", rec.Code, rec.Header())
	}
	other := newWallet()
	h.bal.set(other.addr, 10000)
	h.signIn(other)
	if rec := h.do(other, "GET", "/sites/"+id+"/icon", "", nil); rec.Code != 404 {
		t.Fatalf("another wallet: %d", rec.Code)
	}
	if rec := h.do(nil, "GET", "/sites/"+id+"/icon", "", nil); rec.Code != 404 {
		t.Fatalf("signed out: %d", rec.Code)
	}
	if home := h.do(w, "GET", "/", "", nil).Body.String(); !strings.Contains(home, `<img class="ico" src="/sites/`+id+`/icon?v=`) {
		t.Fatal("the row does not show the icon")
	}
	h.form(w, "/v1/sites/"+id+"/remove", nil)
	if _, typ, _, _ := h.app.store.icon(id); typ != "" {
		t.Fatal("the icon outlived its site")
	}
}

// A site that is only busy (a 5xx) keeps its icon rather than lose it;
// four broken links of its own still leave /favicon.ico its turn.
func TestFetchIconBusy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.Write(testICO) })
	mux.HandleFunc("/four/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<link rel="icon" href="/n1.png"><link rel="icon" href="/n2.png"><link rel="icon" href="/n3.png"><link rel="icon" href="/n4.png">`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer func(p func(net.IP) bool, h func(string) []string) { iconPublic, iconHomes = p, h }(iconPublic, iconHomes)
	iconHomes = func(domain string) []string { return []string{srv.URL + "/" + domain + "/"} }
	iconPublic = func(net.IP) bool { return true }
	if _, typ, err := fetchIcon(context.Background(), "four"); err != nil || typ != "image/x-icon" {
		t.Fatalf("four broken links, then /favicon.ico: %q %v", typ, err)
	}
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<link rel="icon" href="/i.png">`))
			return
		}
		http.Error(w, "busy", 503) // its icon and its /favicon.ico alike
	}))
	defer busy.Close()
	iconHomes = func(domain string) []string { return []string{busy.URL + "/"} }
	if _, _, err := fetchIcon(context.Background(), "busy"); err == nil {
		t.Fatal("a busy site's icon read as none")
	}

	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"busy.example"}})
	id := h.siteID(w, "busy.example")
	h.app.store.saveIcon(id, testPNG, "image/png", time.Now().Add(-8*24*time.Hour).UnixMilli())
	h.app.mu.Lock()
	e := h.app.sites[id]
	e.icon = iconState{}
	h.app.iconDue(e, time.Now())
	e.icon.busy = true
	h.app.mu.Unlock()
	h.app.iconSem <- struct{}{}
	h.app.refreshIcon(e, id, "busy.example")
	if data, typ, _, _ := h.app.store.icon(id); typ != "image/png" || !bytes.Equal(data, testPNG) {
		t.Fatalf("a busy site lost its icon: %q", typ)
	}
}

// At start every due icon is asked for, however many sites there are:
// the warm-up waits for a slot rather than pass a site by.
func TestWarmIcons(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/favicon.ico") {
			time.Sleep(20 * time.Millisecond)
			w.Write(testICO)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<title>no links</title>`))
	}))
	defer srv.Close()
	defer func(p func(net.IP) bool, h func(string) []string) { iconPublic, iconHomes = p, h }(iconPublic, iconHomes)
	iconHomes = func(domain string) []string { return []string{srv.URL + "/" + domain + "/"} }
	iconPublic = func(net.IP) bool { return true }
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 2000000)
	h.signIn(w)
	h.app.mu.Lock()
	for i := range 3 * iconBusy {
		s, err := h.app.store.addSite(w.addr, fmt.Sprintf("s%d.example", i), "UTC", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		h.app.sites[s.ID] = &siteEntry{site: s}
	}
	h.app.mu.Unlock()
	h.app.warmIcons()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		h.app.store.db.QueryRow(`SELECT COUNT(*) FROM icons WHERE type <> ''`).Scan(&n)
		if n == 3*iconBusy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d icons fetched", n, 3*iconBusy)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
