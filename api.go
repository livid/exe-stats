// Package stats is server-side analytics for a Go web server's public
// pages: which pages a browser opened, where the visitor came from, from
// what country and on what device — counted as a page is served, with no
// script and no cookie, and read back as a Mac OS 9 Platinum desk at
// /stats or as JSON at /v1/stats.
//
// A visitor is the day's hash of a secret salt, the address and the
// browser: it rotates every midnight in the host's own time zone and
// cannot be turned back into a person, and neither the address nor the
// user agent is ever stored.
//
// The host owns the database. This package opens no file and registers
// no driver — it is handed a *sql.DB, keeps its tables in it (hits and
// hits_salt, and the finished days it has counted in hits_views,
// hits_days and hits_meta) and leaves the rest alone, so a
// server can keep its readers beside its own records:
//
//	st, err := stats.New(db, stats.Options{Location: time.Local})
//	go st.Run()
//	defer st.Stop()
//	mux.Handle("GET /stats", st.PageHandler())
//	mux.Handle("GET /v1/stats", http.HandlerFunc(st.ServeJSON))
//	mux.HandleFunc("GET /{$}", st.Counted("home", home))
//
// A host with pages of its own renders the desk inside its own chrome
// instead: parse TemplateHTML into its template set, hand PageData's
// result to the "stats" block, and put "statshead" in the document head
// with "statscss".
package stats

