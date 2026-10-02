package stats

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The hits store: one row per page view, in a SQLite database the host
// opens and owns (this package registers no driver and keeps no file of
// its own, so the hub writes its hits into hub.db beside its posts and
// another program writes them wherever it likes). Nothing in a row names
// a person: the visitor id is the day's salted hash, the address and the
// user agent were read and dropped, and what remains is a country, a
// device, a browser, a page and a time.

// DB is the hits tables in the host's database.
type DB struct {
	db *sql.DB
	// gen counts the times kept days were dropped for hits written
	// under them (Add, Sweep), and busy is how many such writes are on
	// their way: a day counted across either is not kept (keepDay)
	gen, busy atomic.Int64
	// fed is told of each batch Add commits, with its ids, and swept of
	// each Sweep's cut: the hits in memory (live.go) follow the store
	fed    func(hits []Hit, ids []int64)
	swept  func(before int64)
	feedMu sync.Mutex // held from a write's commit through its feed, and by loadLive's snapshot
}

// dropping marks a write that drops kept days, from before it begins
// until after it has committed or not.
func (d *DB) dropping() func() {
	d.busy.Add(1)
	return func() {
		d.gen.Add(1)
		d.busy.Add(-1)
	}
}

// Open prepares the hits tables in db and returns them. It is safe to
// call on a database that already has them.
func Open(db *sql.DB) (*DB, error) {
	if db == nil {
		return nil, errors.New("stats: no database")
	}
	d := &DB{db: db}
	if err := d.init(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *DB) init() error {
	_, err := d.db.Exec(`
CREATE TABLE IF NOT EXISTS hits (
  id       INTEGER PRIMARY KEY,
  ts       INTEGER NOT NULL,            -- ms
  vid      TEXT NOT NULL,               -- the visitor: today's salted hash
  sid      TEXT NOT NULL,               -- the session
  entry    INTEGER NOT NULL DEFAULT 0,  -- 1: the session's first page
  path     TEXT NOT NULL,
  kind     TEXT NOT NULL,               -- home | thread | profile | search | skill
  ref      TEXT NOT NULL DEFAULT '',    -- the source's name; '' direct
  chan     TEXT NOT NULL DEFAULT '',    -- direct | search | social | ai | referral | campaign
  country  TEXT NOT NULL DEFAULT '',    -- ISO 3166-1 alpha-2
  region   TEXT NOT NULL DEFAULT '',
  city     TEXT NOT NULL DEFAULT '',
  device   TEXT NOT NULL DEFAULT '',    -- desktop | mobile | tablet | agent
  browser  TEXT NOT NULL DEFAULT '',
  os       TEXT NOT NULL DEFAULT '',
  lang     TEXT NOT NULL DEFAULT '',
  utm_source   TEXT NOT NULL DEFAULT '',
  utm_medium   TEXT NOT NULL DEFAULT '',
  utm_campaign TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS hits_ts ON hits(ts);
CREATE INDEX IF NOT EXISTS hits_sid ON hits(sid, ts);

-- the day's salt for visitor ids: kept for the day so a restart keeps
-- the day whole, deleted with the day
CREATE TABLE IF NOT EXISTS hits_salt (
  day  TEXT PRIMARY KEY,
  salt BLOB NOT NULL
);

-- the finished days a report has counted (days.go). A view is the
-- filters a day was counted under, '' for none; used is when a report
-- last read it, so a filter nobody looks at any more gives its days back
CREATE TABLE IF NOT EXISTS hits_views (
  id     INTEGER PRIMARY KEY AUTOINCREMENT, -- never one a dropped view had
  filter TEXT NOT NULL UNIQUE,
  used   INTEGER NOT NULL          -- ms
);
-- the parts of the days a view keeps — summary, hours or a list's
-- dimension — one row a part a day: its cells packed (days.go pack), a
-- summary's five numbers, an hour's page views and visitors, a list's
-- every key with its count; a day with nothing in it is known to have
-- nothing
CREATE TABLE IF NOT EXISTS hits_days (
  view INTEGER NOT NULL,
  part TEXT NOT NULL,
  day  INTEGER NOT NULL,           -- ms: the day's midnight
  data BLOB NOT NULL,
  PRIMARY KEY (view, part, day)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS hits_days_day ON hits_days(day);
-- what the kept days were counted by: the counting's version and the
-- zone their midnights are in, either of which, changed, drops them
-- all; and since when the zone has been this one
CREATE TABLE IF NOT EXISTS hits_meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);`)
	if err != nil {
		return err
	}
	// a crawler's hit: kept apart from people's (browser holds the
	// crawler's name, device is "bot"); older tables gain the column
	if _, err := d.db.Exec(`ALTER TABLE hits ADD COLUMN bot INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	return nil
}

// Hit is one page view.
type Hit struct {
	TS                    int64
	VID, SID              string
	Entry                 bool
	Path, Kind            string
	Ref, Channel          string
	Country, Region, City string
	Device, Browser, OS   string
	Lang                  string
	UTMSource, UTMMedium  string
	UTMCampaign           string
	Bot                   bool // a crawler: Browser is its name, Device "bot"
}

// StatsAdd writes a batch of hits in one transaction.
func (d *DB) Add(hits []Hit) error {
	oldest := int64(math.MaxInt64)
	for _, h := range hits {
		oldest = min(oldest, h.TS)
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO hits (ts, vid, sid, entry, path, kind, ref, chan, country, region, city,
		device, browser, os, lang, utm_source, utm_medium, utm_campaign, bot) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	ids := make([]int64, len(hits))
	for i, h := range hits {
		entry, bot := 0, 0
		if h.Entry {
			entry = 1
		}
		if h.Bot {
			bot = 1
		}
		res, err := stmt.Exec(h.TS, h.VID, h.SID, entry, h.Path, h.Kind, h.Ref, h.Channel, h.Country, h.Region, h.City,
			h.Device, h.Browser, h.OS, h.Lang, h.UTMSource, h.UTMMedium, h.UTMCampaign, bot)
		if err != nil {
			return err
		}
		if ids[i], err = res.LastInsertId(); err != nil {
			return err
		}
	}
	// a hit for a day that may be kept already — a seeder's, or one that
	// waited on its way here: every kept day that could hold it, and
	// every day after, is counted again. It is judged with the write
	// lock held, by half of sealDelay: a hit judged on time commits long
	// before its day can be counted.
	if oldest < time.Now().Add(-sealDelay/2).UnixMilli() {
		defer d.dropping()()
		if err := dropDays(tx, ">", oldest-longestDay); err != nil {
			return err
		}
	}
	d.feedMu.Lock()
	defer d.feedMu.Unlock()
	if err := tx.Commit(); err != nil {
		return err
	}
	if addCommitted != nil {
		addCommitted()
	}
	if d.fed != nil {
		d.fed(hits, ids)
	}
	return nil
}

// addCommitted runs between Add's commit and its feed, for a test.
var addCommitted func()

// StatsSalt is the day's salt, minted on its first call for the day;
// every other day's salt is dropped, so yesterday's ids can never be
// recomputed.
func (d *DB) Salt(day string) ([]byte, error) {
	var salt []byte
	err := d.db.QueryRow(`SELECT salt FROM hits_salt WHERE day=?`, day).Scan(&salt)
	if err == nil {
		return salt, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	salt = make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM hits_salt WHERE day<>?`, day); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO hits_salt (day, salt) VALUES (?,?)`, day, salt); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(`SELECT salt FROM hits_salt WHERE day=?`, day).Scan(&salt); err != nil {
		return nil, err
	}
	return salt, tx.Commit()
}

// OpenSession is a visitor's session with a hit since the gap: what the
// collector resumes after a restart.
type OpenSession struct {
	VID, SID                          string
	Last                              int64
	Ref, Channel                      string
	UTMSource, UTMMedium, UTMCampaign string
}

func (d *DB) OpenSessions(since int64) ([]OpenSession, error) {
	rows, err := d.db.Query(`SELECT h.vid, h.sid, h.ts, h.ref, h.chan, h.utm_source, h.utm_medium, h.utm_campaign
		FROM hits h JOIN (SELECT vid, MAX(id) AS id FROM hits WHERE ts >= ? GROUP BY vid) last ON last.id = h.id`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenSession
	for rows.Next() {
		var o OpenSession
		if err := rows.Scan(&o.VID, &o.SID, &o.Last, &o.Ref, &o.Channel, &o.UTMSource, &o.UTMMedium, &o.UTMCampaign); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// StatsOnline is how many people had a hit since the given time.
func (d *DB) Online(since int64) (int, error) {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(DISTINCT vid) FROM hits WHERE ts >= ? AND bot = 0`, since).Scan(&n)
	return n, err
}

