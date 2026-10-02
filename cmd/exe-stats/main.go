// exe-stats is the stats package as a service: sign in with a Solana
// wallet, add a site, paste one script tag, and read that site's page
// views on a desk of its own — the same desk the exe hub and the exe
// site keep over their own readers.
//
// How many sites and how many views a month an account gets follows the
// tokens its wallet holds (tier.go). Holding is read over RPC and
// nothing is ever signed but the sign-in message.
//
//	exe-stats -config config.json
//
// The server keeps everything under one directory: app.db (accounts,
// sessions, sites, usage) and sites/<id>.db, one SQLite file a site.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	stats "github.com/livid/exe-stats"
	_ "modernc.org/sqlite"
)

// Config is config.json. Every field has a default, so an empty file
// (or none) runs a server on 127.0.0.1:7795 over ./data.
type Config struct {
	Listen string `json:"listen"`
	Data   string `json:"data"`
	// Origin is the public address, https://stats.v2core.com: the
	// snippet names it, the sign-in message names its host, and a
	// request that changes something must come from it. Empty on a
	// scratch port: the request's own host stands in.
	Origin string `json:"origin"`
	// RPCURL may carry an API key; it is never logged or shown.
	RPCURL   string `json:"rpc_url"`
	Mint     string `json:"mint"`
	Decimals int    `json:"decimals"`
	Token    string `json:"token"` // the token's name on the pages
	Tiers    []Tier `json:"tiers"`
	// The three numbers of the grace design.
	GraceDays      int `json:"grace_days"`       // a short balance keeps its tier this long
	GraceEveryDays int `json:"grace_every_days"` // one grace in this long
	OverFactor     int `json:"over_factor"`      // a first month over counts up to this many caps
}

func defaults(c Config) Config {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7795"
	}
	if c.Data == "" {
		c.Data = "data"
	}
	if c.RPCURL == "" {
		c.RPCURL = "https://api.mainnet-beta.solana.com"
	}
	if c.Mint == "" {
		c.Mint, c.Decimals = "9raUVuzeWUk53co63M4WXLWPWE4Xc6Lpn7RS9dnkpump", 6
	}
	if c.Token == "" {
		c.Token = "$V2EX"
	}
	if len(c.Tiers) == 0 {
		c.Tiers = []Tier{
			{Name: "Base", Hold: 10000, Sites: 2, Views: 10000},
			{Name: "Middle", Hold: 50000, Sites: 5, Views: 100000},
			{Name: "Top", Hold: 100000, Sites: 10, Views: 500000},
		}
	}
	if c.GraceDays == 0 {
		c.GraceDays = 7
	}
	if c.GraceEveryDays == 0 {
		c.GraceEveryDays = 30
	}
	if c.OverFactor == 0 {
		c.OverFactor = 2
	}
	return c
}

func (c Config) rules() (Rules, error) {
	unit := int64(1)
	for range c.Decimals {
		unit *= 10
	}
	for i, t := range c.Tiers {
		if t.Hold <= 0 || t.Sites <= 0 || t.Views <= 0 || t.Name == "" || (i > 0 && t.Hold <= c.Tiers[i-1].Hold) {
			return Rules{}, fmt.Errorf("tier %d: tiers need a name, positive numbers and rising holdings", i)
		}
	}
	if c.Decimals < 0 || c.Decimals > 12 || c.OverFactor < 1 || c.GraceDays < 0 || c.GraceEveryDays < 0 {
		return Rules{}, errors.New("config: decimals, grace_days, grace_every_days or over_factor out of range")
	}
	return Rules{Tiers: c.Tiers, Grace: time.Duration(c.GraceDays) * 24 * time.Hour,
		GraceEvery: time.Duration(c.GraceEveryDays) * 24 * time.Hour, OverFactor: int64(c.OverFactor), Unit: unit}, nil
}

// App is the running server.
type App struct {
	cfg        Config
	rules      Rules
	store      *Store
	rpc        Balancer
	chal       challenges
	limit      *limiter
	originHost string
	secure     bool
	log        func(string, ...any)
	tmpl       *template.Template

	mu      sync.Mutex
	accts   map[string]*acct      // the working copy of an account: record, sites, this month
	sites   map[string]*siteEntry // every site by id
	manual  map[string]time.Time  // the last Check Now of an address
	checkMu sync.Mutex            // one balance check at a time
}

// Balancer reads a wallet's holding; the RPC in service, a table in tests.
type Balancer interface {
	Balance(ctx context.Context, addr string) (int64, error)
}