import (
	_ "embed"

	"context"
	"database/sql"
	"errors"
	"html/template"
	"log"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed document.html
var documentHTML string

//go:embed chrome.css
var chromeCSS string

//go:embed popup.css
var popupCSS string

// ChromeCSS is the Mac OS 9 Platinum chrome the desk is drawn in: the
// window with its striped bar, hard shadow and close box, the sunken
// frame, the status strip and the push button. The standalone page uses
// it, and a host whose own pages should match can serve it as its
// stylesheet — which is how the exe site keeps one chrome across its
// homepage, its documentation and this desk.
func ChromeCSS() string { return chromeCSS }

// PopupCSS is the Platinum pop-up menu button, one block for every page
// that wears the chrome (popup.css): a host links or embeds it, never
// copies it. The template func "popup" is the same for a template.
func PopupCSS() string { return popupCSS }

// Options configures a Stats. Every field may be left empty.
type Options struct {
	// Location is where the days begin: the salt rotates at midnight
	// there and the page's buckets follow it. Default UTC.
	Location *time.Location
	// PathLabel names a path in the ranked lists and the Live window —
	// a hub names a thread by its words, a shop names a product. The
	// default shows the path itself.
	PathLabel func(path string) string
	// Title heads the standalone page's window and its <title>. Default
	// "Stats".
	Title string
	// Log takes the collector's complaints, and says why a report could
	// not be read. Default log.Printf.
	Log func(format string, args ...any)
	// LiveHeader marks a page's own refetch, so a page that keeps itself
	// live is not counted again every time. Default DefaultLiveHeader.
	LiveHeader string
	// AnyClientKinds are the kinds of page counted whatever the client
	// is, for a page tools read as much as browsers do — the hub counts
	// its agent guide, "skill", that way. Every other kind counts only
	// when a browser navigated to it.
	AnyClientKinds []string
	// HomeURL is where the desk's close box leads, and HomeLabel is what
	// it says. Defaults "/" and "Home".
	HomeURL, HomeLabel string
	// Path is where the desk is served and JSONPath where its JSON is:
	// every link the desk writes points at Path, so a host that keeps
	// one desk per site mounts each at a path of its own. Defaults
	// "/stats" and "/v1/stats".
	Path, JSONPath string
	// Reader is a second pool on the same database for the reports to
	// read through, read-only (the database in WAL): a report runs as
	// many of its queries at once as the pool has connections (one a
	// core when it sets no limit), and the writer's connection stays
	// free. Default: the database itself, one query at a time.
	Reader *sql.DB
}

// Stats is the counting and the report: one per database.
type Stats struct {
	db    *DB
	rd    *DB // what reports read through: Options.Reader, else db
	width int // how many of a report's queries run at once: rd's connections
	coll  *Collector
	opt   Options

	mu    sync.Mutex
	cache map[string]cacheEntry

	seal    time.Duration      // how long after its end a day is kept (sealDelay)
	since   int64              // ms: the days before are read from their hits (countedBy)
	views   [16]chan struct{}  // lockView's, each holding one
	ctx     context.Context    // the keeper's and the reports': done once Stop is called
	cancel  context.CancelFunc //
	kept    chan struct{}      // closed when the keeper has returned
	keeping atomic.Bool        // whether Run started the keeper
	tidied  atomic.Int64       // ms: when the views were last trimmed (tidy)
	live    *live              // the last three days' hits, in memory
	noLive  bool               // a test's: read the store

	tmpl *template.Template // the standalone page
}

// New prepares the hits tables in db and returns the analytics over
// them. Counting begins when Run does.
func New(db *sql.DB, o Options) (*Stats, error) {
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.Title == "" {
		o.Title = "Stats"
	}
	if o.Log == nil {
		o.Log = log.Printf
	}
	if o.LiveHeader == "" {
		o.LiveHeader = DefaultLiveHeader
	}
	if o.HomeURL == "" {
		o.HomeURL = "/"
	}
	if o.HomeLabel == "" {
		o.HomeLabel = "Home"
	}
	if o.Path == "" {
		o.Path = "/stats"
	}
	if o.JSONPath == "" {
		o.JSONPath = "/v1/stats"
	}
	if db == nil {
		return nil, errors.New("stats: no database")
	}
	d, err := Open(db)
	if err != nil {
		return nil, err
	}
	c, err := NewCollector(d, o.Location)
	if err != nil {
		return nil, err
	}
	c.Log = o.Log
	since, err := d.countedBy(o.Location, time.Now())
	if err != nil {
		return nil, err
	}
	t, err := template.New("stats").Funcs(Funcs()).Parse(pageHTML)
	if err != nil {
		return nil, err
	}
	if _, err := t.Parse(documentHTML); err != nil {
		return nil, err
	}
	rd, width := d, 1
	if o.Reader != nil {
		rd, width = &DB{db: o.Reader}, o.Reader.Stats().MaxOpenConnections
		if width <= 0 {
			width = runtime.NumCPU()
		}
	}
	s := &Stats{db: d, rd: rd, width: width, coll: c, opt: o, tmpl: t, seal: sealDelay, since: since,
		kept: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.live = &live{}
	d.fed = func(hits []Hit, ids []int64) { s.live.fed(hits, ids, time.Now(), o.Location) }
	d.swept = s.live.swept
	for i := range s.views {
		s.views[i] = make(chan struct{}, 1)
	}
	return s, nil
}

// Run writes the queued hits, and keeps the finished days counted; it
// does not return. Start it in a goroutine once, after New.
func (s *Stats) Run() {
	if s.keeping.CompareAndSwap(false, true) {
		go s.keeper()
	}
	s.coll.Run()
}

// Flush writes what is queued now and lets the next report see it —
// what Run does once a second. It is for a host that is not running Run
// (a test, a seeder); calling it while Run is running is not safe.
func (s *Stats) Flush() error {
	var batch []Hit
	for {
		h, ok := s.coll.take()
		if !ok {
			break
		}
		batch = append(batch, h)
	}
	if len(batch) > 0 {
		if err := s.db.Add(batch); err != nil {
			return err
		}
	}
	s.ResetCache()
	return nil
}

// Wanted says whether a served page counts under this host's policy —
// what Counted asks before recording. A host that records hits itself
// through Collector().Record asks this, not the package-level Wanted,
// which knows nothing of the options.
func (s *Stats) Wanted(r *http.Request, kind string) bool {
	return wanted(r, kind, s.opt.LiveHeader, s.opt.AnyClientKinds)
}

// Stop ends Run, having written the hits it still held, and the keeper,
// having waited for it. A host closing its database calls this first;
// after it, nothing more is recorded, and a report says it was stopped.
func (s *Stats) Stop() {
	s.cancel()
	s.coll.Stop()
	if s.keeping.Load() {
		<-s.kept // cancelled, it returns at once: nothing of it outlives Stop
	}
}

// DB is the hits tables, for a host that wants to query or sweep them.
func (s *Stats) DB() *DB { return s.db }

// Collector is the counting half, for a host that records hits itself
// rather than through Counted.
func (s *Stats) Collector() *Collector { return s.coll }

// Location is where the days begin.
func (s *Stats) Location() *time.Location { return s.opt.Location }

// Online is how many visitors had a page in the last five minutes.
func (s *Stats) Online() int { return s.coll.Online() }

// Sweep drops the hits before a time — a retention policy is the host's
// to run — and says how many went.
func (s *Stats) Sweep(before time.Time) (int64, error) { return s.db.Sweep(before) }

// Counted wraps a page handler so a served page is recorded as a hit,
// once the handler has answered 200 and only for a request that was a
// person opening a page (Wanted). kind names the sort of page — "home",
// "thread", whatever the host calls it — and is kept with the hit.
func (s *Stats) Counted(kind string, h http.HandlerFunc) http.HandlerFunc {
	return s.counted(kind, h)
}

// PageHandler serves the stats desk as a page of its own, in the
// Platinum chrome this package carries. A host with its own chrome uses
// PageData instead.
func (s *Stats) PageHandler() http.Handler { return http.HandlerFunc(s.ServePage) }

// JSONHandler serves the same report as JSON, under the same query.
func (s *Stats) JSONHandler() http.Handler { return http.HandlerFunc(s.ServeJSON) }

// Handler is both, under the paths the page's own links use: GET /stats
// and GET /v1/stats, or Options.Path and Options.JSONPath. It counts
// nothing itself.
func (s *Stats) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+s.opt.Path, s.PageHandler())
	mux.Handle("GET "+s.opt.JSONPath, s.JSONHandler())
	return mux
}

// Funcs are the template functions the page's blocks call. A host that
// parses TemplateHTML into its own set registers these on it first.
func Funcs() template.FuncMap {
	return template.FuncMap{
		"chrome": func() template.CSS { return template.CSS(chromeCSS) },
		"popup":  func() template.CSS { return template.CSS(popupCSS) },
		"mul":    func(a, b int) int { return a * b },
		"pct":    func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
	}
}

// ResetCache drops the kept reports, so the next one is computed afresh.
// A report is cached for a few seconds under its query; a host that has
// just written hits itself and wants them seen at once (a test, a
// seeder) says so here.
func (s *Stats) ResetCache() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

// TemplateHTML is the page's template text — the blocks "statshead"
// (the head script that goes to the range last pressed), "statscss"
// (the style), "stats" (the desk itself, given a *Page) and "disc" (a
// visitor's mark). A host parses it into its own template set to render
// the desk inside its chrome.
func TemplateHTML() string { return pageHTML }

// pathLabelOf names a path for a list row, through the host's hook.
func (s *Stats) pathLabelOf(path string) string {
	if s.opt.PathLabel == nil {
		return path
	}
	return s.opt.PathLabel(path)
}
