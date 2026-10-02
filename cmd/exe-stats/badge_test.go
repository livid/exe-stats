package main

import (
	"bytes"
	"fmt"
	"html"
	"image"
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

// TestCounterFace: the Hit Counter is badge.py's drawing. Its first
// frame, given badge.gif's numbers — 01997, and the day up to the 18:00
// bar five px into it — is badge.gif's first frame, pixel for pixel.
func TestCounterFace(t *testing.T) {
	g, err := gif.DecodeAll(bytes.NewReader(badgeGIF))
	if err != nil {
		t.Fatal(err)
	}
	f := counterFrame{wheels: digitsOf(1997, wheels), bars: [12]int{4, 3, 2, 3, 6, 9, 11, 12, 11, 5, 0, 0}, cur: 9}
	c, want := f.draw(), g.Image[0]
	bad := 0
	for y := range ch {
		for x := range cw {
			r0, g0, b0, _ := counterPal[counterIndex[c[y][x]]].RGBA()
			r1, g1, b1, _ := want.At(x, y).RGBA()
			if r0 != r1 || g0 != g1 || b0 != b1 {
				if bad++; bad <= 5 {
					t.Errorf("(%d, %d) is %q here and not in badge.gif", x, y, c[y][x])
				}
			}
		}
	}
	if bad > 0 {
		t.Fatalf("%d px differ from badge.gif's first frame", bad)
	}
}

// TestCounterLoop: the numbers stand still and only the lamp moves: the
// first frame is the badge at rest and held, every later one changes the
// lamp's box alone. A count past five wheels is told in thousands.
func TestCounterLoop(t *testing.T) {
	day := counterDay{Total: 1234, Bars: [12]int64{3, 1, 0, 2, 9, 30, 41, 52, 40, 12}, Now: 9, Top: 60}
	fs := counterFrames(day)
	if f := fs[0]; f.glow != 0 || f.cs < 200 || fmt.Sprint(f.wheels) != "[0 1 2 3 4]" || f.cur != 9 ||
		f.bars != [12]int{1, 1, 0, 1, 2, 6, 8, 10, 8, 2} {
		t.Fatalf("the first frame: %+v", f)
	}
	lamp := image.Rect(lampX-1, lampY-1, lampX+6, lampY+6)
	for name, d := range map[string]counterDay{"a day": day, "a quiet one": {Total: 7, Now: 5},
		"a big one": {Total: 1234567, Bars: [12]int64{500, 9000}, Now: 1, Top: 9000}} {
		g, err := gif.DecodeAll(bytes.NewReader(drawCounter(d)))
		if err != nil || g.Config.Width != 88 || g.Config.Height != 31 || g.LoopCount != 0 {
			t.Fatalf("%s: %v, %d×%d looping %d", name, err, g.Config.Width, g.Config.Height, g.LoopCount)
		}
		if len(g.Image) < 3 || g.Delay[0] < 200 {
			t.Errorf("%s: %d frames, the first standing %d cs; want the lamp breathing after a rest", name, len(g.Image), g.Delay[0])
		}
		for i, im := range g.Image[1:] {
			if !im.Bounds().In(lamp) {
				t.Errorf("%s: frame %d changes %v, outside the lamp", name, i+1, im.Bounds())
			}
		}
	}
	if f := counterFrames(counterDay{Total: 1234567})[0]; fmt.Sprint(f.wheels) != "[1 2 3 4]" || f.letter == nil {
		t.Errorf("1,234,567 stands on the wheels as %v, letter %v; want 1234 and a K", f.wheels, f.letter)
	}
}