func newApp(cfg Config, appDB *sql.DB, rpc Balancer) (*App, error) {
	cfg = defaults(cfg)
	rules, err := cfg.rules()
	if err != nil {
		return nil, err
	}
	st, err := openStore(appDB)
	if err != nil {
		return nil, err
	}
	a := &App{cfg: cfg, rules: rules, store: st, rpc: rpc, limit: newLimiter(), log: log.Printf,
		accts: map[string]*acct{}, sites: map[string]*siteEntry{}, manual: map[string]time.Time{}}
	if cfg.Origin != "" {
		u, err := url.Parse(cfg.Origin)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return nil, errors.New("config: origin must be a URL like https://stats.example.com")
		}
		a.originHost, a.secure = u.Host, u.Scheme == "https"
	}
	if a.tmpl, err = template.New("pages").Funcs(stats.Funcs()).Parse(pagesHTML); err == nil {
		_, err = a.tmpl.Parse(badgeHTML)
	}
	if err != nil {
		return nil, err
	}
	all, err := st.allSites()
	if err != nil {
		return nil, err
	}
	for _, s := range all {
		a.sites[s.ID] = &siteEntry{site: s, saved: s.Seen, backfilled: s.Seen != 0}
	}
	return a, nil
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.handleHome)
	mux.HandleFunc("GET /js/s.js", a.handleScript)
	mux.HandleFunc("GET /favicon.ico", serveIcon("image/x-icon", faviconICO))
	mux.HandleFunc("GET /apple-touch-icon.png", serveIcon("image/png", touchIconPNG))
	mux.HandleFunc("GET /apple-touch-icon-precomposed.png", serveIcon("image/png", touchIconPNG))
	mux.HandleFunc("GET /badge.gif", a.handleBadgeGIF)
	mux.HandleFunc("GET /badge/{$}", a.handleBadgePage)
	mux.HandleFunc("GET /badge/{file}", a.handleSiteBadge)
	mux.Handle("GET /badge", http.RedirectHandler("/badge/", http.StatusMovedPermanently))
	mux.HandleFunc("/e", a.handleCollect)
	mux.HandleFunc("GET /v1/challenge", a.handleChallenge)
	mux.HandleFunc("POST /v1/signin", a.handleSignIn)
	mux.HandleFunc("POST /v1/signout", a.handleSignOut)
	mux.HandleFunc("POST /v1/check", a.handleCheck)
	mux.HandleFunc("POST /v1/sites", a.handleAddSite)
	mux.HandleFunc("POST /v1/sites/{id}/remove", a.handleRemoveSite)
	mux.HandleFunc("POST /v1/sites/{id}/pick", a.handlePickSite)
	mux.HandleFunc("GET /sites/{id}", a.handleDesk)
	mux.HandleFunc("GET /v1/sites/{id}/stats", a.handleDesk)
	mux.HandleFunc("GET /sites/{id}/settings", a.handleSettings)
	mux.HandleFunc("GET /v1/sites/{id}/setup", a.handleSetup)
	mux.HandleFunc("POST /v1/sites/{id}/hosts", a.handleAddHost)
	mux.HandleFunc("POST /v1/sites/{id}/hosts/remove", a.handleRemoveHost)
	mux.HandleFunc("POST /v1/sites/{id}/badge", a.handleBadgeStyle)
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, "User-agent: *\nDisallow: /sites/\nDisallow: /v1/\nDisallow: /e\n")
	})
	return mux
}

// run is the background work: the daily balance checks, the month's
// counts and the snippets' reports written down, and the limiter kept
// small.
func (a *App) run(ctx context.Context) {
	flush := time.NewTicker(5 * time.Second)
	check := time.NewTicker(time.Minute)
	defer flush.Stop()
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			a.flushUsage()
			a.flushSeen(false)
		case now := <-check.C:
			a.checkDue(ctx, now)
			a.limit.prune(now)
			a.store.sweepSessions(now)
		}
	}
}

// close writes what is held and closes every site's file.
func (a *App) close() {
	a.flushUsage()
	a.flushSeen(true)
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.sites {
		e.close()
	}
}

func main() {
	path := flag.String("config", "", "config.json (optional: every field has a default)")
	listen := flag.String("listen", "", "address to listen on, over the config's")
	data := flag.String("data", "", "data directory, over the config's")
	flag.Parse()
	var cfg Config
	if *path != "" {
		b, err := os.ReadFile(*path)
		if err != nil {
			log.Fatalf("config: %v", err)
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Fatalf("config: %v", err)
		}
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *data != "" {
		cfg.Data = *data
	}
	cfg = defaults(cfg)
	if err := mkdirSites(cfg.Data); err != nil {
		log.Fatal(err)
	}
	db, err := openDB(filepath.Join(cfg.Data, "app.db"))
	if err != nil {
		log.Fatal(err)
	}
	app, err := newApp(cfg, db, &RPC{URL: cfg.RPCURL, Mint: cfg.Mint})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.run(ctx)
	srv := &http.Server{Addr: cfg.Listen, Handler: app.routes(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	log.Printf("exe-stats on %s, data in %s", cfg.Listen, cfg.Data)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	app.close()
	db.Close()
}

// mkdirSites makes the data directory and the sites' own under it.
func mkdirSites(data string) error { return os.MkdirAll(filepath.Join(data, "sites"), 0o750) }

// openDB opens a SQLite file the way the stats package asks: WAL, a busy
// timeout, and one connection.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openReader opens a second, read-only pool on a site's file for its
// reports: a connection a core, so a report's queries — and the days
// it finds uncounted — run all at once (stats.Options.Reader).
func openReader(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(runtime.NumCPU())
	return db, nil
}
