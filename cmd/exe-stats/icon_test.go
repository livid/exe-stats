package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

// TestIcons: /favicon.ico holds the counter drawn at 16, 24 and 32, each
// a PNG of its own size; the touch icon is 180 square with no
// transparency for iOS to fill with black; and every page links both.
func TestIcons(t *testing.T) {
	h := newHarness(t)
	rec := h.do(nil, "GET", "/favicon.ico", "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/x-icon" {
		t.Fatalf("/favicon.ico answered %d as %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	b := rec.Body.Bytes()
	if len(b) < 6 || binary.LittleEndian.Uint16(b[2:]) != 1 {
		t.Fatal("/favicon.ico is not an icon file")
	}
	var sizes []int
	for i := range int(binary.LittleEndian.Uint16(b[4:])) {
		e := b[6+16*i:]
		n, off := binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])
		im, err := png.Decode(bytes.NewReader(b[off : off+n]))
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if w := im.Bounds().Dx(); w != int(e[0]) || im.Bounds().Dy() != int(e[1]) {
			t.Errorf("frame %d says %d×%d and is %v", i, e[0], e[1], im.Bounds().Size())
		}
		sizes = append(sizes, im.Bounds().Dx())
	}
	if len(sizes) != 3 || sizes[0] != 16 || sizes[1] != 24 || sizes[2] != 32 {
		t.Errorf("the icon is drawn at %v, want [16 24 32]", sizes)
	}

	for _, p := range []string{"/apple-touch-icon.png", "/apple-touch-icon-precomposed.png"} {
		rec := h.do(nil, "GET", p, "", nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("%s answered %d as %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		im, err := png.Decode(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		if im.Bounds().Dx() != 180 || im.Bounds().Dy() != 180 {
			t.Errorf("%s is %v, want 180×180", p, im.Bounds().Size())
		}
		if o, ok := im.(interface{ Opaque() bool }); !ok || !o.Opaque() {
			t.Errorf("%s has transparency, which iOS fills with black", p)
		}
	}

	for _, p := range []string{"/", "/badge/"} {
		got := h.do(nil, "GET", p, "", nil).Body.String()
		if !strings.Contains(got, `<link rel="icon" href="/favicon.ico"`) || !strings.Contains(got, `<link rel="apple-touch-icon" href="/apple-touch-icon.png">`) {
			t.Errorf("%s links no icons", p)
		}
	}
}
