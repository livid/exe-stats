package main

import (
	_ "embed"
	"net/http"
)

// The server's icons, drawn by icon.py beside them: a chrome tally
// counter whose last wheel is the visitors' orange. favicon.ico holds it
// drawn at 16, 24 and 32 for a tab at 100, 150 and 200 percent;
// apple-touch-icon.png is the 32 at 5x on the desktop's lavender, for a
// home screen. They sit at the paths browsers ask for unasked, so the
// desk, which is the stats package's page and links neither, has them
// too.

//go:embed favicon.ico
var faviconICO []byte

//go:embed apple-touch-icon.png
var touchIconPNG []byte

// serveIcon answers with an icon, kept a day.
func serveIcon(kind string, b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", kind)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(b)
	}
}
