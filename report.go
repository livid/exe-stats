package stats

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Stats — the pages' own analytics (PLAN.md, Stats). GET /stats is a
// Platinum window of the numbers: a range of days, the headline tiles
// with their change against the span before, a chart of page views and
// visitors, who is here now, and ranked lists of sources, pages,
// locations and devices; every state is a URL (range, list views,
// stacked filters), so the page works without script and a view can be
// shared as a link. GET /v1/stats is the same report as JSON. The
// counting itself is in internal/stats and the wrap below.

//go:embed page.html
var pageHTML string

// counted wraps a page handler so a served page is recorded as a hit
// (once the handler answered 200, and only for a request that was a
// person opening a page: Wanted). Without a collector the handler
// is returned as it is.
func (s *Stats) counted(kind string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		h(sw, r)
		// a handler that wrote nothing still answered 200, and an
		// informational answer (103 Early Hints) is not the answer
		if sw.status() == http.StatusOK && s.Wanted(r, kind) {
			s.coll.Record(r, kind)
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	// 1xx is a message on the way, not the answer — except 101, where
	// the protocol is handed over and nothing else follows
	if w.code == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

// Flush commits the answer, so whatever the handler says afterwards,
// 200 is what went on the wire. Having it here also keeps http.Flusher
// working for a handler that asserts it.
func (w *statusWriter) Flush() {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack lets a handler take the connection over, as it could without
// this wrapper; a hijacked connection is no page view.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if w.code == 0 {
		w.code = http.StatusSwitchingProtocols
	}
	return http.NewResponseController(w.ResponseWriter).Hijack()
}

// status is what the handler answered: nothing written at all is the
// 200 net/http sends for it.
func (w *statusWriter) status() int {
	if w.code == 0 {
		return http.StatusOK
	}
	return w.code
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ranges are the spans the page offers, in the order of its buttons.
var ranges = []struct{ key, label, compare string }{
	{"today", "Today", "the same hours yesterday"}, {"yesterday", "Yesterday", "the day before"},
	{"24h", "24 hours", "the 24 hours before"}, {"7d", "7 days", "the 7 days before"},
	{"30d", "30 days", "the 30 days before"}, {"3m", "3 months", "the 3 months before"},
	{"6m", "6 months", "the 6 months before"}, {"12m", "12 months", "the 12 months before"},
}

const defaultRange = "7d"

// Span is a resolved range: its bounds, the span before it of the
// same length (what the tiles compare against), and the bucket its
// chart draws in.
type Span struct {
	Key, Label     string
	Compare        string    // what the tiles' change is against, in words
	From, To       time.Time // [From, To)
	PrevFrom       time.Time
	PrevTo         time.Time
	Step           string // hour | day | month
	Buckets        []time.Time
	IncompleteLast bool // the last bucket holds now: still filling
}

// midnight is where a date begins in loc, where the salt turns and with
// it the visitors: its first instant. That is 00:00, or where a clock
// change skips 00:00 the moment it jumps (Santiago's spring goes from
// 23:59 to 01:00), and where one goes back over 00:00 the first of the
// two (Havana's autumn: 00:59, then 00:00 again). d may run past the
// month's end, as time.Date takes it.
func midnight(y int, mo time.Month, d int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	n := time.Date(y, mo, d, 12, 0, 0, 0, loc) // the date, normalised
	on := func(u time.Time) bool {
		u = u.In(loc)
		return u.Year() == n.Year() && u.YearDay() == n.YearDay()
	}
	if !on(t) {
		if _, end := t.ZoneBounds(); !end.IsZero() {
			return end
		}
		return t
	}
	if start, _ := t.ZoneBounds(); !start.IsZero() && t.Sub(start) < 3*time.Hour {
		if c := start.Add(-time.Nanosecond); on(c) {
			_, off := c.Zone()
			return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.FixedZone("", off)).In(loc)
		}
	}
	return t
}

// spanOf resolves a range key at a moment in the collector's zone.
// today and yesterday are calendar days there (today up to now, drawn
// on the whole day's axis), 24h the clock's last day, 7d and 30d that
// many days ending today, 3m/6m/12m calendar months from the first of
// the month that many months back. The span before: the same length,
// ending where this one starts — today's is the same hours of yesterday.
func spanOf(key string, now time.Time, loc *time.Location) Span {
	now = now.In(loc)
	day := midnight(now.Year(), now.Month(), now.Day(), loc)
	month := midnight(now.Year(), now.Month(), 1, loc)
	days := func(t time.Time, n int) time.Time { // the midnight n days on
		t = t.In(loc)
		return midnight(t.Year(), t.Month(), t.Day()+n, loc)
	}
	months := func(t time.Time, n int) time.Time { // the first of the month n months on
		t = t.In(loc)
		return midnight(t.Year(), t.Month()+time.Month(n), 1, loc)
	}
	sp := Span{Key: key}
	for _, r := range ranges {
		if r.key == key {
			sp.Label, sp.Compare = r.label, r.compare
		}
	}
	end := now.Add(time.Millisecond) // the current millisecond included
	switch key {
	case "today":
		sp.From, sp.To, sp.Step = day, end, "hour"
		sp.PrevFrom, sp.PrevTo = days(day, -1), end.AddDate(0, 0, -1)
	case "yesterday":
		sp.From, sp.To, sp.Step = days(day, -1), day, "hour"
		sp.PrevFrom, sp.PrevTo = days(day, -2), days(day, -1)
	case "24h":
		sp.From, sp.To, sp.Step = now.Add(-24*time.Hour), end, "hour"
		sp.PrevFrom, sp.PrevTo = sp.From.Add(-24*time.Hour), sp.From
	case "30d":
		sp.From, sp.To, sp.Step = days(day, -29), end, "day"
		sp.PrevFrom, sp.PrevTo = days(sp.From, -30), sp.From
	case "3m":
		sp.From, sp.To, sp.Step = months(month, -2), end, "day"
		sp.PrevFrom, sp.PrevTo = months(sp.From, -3), sp.From
	case "6m":
		sp.From, sp.To, sp.Step = months(month, -5), end, "month"
		sp.PrevFrom, sp.PrevTo = months(sp.From, -6), sp.From
	case "12m":
		sp.From, sp.To, sp.Step = months(month, -11), end, "month"
		sp.PrevFrom, sp.PrevTo = months(sp.From, -12), sp.From
	default: // 7d
		sp.Key, sp.Label, sp.Compare = "7d", "7 days", "the 7 days before"
		sp.From, sp.To, sp.Step = days(day, -6), end, "day"
		sp.PrevFrom, sp.PrevTo = days(sp.From, -7), sp.From
	}
	// the buckets: whole hours, days or months from the span's start up
	// to its end — today and 24h draw the whole day's axis, so the
	// chart's shape is the same all day
	axisEnd := sp.To
	switch key {
	case "today":
		axisEnd = days(day, 1)
	case "24h":
		axisEnd = sp.From.Add(24 * time.Hour)
	}
	var b time.Time
	switch sp.Step {
	case "hour":
		b = time.Date(sp.From.Year(), sp.From.Month(), sp.From.Day(), sp.From.Hour(), 0, 0, 0, loc)
	case "day":
		b = days(sp.From, 0)
	default:
		b = months(sp.From, 0)
	}
	for b.Before(axisEnd) {
		sp.Buckets = append(sp.Buckets, b)
		switch sp.Step {
		case "hour":
			b = b.Add(time.Hour)
		case "day":
			b = days(b, 1)
		default:
			b = months(b, 1)
		}
	}
	sp.IncompleteLast = key != "yesterday" // every other span reaches now: its last bucket is still filling
	return sp
}

// filters are the dimensions a view can be held to, as the query
// names them; the order is the filter row's.
var filters = []struct{ param, label string }{
	{"country", "Country"}, {"region", "Region"}, {"city", "City"}, {"lang", "Language"},
	{"page", "Page"}, {"source", "Source"}, {"channel", "Channel"}, {"campaign", "Campaign"},
	{"device", "Device"}, {"browser", "Browser"}, {"os", "OS"}, {"bot", "Bot"},
}

func filterOf(q url.Values) Filter {
	get := func(k string) string {
		v := q.Get(k)
		if len(v) > 256 {
			v = v[:256]
		}
		return v
	}
	return Filter{
		Country: get("country"), Region: get("region"), City: get("city"), Lang: get("lang"),
		Path: get("page"), Source: get("source"), Channel: get("channel"), Campaign: get("campaign"),
		Device: get("device"), Browser: get("browser"), OS: get("os"), Bot: get("bot"),
	}
}

func value(f Filter, param string) string {
	switch param {
	case "country":
		return f.Country
	case "region":
		return f.Region
	case "city":
		return f.City
	case "lang":
		return f.Lang
	case "page":
		return f.Path
	case "source":
		return f.Source
	case "channel":
		return f.Channel
	case "campaign":
		return f.Campaign
	case "device":
		return f.Device
	case "browser":
		return f.Browser
	case "os":
		return f.OS
	case "bot":
		return f.Bot
	}
	return ""
}

// Point is one bucket of the chart.
type Point struct {
	T         string `json:"t"` // the bucket's start, RFC 3339 in the zone
	Pageviews int    `json:"pageviews"`
	Visitors  int    `json:"visitors"`
	Label     string `json:"-"` // the axis label, "" for a bucket without one
	Future    bool   `json:"-"` // after now: on the axis, without a point
	start     time.Time
}

// Live is who is here now: the online count and the latest pages.
type Live struct {
	Online int       `json:"online"`
	Recent []LiveRow `json:"recent"`
}

type LiveRow struct {
	Alias   string `json:"alias"`
	Colour  string `json:"colour"` // the alias's colour, the disc before the name
	Country string `json:"country,omitempty"`
	Path    string `json:"path"`
	Label   string `json:"label,omitempty"`
	Device  string `json:"device,omitempty"`
	TS      string `json:"ts"`
	Ago     string `json:"-"`
}

// Report is one view of the numbers, for the page and the JSON.
type Report struct {
	Range    string            `json:"range"`
	From     string            `json:"from"`
	To       string            `json:"to"`
	Zone     string            `json:"zone"`
	Step     string            `json:"step"`
	Filters  map[string]string `json:"filters,omitempty"`
	Summary  Summary           `json:"summary"`
	Previous Summary           `json:"previous"`
	Series   []Point           `json:"series"`
	Lists    map[string][]Row  `json:"lists"`
	Live     Live              `json:"live"`
	span     Span
	filter   Filter
}

// lists are the ranked lists by the view name the page uses, with
// the store dimension and what the count is.
var listDims = map[string]struct{ dim, count string }{
	"sources": {"source", "Sessions"}, "channels": {"channel", "Sessions"}, "campaigns": {"campaign", "Sessions"},
	"pages": {"path", "Visitors"}, "entry": {"entry", "Sessions"}, "exit": {"exit", "Sessions"},
	"countries": {"country", "Visitors"}, "regions": {"region", "Visitors"}, "cities": {"city", "Visitors"}, "languages": {"lang", "Visitors"},
	"devices": {"device", "Visitors"}, "browsers": {"browser", "Visitors"}, "oses": {"os", "Visitors"},
	"crawlers": {"crawler", "Page views"}, "botpages": {"botpath", "Page views"},
}

// recentN is how many latest page views the Live list shows.
const recentN = 10

// clone is a report of the caller's own: the cached one is shared with
// every request in its few seconds, so what is handed out may not be
// the same maps and slices.
func (r *Report) clone() *Report {
	if r == nil {
		return nil
	}
	c := *r
	c.Series = append([]Point(nil), r.Series...)
	c.Live.Recent = append([]LiveRow(nil), r.Live.Recent...)
	c.Lists = make(map[string][]Row, len(r.Lists))
	for k, v := range r.Lists {
		c.Lists[k] = append([]Row(nil), v...)
	}
	c.Filters = make(map[string]string, len(r.Filters))
	for k, v := range r.Filters {
		c.Filters[k] = v
	}
	return &c
}

// build computes a report: the span's numbers and the span before,
// the series, the lists asked for (nil = all of them) and the live part.
func (s *Stats) build(sp Span, f Filter, lists []string, now time.Time) (*Report, error) {
	loc := sp.From.Location()
	r := &Report{Range: sp.Key, From: sp.From.Format(time.RFC3339), To: sp.To.Format(time.RFC3339),
		Zone: loc.String(), Step: sp.Step, Lists: map[string][]Row{}, span: sp, filter: f}
	if lists == nil {
		for name := range listDims {
			lists = append(lists, name)
		}
		sort.Strings(lists)
	}
	w := want{hours: true, firsts: sp.Step != "hour"}
	for _, name := range lists {
		if l, ok := listDims[name]; ok && !slices.Contains(w.dims, l.dim) {
			w.dims = append(w.dims, l.dim)
		}
	}
	var cur, prev counts
	var online int
	var recent []LiveHit
	if err := s.par(
		func() (err error) { cur, err = s.over(f, sp.From, sp.To, now, w); return err },
		func() (err error) { prev, err = s.over(f, sp.PrevFrom, sp.PrevTo, now, want{}); return err },
		func() (err error) { online, err = s.rd.Online(now.Add(-OnlineWindow).UnixMilli()); return err },
		func() (err error) { recent, err = s.rd.Recent(recentN); return err },
	); err != nil {
		return nil, err
	}
	if now.UnixMilli()-s.tidied.Load() > 24*3600*1000 {
		s.tidy(now) // a day since: without Run, nothing else does
	}
	r.Summary, r.Previous = cur.summary(), prev.summary()
	r.Filters = map[string]string{}
	for _, d := range filters {
		if v := value(f, d.param); v != "" {
			r.Filters[d.param] = v
		}
	}
	r.Series = series(sp, cur, now)
	for _, name := range lists {
		if l, ok := listDims[name]; ok {
			r.Lists[name] = ranked(cur.lists[l.dim])
		}
	}
	r.Live = Live{Online: online, Recent: []LiveRow{}}
	for _, h := range recent {
		r.Live.Recent = append(r.Live.Recent, LiveRow{
			Alias: Alias(h.VID), Colour: Colour(h.VID), Country: h.Country, Path: h.Path, Device: h.Device,
			TS: time.UnixMilli(h.TS).In(loc).Format(time.RFC3339), Ago: ago(now.Sub(time.UnixMilli(h.TS))),
		})
	}
	return r, nil
}

// series buckets the span's hours (page views, and visitors within
// the hour) and where its visitors begin (one per visitor-day) into the
// span's buckets: an hour bucket takes its hour's visitors, a day or
// month bucket counts the visitors whose day falls in it.
func series(sp Span, c counts, now time.Time) []Point {
	pts := make([]Point, len(sp.Buckets))
	for i, b := range sp.Buckets {
		pts[i].start = b
		pts[i].T = b.Format(time.RFC3339)
		pts[i].Future = !b.Before(now)
	}
	at := func(t time.Time) int {
		// the last bucket starting at or before t
		i := sort.Search(len(sp.Buckets), func(i int) bool { return sp.Buckets[i].After(t) }) - 1
		return i
	}
	for _, h := range c.hours {
		// the hours come out of the store in UTC; where a zone is offset
		// by half an hour a bucket begins inside one, so place an hour by
		// its middle, and let an hour straddling the span's start count
		// in the first bucket rather than in none
		mid := time.UnixMilli(h.Hour*3600000 + 1800000)
		i := at(mid)
		if i < 0 && mid.After(sp.From) {
			i = 0
		}
		if i >= 0 {
			pts[i].Pageviews += h.Pageviews
			if sp.Step == "hour" {
				pts[i].Visitors += h.Visitors
			}
		}
	}
	if sp.Step != "hour" {
		for _, v := range c.firsts {
			if i := at(time.UnixMilli(v.at)); i >= 0 {
				pts[i].Visitors += v.n
			}
		}
	}
	// axis labels: hours every 4th (or 6th past 24), days every one up
	// to 10 buckets, then every 5th, months each
	n := len(pts)
	for i := range pts {
		b := pts[i].start
		switch sp.Step {
		case "hour":
			if i%4 == 0 {
				pts[i].Label = b.Format("15:04")
			}
		case "day":
			switch {
			case n <= 10:
				pts[i].Label = b.Format("Jan 2")
			case n <= 40 && i%5 == 0:
				pts[i].Label = b.Format("Jan 2")
			case n > 40 && b.Day() == 1:
				pts[i].Label = b.Format("Jan 2")
			}
		default:
			pts[i].Label = b.Format("Jan")
			if b.Month() == time.January || i == 0 {
				pts[i].Label = b.Format("Jan 2006")
			}
		}
	}
	return pts
}

// Today is today's chart: a point an hour from the zone's midnight to
// the next, the hours still to come Future, page views and visitors of
// people alone — read from memory where it can be, so a list of sites
// can draw each one's day.
func (s *Stats) Today(now time.Time) ([]Point, error) {
	sp := spanOf("today", now, s.coll.Location())
	c, err := s.over(Filter{}, sp.From, sp.To, now, want{hours: true})
	if err != nil {
		return nil, err
	}
	return series(sp, c, now), nil
}

// Yesterday is the day before's chart as Today draws today's: a point
// an hour, midnight to midnight, people alone. It is a day that has
// ended, so it is read from the days kept (and from the hits until it is
// kept, a few minutes past midnight).
func (s *Stats) Yesterday(now time.Time) ([]Point, error) {
	sp := spanOf("yesterday", now, s.coll.Location())
	c, err := s.over(Filter{}, sp.From, sp.To, now, want{hours: true})
	if err != nil {
		return nil, err
	}
	return series(sp, c, now), nil
}

func ago(d time.Duration) string {
	switch {
	case d < 5*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

// ---- the page ----

// Tile is one headline number and its change against the span before.
type Tile struct {
	Label, Value, Delta string
}

// List is one ranked-list window as the page draws it: its views
// (the small buttons along its strip), the rows with bar widths, and
// what the count is.
type List struct {
	Title    string
	Anchor   string // the window's id: what a link lands on without script
	Views    []View
	Rows     []RowView
	Count    string // "Visitors" | "Sessions" | "Page views"
	More     int    // rows past those shown
	Empty    string
	Bots     bool   // the Bots window
	OnlyBots string // the Bots window, unfiltered: the link that holds the whole view to crawlers
}

type View struct {
	Key, Label, URL string
	On              bool
}

type RowView struct {
	Label, Title, URL string
	N                 string
	Pct               int    // the bar, per cent of the top row
	Tag               string // a country code beside its name
}

// Page is the template's world for /
type Page struct {
	Report    *Report
	Ranges    []View
	Default   string // the range of a URL that names none: the remembered one need not replace it
	Filters   []Chip
	Tiles     []Tile
	Chart     Chart
	Lanes     [][]List // the list windows, dealt two abreast (lanes)
	Span      string   // the span in words, the chart's status line
	Zone      string
	ZoneAbbr  string // the zone as the clock says it now: PDT
	JSON      string // the same view as JSON
	Online    int
	Recent    []LiveRow
	Title     string // the desk's name (Options.Title)
	Host      string // the host it was served for
	HomeURL   string // where the close box leads (Options.HomeURL)
	HomeLabel string
	Path      string // where the desk is served (Options.Path)
	// LiveHeader is the mark the page's own refetches wear so they are
	// not counted as visits (Options.LiveHeader).
	LiveHeader string
}

type Chip struct {
	Label, Value, URL string
}

// Chart is the chart's geometry: two polylines in a 1000×1000 box
// stretched to the plot (strokes non-scaling, so they stay 2px), the
// same buckets as bars, the y-axis labels and the x-axis labels with
// their positions. The page draws the lines or the bars, whichever the
// reader last pressed; both come with every page.
type Chart struct {
	Views, Visitors  string // "x,y x,y …" up to the last complete bucket
	ViewsEnd, VisEnd string // the segment into the incomplete bucket
	Bars             []Bar
	Y                []string
	X                []XLabel
	Max              int
	Empty            bool
}

// Bar is one bucket as a pair of bars, page views and visitors, each
// in thousandths of the chart's top, and what the tip over it says.
type Bar struct {
	Views, Visitors int
	Filling         bool   // the bucket still filling, drawn lighter
	Future          bool   // after now: a slot on the axis, no bars
	When            string // the bucket's time in words: "Oct 2, 14:00 – 15:00"
	Count           string // "1,234 page views · 567 visitors"
}

type XLabel struct {
	Label string
	Pct   float64 // where the lines put its bucket, per cent of the plot
	Mid   float64 // the middle of its bucket's slot, where the bars stand
	Edge  string  // "first" | "last" | ""
}

// shown caps a list on the page; the JSON has them all.
const shown = 12

// A list window's height is known before it is drawn: its chrome, 20px
// a row, one line when the list is empty, and the margin under it
// (html; measured in the browser, 92 + 20n).
const (
	winChrome = 92 // titlebar 17, the window's and the frame's edges 8, strip 37, the rows' padding 14, status line 16
	winRow    = 20
	winNone   = 22 // the line an empty list says
	winGap    = 22 // the margin under a window
)

// lanes deals the list windows into the two lanes they stand in
// once there is room. A lane is a run of the reading order: the first
// few windows left, the rest right, cut where the two lanes' heights
// come closest (the left one the taller on a tie) — so Devices climbs
// up under a short lane instead of leaving a hole beside Locations, and
// the order the page is written in is the order the eye reads at every
// width: down the left lane and then the right, or down a phone's one
// column. Tab and a screen reader step through the markup, so the two
// must not disagree — a shorter-lane-first packing dealt the windows
// out of order and put them back with the `order` property, which moves
// the picture and not the sequence (Codex caught it, 2026-09-17). The
// server cuts because it can: it knows every window's height, so no
// script measures and nothing moves after the first paint. (CSS Grid
// Level 3's `display: grid-lanes` packs without the server, but only
// Safari 26.4 has it; Chrome and Firefox keep it behind a flag, so the
// page does not lean on it.)
func lanes(lists []List) [][]List {
	tall := make([]int, len(lists))
	all := 0
	for i, l := range lists {
		tall[i] = winChrome + winRow*len(l.Rows) + winGap
		if len(l.Rows) == 0 {
			tall[i] += winNone
		}
		all += tall[i]
	}
	cut, best, left := len(lists), all+1, 0
	for k := 1; k < len(lists); k++ { // both lanes hold a window when there are two
		left += tall[k-1]
		apart := left - (all - left)
		if apart < 0 {
			apart = -apart
		}
		if apart <= best {
			cut, best = k, apart
		}
	}
	return [][]List{lists[:cut], lists[cut:]}
}

// listViews are each window's views, in strip order.
var listViews = []struct {
	param, title string
	views        []struct{ key, label string }
}{
	{"src", "Sources", []struct{ key, label string }{{"sources", "Sources"}, {"channels", "Channels"}, {"campaigns", "Campaigns"}}},
	{"pg", "Pages", []struct{ key, label string }{{"pages", "Top"}, {"entry", "Entry"}, {"exit", "Exit"}}},
	{"loc", "Locations", []struct{ key, label string }{{"countries", "Countries"}, {"regions", "Regions"}, {"cities", "Cities"}, {"languages", "Languages"}}},
	{"dev", "Devices", []struct{ key, label string }{{"devices", "Device"}, {"browsers", "Browser"}, {"oses", "OS"}}},
	{"bt", "Bots", []struct{ key, label string }{{"crawlers", "Crawlers"}, {"botpages", "Pages"}}},
}

// Query is the page's URL state: the range, each window's view and
// the filters; withParam and withoutParam derive a neighbouring view's
// URL from it.
type Query struct {
	v    url.Values
	base string // the desk's path (Options.Path)
}

func queryOf(q url.Values, base string) Query {
	keep := url.Values{}
	for _, k := range []string{"range", "src", "pg", "loc", "dev", "bt"} {
		if v := q.Get(k); v != "" {
			keep.Set(k, v)
		}
	}
	for _, d := range filters {
		if v := q.Get(d.param); v != "" {
			keep.Set(d.param, v)
		}
	}
	return Query{keep, base}
}

func (q Query) with(k, v string) string {
	c := url.Values{}
	for key, vals := range q.v {
		c[key] = vals
	}
	if v == "" {
		c.Del(k)
	} else {
		c.Set(k, v)
	}
	if len(c) == 0 {
		return q.base
	}
	return q.base + "?" + c.Encode()
}

func (q Query) String() string { return q.with("", "") }

// withBot is with, over the crawlers' hits: a click in the Bots window
// holds the view to bots (all of them, unless one is held already).
func (q Query) withBot(k, v string) string {
	if q.v.Get("bot") != "" {
		return q.with(k, v)
	}
	c := Query{url.Values{}, q.base}
	for key, vals := range q.v {
		c.v[key] = vals
	}
	c.v.Set("bot", "all")
	return c.with(k, v)
}

// PageData builds the desk's view model for the request's query — the
// range, the filters and each window's view are all in the URL. A host
// renders it with the "stats" template block inside its own chrome;
// ServePage renders it in this package's.
func (s *Stats) PageData(r *http.Request) (*Page, error) {
	q := r.URL.Query()
	now := time.Now()
	sp := spanOf(q.Get("range"), now, s.coll.Location())
	f := filterOf(q)
	sq := queryOf(q, s.opt.Path)
	// each window's view, the first when the query names none or a stranger
	var want []string
	views := map[string]string{}
	for _, lv := range listViews {
		v := q.Get(lv.param)
		ok := false
		for _, o := range lv.views {
			ok = ok || o.key == v
		}
		if !ok {
			v = lv.views[0].key
		}
		views[lv.param] = v
		want = append(want, v)
	}
	rep, err := s.cached(sp, f, want, now)
	if err != nil {
		return nil, err
	}
	// Recent is labelled below and the report behind it is cached and
	// shared with every other request in the window: label a copy
	p := &Page{Report: rep.clone(), Default: defaultRange, Online: rep.Live.Online,
		Recent: append([]LiveRow(nil), rep.Live.Recent...), Zone: rep.Zone,
		ZoneAbbr: now.In(s.coll.Location()).Format("MST")}
	p.JSON = s.opt.JSONPath + strings.TrimPrefix(sq.String(), s.opt.Path)
	for _, rg := range ranges {
		p.Ranges = append(p.Ranges, View{Key: rg.key, Label: rg.label, URL: sq.with("range", rg.key), On: rg.key == sp.Key})
	}
	for _, d := range filters {
		if v := value(f, d.param); v != "" {
			label := v
			if d.param == "country" {
				label = CountryName(v)
			}
			if d.param == "bot" && v == "all" {
				label = "all crawlers"
			}
			p.Filters = append(p.Filters, Chip{Label: d.label, Value: label, URL: sq.with(d.param, "")})
		}
	}
	p.Tiles = tiles(rep.Summary, rep.Previous)
	p.Chart = chartOf(rep.Series, sp)
	var lists []List
	for _, lv := range listViews {
		l := List{Title: lv.title, Anchor: "w-" + lv.param}
		for _, o := range lv.views {
			l.Views = append(l.Views, View{Key: o.key, Label: o.label, URL: sq.with(lv.param, o.key), On: o.key == views[lv.param]})
		}
		view := views[lv.param]
		l.Count = listDims[view].count
		rows := rep.Lists[view]
		if len(rows) > shown {
			l.More = len(rows) - shown
			rows = rows[:shown]
		}
		top := 1
		if len(rows) > 0 {
			top = max(1, rows[0].N)
		}
		for _, row := range rows {
			l.Rows = append(l.Rows, s.rowView(view, row, top, sq))
		}
		if len(rows) == 0 {
			l.Empty = "Nothing yet."
			if view == "campaigns" {
				l.Empty = "No tagged links yet: a link with utm_campaign shows here."
			}
			if view == "regions" || view == "cities" {
				l.Empty = "Not known: the proxy in front sends no region or city."
			}
			if view == "crawlers" || view == "botpages" {
				l.Empty = "No crawler yet."
			}
		}
		if lv.param == "bt" {
			l.Bots = true
			if f.Bot == "" {
				l.OnlyBots = sq.with("bot", "all") + "#w-bt"
			}
		}
		lists = append(lists, l)
	}
	p.Lanes = lanes(lists)
	for i := range p.Recent {
		p.Recent[i].Label = s.pathLabelOf(p.Recent[i].Path)
	}
	p.Span = spanWords(sp)
	p.Title, p.Host = s.opt.Title, r.Host
	p.HomeURL, p.HomeLabel, p.LiveHeader, p.Path = s.opt.HomeURL, s.opt.HomeLabel, s.opt.LiveHeader, s.opt.Path
	return p, nil
}

// ServePage answers with the desk as a page of its own.
func (s *Stats) ServePage(w http.ResponseWriter, r *http.Request) {
	p, err := s.PageData(r)
	if err != nil {
		s.opt.Log("stats: %v", err)
		http.Error(w, "the stats could not be read", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := s.tmpl.ExecuteTemplate(w, "document", p); err != nil {
		s.opt.Log("stats: %v", err)
	}
}

// handleStats is the report as JSON: every list, the series and who is
// here now, under the same query as the page.
func (s *Stats) ServeJSON(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	sp := spanOf(q.Get("range"), now, s.coll.Location())
	rep, err := s.cached(sp, filterOf(q), nil, now)
	if err != nil {
		s.opt.Log("stats: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "the stats could not be read"})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(rep)
}

// cache keeps a computed report for a few seconds under its query,
// so the page's refetches and a busy moment cost the store one pass.
const cacheTTL = 10 * time.Second

type cacheEntry struct {
	at  time.Time
	rep *Report
}

func (s *Stats) cached(sp Span, f Filter, lists []string, now time.Time) (*Report, error) {
	k, _ := json.Marshal(struct {
		Span   string
		Filter Filter
		Lists  []string
	}{sp.Key, f, lists})
	key := string(k)
	s.mu.Lock()
	if e, ok := s.cache[key]; ok && now.Sub(e.at) < cacheTTL {
		s.mu.Unlock()
		return e.rep, nil
	}
	s.mu.Unlock()
	rep, err := s.build(sp, f, lists, now)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.cache == nil || len(s.cache) > 64 {
		s.cache = map[string]cacheEntry{}
	}
	s.cache[key] = cacheEntry{at: now, rep: rep}
	s.mu.Unlock()
	return rep, nil
}

func tiles(cur, prev Summary) []Tile {
	pct := func(a, b int) string {
		if b == 0 {
			return ""
		}
		d := float64(a-b) / float64(b) * 100
		return signed(d, "%")
	}
	bounce := func(s Summary) float64 {
		if s.Sessions == 0 {
			return 0
		}
		return float64(s.Bounces) / float64(s.Sessions) * 100
	}
	tiles := []Tile{
		{"Visitors", num(cur.Visitors), pct(cur.Visitors, prev.Visitors)},
		{"Page views", num(cur.Pageviews), pct(cur.Pageviews, prev.Pageviews)},
		{"Sessions", num(cur.Sessions), pct(cur.Sessions, prev.Sessions)},
		{"Bounce rate", fmt.Sprintf("%.0f%%", bounce(cur)), ""},
		{"Session time", dur(cur.Duration), ""},
	}
	if prev.Sessions > 0 && cur.Sessions > 0 {
		tiles[3].Delta = signed(bounce(cur)-bounce(prev), " pp")
	}
	if prev.Duration > 0 && cur.Sessions > 0 {
		tiles[4].Delta = signed((cur.Duration-prev.Duration)/prev.Duration*100, "%")
	}
	return tiles
}

func signed(d float64, unit string) string {
	switch {
	case d > 0.05:
		return fmt.Sprintf("+%.1f%s", d, unit)
	case d < -0.05:
		return fmt.Sprintf("−%.1f%s", -d, unit)
	}
	return "±0" + unit
}

// num prints a count with thousands separators.
func num(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func dur(sec float64) string {
	s := int(sec + 0.5)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm %02ds", s/60, s%60)
}

// ceil is the chart's top: the smallest of a set of round numbers,
// each divisible by four so the quarter lines label whole numbers.
func ceil(m int) int {
	if m <= 4 {
		return 4
	}
	for k := 1; k < 1e9; k *= 10 {
		for _, b := range []int{4, 8, 12, 16, 20, 32, 40, 60, 80} {
			if b*k >= m {
				return b * k
			}
		}
	}
	return m
}

func chartOf(pts []Point, sp Span) Chart {
	var c Chart
	m := 0
	for _, p := range pts {
		m = max(m, p.Pageviews, p.Visitors)
	}
	c.Empty = m == 0
	c.Max = ceil(m)
	for i := 4; i >= 0; i-- {
		c.Y = append(c.Y, num(c.Max*i/4))
	}
	n := len(pts)
	x := func(i int) float64 {
		if n <= 1 {
			return 0
		}
		return float64(i) * 1000 / float64(n-1)
	}
	y := func(v int) float64 { return 1000 - float64(v)*1000/float64(c.Max) }
	// the points up to the last with data (the buckets after now stay empty)
	last := -1
	for i, p := range pts {
		if !p.Future {
			last = i
		}
	}
	end := last
	if sp.IncompleteLast && last > 0 {
		end = last - 1 // the filling bucket hangs off the line, lighter
	}
	var vw, vi strings.Builder
	for i := 0; i <= end; i++ {
		fmt.Fprintf(&vw, "%.1f,%.1f ", x(i), y(pts[i].Pageviews))
		fmt.Fprintf(&vi, "%.1f,%.1f ", x(i), y(pts[i].Visitors))
	}
	c.Views, c.Visitors = strings.TrimSpace(vw.String()), strings.TrimSpace(vi.String())
	if end >= 0 && last > end {
		c.ViewsEnd = fmt.Sprintf("%.1f,%.1f %.1f,%.1f", x(end), y(pts[end].Pageviews), x(last), y(pts[last].Pageviews))
		c.VisEnd = fmt.Sprintf("%.1f,%.1f %.1f,%.1f", x(end), y(pts[end].Visitors), x(last), y(pts[last].Visitors))
	}
	// the bars: a slot a bucket, the thousandths rounded up so a bucket
	// with anything in it shows something
	h := func(v int) int { return (v*1000 + c.Max - 1) / c.Max }
	for i, p := range pts {
		b := Bar{Views: h(p.Pageviews), Visitors: h(p.Visitors), Filling: sp.IncompleteLast && i == last, Future: i > last}
		if !b.Future {
			b.When = whenOf(sp, p.start)
			if b.Filling {
				b.When += " · still filling"
			}
			b.Count = counted(p.Pageviews, "page view") + " · " + counted(p.Visitors, "visitor")
		}
		c.Bars = append(c.Bars, b)
	}
	for i, p := range pts {
		if p.Label == "" {
			continue
		}
		l := XLabel{Label: p.Label, Pct: x(i) / 10, Mid: (float64(i) + .5) * 100 / float64(n)}
		if i == 0 {
			l.Edge = "first"
		} else if i == n-1 {
			l.Edge = "last"
		}
		c.X = append(c.X, l)
	}
	return c
}

// whenOf says a bucket's time for the tip over its bars: an hour as the
// hour it spans (24 hours' first from where the span begins, inside
// it), a day as its date, a month by name.
func whenOf(sp Span, b time.Time) string {
	switch sp.Step {
	case "hour":
		from := b
		if from.Before(sp.From) {
			from = sp.From
		}
		return from.Format("Jan 2, 15:04") + " – " + b.Add(time.Hour).Format("15:04")
	case "day":
		return b.Format("Mon, Jan 2")
	}
	return b.Format("January 2006")
}

// counted is a count and its noun: "1 visitor", "1,234 page views".
func counted(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return num(n) + " " + noun + "s"
}

// RowView draws one row: the label (a country by name with its
// code, a thread by its words, a profile by its name), the click that
// filters the view by it, and the bar as a share of the top row.
func (s *Stats) rowView(view string, row Row, top int, sq Query) RowView {
	v := RowView{Label: row.Key, N: num(row.N), Pct: row.N * 100 / top}
	param := ""
	switch view {
	case "sources", "channels":
		if row.Key == "" {
			v.Label = "Direct"
			if view == "channels" {
				v.Label = "direct"
			}
		}
		param = "source"
		if view == "channels" {
			param = "channel"
			if row.Key != "" {
				v.Label = row.Key
			}
		}
		if row.Key == "" && view == "channels" {
			param = "channel"
			v.URL = sq.with(param, "direct")
		}
	case "campaigns":
		param = "campaign"
	case "pages", "entry", "exit":
		param = "page"
		v.Title = row.Key
		v.Label = s.pathLabelOf(row.Key)
	case "countries":
		param = "country"
		v.Label, v.Tag = CountryName(row.Key), row.Key
	case "regions":
		param = "region"
	case "cities":
		param = "city"
	case "languages":
		param = "lang"
	case "devices":
		param = "device"
	case "browsers":
		param = "browser"
	case "oses":
		param = "os"
	case "crawlers":
		v.URL = sq.with("bot", row.Key) // that crawler alone, wherever the view was
	case "botpages":
		v.Title = row.Key
		v.Label = s.pathLabelOf(row.Key)
		v.URL = sq.withBot("page", row.Key)
	}
	if v.URL == "" && param != "" {
		v.URL = sq.with(param, row.Key)
	}
	return v
}

// spanWords is the chart's status line: the span's dates and what
// the tiles compare against.
func spanWords(sp Span) string {
	loc := sp.From.Location()
	last := sp.To.Add(-time.Millisecond).In(loc)
	from := sp.From.In(loc)
	var span string
	switch sp.Key {
	case "today", "yesterday":
		span = from.Format("Jan 2, 2006")
	case "24h":
		span = from.Format("Jan 2 15:04") + " – " + last.Format("Jan 2 15:04")
	default:
		span = from.Format("Jan 2") + " – " + last.Format("Jan 2, 2006")
	}
	return span + " \u00b7 change vs " + sp.Compare
}