// Filter selects the hits a report is over: a time span, and any
// of the dimensions held to one value (a click on a row of the page).
// Bot chooses whose hits: "" people's (the default: crawlers are out of
// every human number), "all" every crawler's, a name that crawler's.
type Filter struct {
	From, To                        int64 // ms, [From, To)
	Country, Region, City, Lang     string
	Path, Source, Channel, Campaign string
	Device, Browser, OS             string
	Bot                             string
}

func (f Filter) where() (string, []any) { return f.clause(false) }

// clause is where's; with both, a filter on people's hits (Bot "")
// takes the crawlers' too, for a pass that sorts them itself (countDay).
func (f Filter) clause(both bool) (string, []any) {
	w := []string{"ts >= ?", "ts < ?"}
	args := []any{f.From, f.To}
	switch {
	case f.Bot == "" && both:
	case f.Bot == "":
		w = append(w, "bot = 0")
	case f.Bot == "all":
		w = append(w, "bot = 1")
	default:
		w = append(w, "bot = 1", "browser = ?")
		args = append(args, f.Bot)
	}
	for _, c := range []struct{ col, v string }{
		{"country", f.Country}, {"region", f.Region}, {"city", f.City}, {"lang", f.Lang},
		{"path", f.Path}, {"ref", f.Source}, {"chan", f.Channel}, {"utm_campaign", f.Campaign},
		{"device", f.Device}, {"browser", f.Browser}, {"os", f.OS},
	} {
		if c.v != "" {
			w = append(w, c.col+" = ?")
			args = append(args, c.v)
		}
	}
	return strings.Join(w, " AND "), args
}

