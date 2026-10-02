package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"image/gif"
	"net/http"
)

// The badge: an 88×31 animated GIF (badge.gif, drawn by badge.py) for
// other pages to link here with, and a page of its own at /badge/ that
// hands out the HTML and the Markdown to do it.

//go:embed badge.gif
var badgeGIF []byte

//go:embed badge.html
var badgeHTML string

// badgeFrames and badgeSize are what the page's status line says of the
// GIF, read off the file itself so a new drawing is never described as
// the old.
var badgeFrames, badgeSize = func() (int, string) {
	g, err := gif.DecodeAll(bytes.NewReader(badgeGIF))
	if err != nil {
		panic("badge.gif: " + err.Error())
	}
	return len(g.Image), fmt.Sprintf("%.1f KB", float64(len(badgeGIF))/1000)
}()

// badgeAlt is the badge's words for a reader who cannot see it.
const badgeAlt = "exe-stats: page views for your own sites"

// GET /badge.gif — the badge as other pages link it, kept four hours so
// a new drawing reaches them the same day.
func (a *App) handleBadgeGIF(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "public, max-age=14400")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.Write(badgeGIF)
}

type badgeData struct {
	Host, HTML, MD, Alt, Size string
	Frames                    int
}

// GET /badge/ — the page that hands the badge out. Its snippets name the
// server by its public address, or on a scratch port by the one it was
// asked by.
func (a *App) handleBadgePage(w http.ResponseWriter, r *http.Request) {
	o := a.originOf(r)
	d := badgeData{Host: a.host(r), Alt: badgeAlt, Size: badgeSize, Frames: badgeFrames,
		HTML: `<a href="` + o + `/"><img src="` + o + `/badge.gif" width="88" height="31" alt="` + badgeAlt + `"></a>`,
		MD:   `[![` + badgeAlt + `](` + o + `/badge.gif)](` + o + `/)`}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := a.tmpl.ExecuteTemplate(w, "badge", d); err != nil {
		a.log("badge: %v", err)
	}
}
