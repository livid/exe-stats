package main

import (
	"bytes"
	"html"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestBadgeGIF: the badge is an 88×31 GIF that plays for good, served
// for other pages to link and kept four hours, so a new drawing reaches
// them the same day.
func TestBadgeGIF(t *testing.T) {
	h := newHarness(t)
	rec := h.do(nil, "GET", "/badge.gif", "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/gif" {
		t.Fatalf("/badge.gif answered %d as %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=14400" {
		t.Errorf("/badge.gif is kept as %q", got)
	}
	g, err := gif.DecodeAll(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if g.Config.Width != 88 || g.Config.Height != 31 {
		t.Errorf("the badge is %d×%d, want 88×31", g.Config.Width, g.Config.Height)
	}
	if len(g.Image) < 2 || g.LoopCount != 0 {
		t.Errorf("%d frames looping %d times; want an animation that plays for good", len(g.Image), g.LoopCount)
	}
	for i, d := range g.Delay {
		if d < 2 {
			t.Errorf("frame %d waits %d cs; a browser would slow it to its own floor", i, d)
		}
	}
}

// TestBadgePage: /badge/ hands the badge out — HTML and Markdown that
// link the server by its public address, or on a scratch port by the
// one it was asked by — and /badge leads there.
func TestBadgePage(t *testing.T) {
	h := newHarness(t)
	rec := h.do(nil, "GET", "/badge", "", nil)
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/badge/" {
		t.Fatalf("/badge answered %d to %q", rec.Code, rec.Header().Get("Location"))
	}

	// a scratch server, with no origin of its own
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	scratch, err := newApp(Config{Data: dir}, db, &balances{m: map[string]int64{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { scratch.close(); db.Close() })

	field := regexp.MustCompile(`<input class="field snip" id="(html|md)" readonly value="([^"]*)">`)
	for _, c := range []struct {
		name, origin string
		srv          http.Handler
	}{
		{"the public server", "https://stats.example", h.srv},
		{"a scratch server", "http://127.0.0.1:7796", scratch.routes()},
	} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:7796/badge/", nil)
		w := httptest.NewRecorder()
		c.srv.ServeHTTP(w, r)
		body := w.Body.String()
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: /badge/ answered %d as %q", c.name, w.Code, w.Header().Get("Content-Type"))
		}
		got := map[string]string{}
		for _, m := range field.FindAllStringSubmatch(body, -1) {
			got[m[1]] = html.UnescapeString(m[2])
		}
		want := map[string]string{
			"html": `<a href="` + c.origin + `/"><img src="` + c.origin + `/badge.gif" width="88" height="31" alt="` + badgeAlt + `"></a>`,
			"md":   `[![` + badgeAlt + `](` + c.origin + `/badge.gif)](` + c.origin + `/)`,
		}
		if len(got) != 2 || got["html"] != want["html"] || got["md"] != want["md"] {
			t.Errorf("%s: /badge/ hands out\n%q\nwant\n%q", c.name, got, want)
		}
		if facts := "GIF, 88 × 31, " + strconv.Itoa(badgeFrames) + " frames, " + badgeSize; !strings.Contains(body, facts) {
			t.Errorf("%s: the status line does not say %q", c.name, facts)
		}
	}
}