// Summary is the headline numbers over a span: visitors (distinct
// per day, as the ids rotate), page views, sessions, the sessions that
// saw one page (bounces), and the mean session length in seconds
// (first page to last; a one-page session is 0).
type Summary struct {
	Visitors  int     `json:"visitors"`
	Pageviews int     `json:"pageviews"`
	Sessions  int     `json:"sessions"`
	Bounces   int     `json:"bounces"`
	Duration  float64 `json:"duration"`
}

func (d *DB) Summary(f Filter) (Summary, error) {
	t, err := d.tally(f)
	return t.summary(), err
}

// tally is a summary in numbers that add up from one stretch to the
// next: the sessions' time is summed, in ms, rather than averaged.
type tally struct {
	Summary
	durMS int64
}

func (t *tally) add(u tally) {
	t.Visitors += u.Visitors
	t.Pageviews += u.Pageviews
	t.Sessions += u.Sessions
	t.Bounces += u.Bounces
	t.durMS += u.durMS
}

func (t tally) summary() Summary {
	s := t.Summary
	s.Duration = 0
	if s.Sessions > 0 {
		s.Duration = float64(t.durMS) / 1000 / float64(s.Sessions)
	}
	return s
}

func (d *DB) tally(f Filter) (tally, error) {
	var t tally
	w, args := f.where()
	if err := d.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT vid), COUNT(DISTINCT sid) FROM hits WHERE `+w, args...).
		Scan(&t.Pageviews, &t.Visitors, &t.Sessions); err != nil {
		return t, err
	}
	var bounces, dur sql.NullInt64
	if err := d.db.QueryRow(`SELECT SUM(n = 1), SUM(dur) FROM
		(SELECT COUNT(*) AS n, MAX(ts) - MIN(ts) AS dur FROM hits WHERE `+w+` GROUP BY sid)`, args...).
		Scan(&bounces, &dur); err != nil {
		return t, err
	}
	t.Bounces, t.durMS = int(bounces.Int64), dur.Int64
	return t, nil
}

// HourRow is one hour's page views and visitors (distinct within the
// hour): the finest bucket a chart draws.
type HourRow struct {
	Hour      int64 // unix hours
	Pageviews int
	Visitors  int
}

func (d *DB) Hours(f Filter) ([]HourRow, error) {
	w, args := f.where()
	rows, err := d.db.Query(`SELECT ts / 3600000, COUNT(*), COUNT(DISTINCT vid) FROM hits WHERE `+w+` GROUP BY 1 ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HourRow
	for rows.Next() {
		var r HourRow
		if err := rows.Scan(&r.Hour, &r.Pageviews, &r.Visitors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StatsVisitorFirst is each visitor's first hit time in the span: a
// visitor id belongs to one day, so bucketing these by day or month
// counts distinct visitors per bucket.
func (d *DB) VisitorFirst(f Filter) ([]int64, error) {
	w, args := f.where()
	rows, err := d.db.Query(`SELECT MIN(ts) FROM hits WHERE `+w+` GROUP BY vid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Row is one line of a ranked list: a key and its count.
type Row struct {
	Key   string `json:"key"`
	Label string `json:"label,omitempty"` // a thread's words, a profile's name (set by the page)
	N     int    `json:"n"`
}

// topMax caps a ranked list: a hub's paths over a year can run long.
const topMax = 500

// StatsTop ranks one dimension over the span. Sources, channels and
// campaigns count sessions (by their first page); entry and exit pages
// count sessions too; crawlers and the pages they crawl count hits;
// every other dimension counts visitors.
func (d *DB) Top(f Filter, dim string) ([]Row, error) { return d.rank(f, dim, topMax) }

// rank is Top cut at limit rows; -1 is every row.
func (d *DB) rank(f Filter, dim string, limit int) ([]Row, error) {
	if (dim == "crawler" || dim == "botpath") && f.Bot == "" {
		f.Bot = "all" // the crawlers' lists are always over crawlers
	}
	w, args := f.where()
	var q string
	switch dim {
	case "crawler":
		q = `SELECT browser, COUNT(*) FROM hits WHERE ` + w + ` GROUP BY browser`
	case "botpath":
		q = `SELECT path, COUNT(*) FROM hits WHERE ` + w + ` GROUP BY path`
	case "source":
		q = `SELECT ref, COUNT(*) FROM hits WHERE entry = 1 AND ` + w + ` GROUP BY ref`
	case "channel":
		q = `SELECT chan, COUNT(*) FROM hits WHERE entry = 1 AND ` + w + ` GROUP BY chan`
	case "campaign":
		q = `SELECT utm_campaign, COUNT(*) FROM hits WHERE entry = 1 AND utm_campaign <> '' AND ` + w + ` GROUP BY utm_campaign`
	case "entry":
		q = `SELECT path, COUNT(*) FROM hits WHERE entry = 1 AND ` + w + ` GROUP BY path`
	case "exit":
		q = `SELECT path, COUNT(*) FROM hits WHERE id IN (SELECT MAX(id) FROM hits WHERE ` + w + ` GROUP BY sid) GROUP BY path`
	case "path", "country", "region", "city", "device", "browser", "os", "lang":
		q = `SELECT ` + dim + `, COUNT(DISTINCT vid) FROM hits WHERE ` + dim + ` <> '' AND ` + w + ` GROUP BY ` + dim
	default:
		return nil, fmt.Errorf("stats: no dimension %q", dim)
	}
	rows, err := d.db.Query(q+` ORDER BY 2 DESC, 1 LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.Key, &r.N); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LiveHit is one of the latest page views, for the Live list.
type LiveHit struct {
	TS              int64
	VID, Path, Kind string
	Country, Device string
}

// StatsRecent is the latest n hits by people, newest first.
func (d *DB) Recent(n int) ([]LiveHit, error) {
	rows, err := d.db.Query(`SELECT ts, vid, path, kind, country, device FROM hits WHERE bot = 0 ORDER BY ts DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveHit
	for rows.Next() {
		var h LiveHit
		if err := rows.Scan(&h.TS, &h.VID, &h.Path, &h.Kind, &h.Country, &h.Device); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// StatsSweep drops the hits before a time (the retention) and reports
// how many went. The kept days that held any of them go too, so a
// report over them reads what is left.
func (d *DB) Sweep(before time.Time) (int64, error) {
	defer d.dropping()()
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM hits WHERE ts < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	if err := dropDays(tx, "<", before.UnixMilli()); err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	d.feedMu.Lock()
	defer d.feedMu.Unlock()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if d.swept != nil {
		d.swept(before.UnixMilli())
	}
	return n, nil
}
