package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// The server's own records, in app.db beside the sites' files: who has
// signed in, which sites are theirs, and how many views each account
// has counted this month. A site's hits are not here — each site has a
// SQLite file of its own (sites.go), which is the database the stats
// package is handed.

type Store struct{ db *sql.DB }

func openStore(db *sql.DB) (*Store, error) {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS accounts (
  addr        TEXT PRIMARY KEY,            -- the wallet's address: the account
  balance     INTEGER NOT NULL DEFAULT 0,  -- raw units at the last answered check
  checked     INTEGER NOT NULL DEFAULT 0,  -- ms
  next_check  INTEGER NOT NULL DEFAULT 0,  -- ms
  tier        INTEGER NOT NULL DEFAULT -1, -- the tier in force
  short_since INTEGER NOT NULL DEFAULT 0,  -- ms; 0: not short
  grace_began INTEGER NOT NULL DEFAULT 0,  -- ms
  created     INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token   TEXT PRIMARY KEY,                -- sha256 of the cookie, never the cookie
  addr    TEXT NOT NULL,
  expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sites (
  id      TEXT PRIMARY KEY,
  addr    TEXT NOT NULL,
  domain  TEXT NOT NULL,
  tz      TEXT NOT NULL DEFAULT 'UTC',
  picked  INTEGER NOT NULL DEFAULT 0,      -- ms; the owner chose it to count when the tier holds fewer
  created INTEGER NOT NULL,
  seen    INTEGER NOT NULL DEFAULT 0,      -- ms; the last view from the site's own pages: the snippet is in place
  hosts   TEXT NOT NULL DEFAULT '',        -- more hosts whose pages count, space-separated: blog.example.com, *.example.com
  UNIQUE (addr, domain)
);
CREATE INDEX IF NOT EXISTS sites_addr ON sites(addr);
CREATE TABLE IF NOT EXISTS usage (
  addr      TEXT NOT NULL,
  month     TEXT NOT NULL,                 -- 2006-01, UTC
  views     INTEGER NOT NULL DEFAULT 0,
  full      INTEGER NOT NULL DEFAULT 0,
  full_tier INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (addr, month)
);`)
	if err != nil {
		return nil, err
	}
	// a file made before the setup check, or before hosts, gains its column
	for _, col := range []string{`seen INTEGER NOT NULL DEFAULT 0`, `hosts TEXT NOT NULL DEFAULT ''`} {
		if _, err := db.Exec(`ALTER TABLE sites ADD COLUMN ` + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, err
		}
	}
	return &Store{db}, nil
}

func newToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

const accountCols = `addr, balance, checked, next_check, tier, short_since, grace_began, created`

func scanAccount(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	err := row.Scan(&a.Addr, &a.Balance, &a.Checked, &a.NextCheck, &a.Tier, &a.ShortSince, &a.GraceBegan, &a.Created)
	return a, err
}

// account is the wallet's record, made on its first sign-in.
func (s *Store) account(addr string, now time.Time) (Account, error) {
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO accounts (addr, created) VALUES (?, ?)`, addr, now.UnixMilli()); err != nil {
		return Account{}, err
	}
	return scanAccount(s.db.QueryRow(`SELECT `+accountCols+` FROM accounts WHERE addr=?`, addr))
}

func (s *Store) saveAccount(a Account) error {
	_, err := s.db.Exec(`UPDATE accounts SET balance=?, checked=?, next_check=?, tier=?, short_since=?, grace_began=? WHERE addr=?`,
		a.Balance, a.Checked, a.NextCheck, a.Tier, a.ShortSince, a.GraceBegan, a.Addr)
	return err
}

// due are the accounts whose daily check has come: those that hold a
// tier or have a site. A wallet that only ever looked is not asked about.
func (s *Store) due(now time.Time, n int) ([]Account, error) {
	rows, err := s.db.Query(`SELECT `+accountCols+` FROM accounts a WHERE next_check <= ?
		AND (tier >= 0 OR EXISTS (SELECT 1 FROM sites WHERE sites.addr = a.addr)) ORDER BY next_check LIMIT ?`, now.UnixMilli(), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- sessions ----

func (s *Store) newSession(addr string, now time.Time, life time.Duration) (string, error) {
	t := newToken(32)
	_, err := s.db.Exec(`INSERT INTO sessions (token, addr, expires) VALUES (?, ?, ?)`, hashToken(t), addr, now.Add(life).UnixMilli())
	return t, err
}

func (s *Store) sessionAddr(token string, now time.Time) string {
	var addr string
	s.db.QueryRow(`SELECT addr FROM sessions WHERE token=? AND expires > ?`, hashToken(token), now.UnixMilli()).Scan(&addr)
	return addr
}

func (s *Store) endSession(token string) {
	s.db.Exec(`DELETE FROM sessions WHERE token=?`, hashToken(token))
}

func (s *Store) sweepSessions(now time.Time) {
	s.db.Exec(`DELETE FROM sessions WHERE expires <= ?`, now.UnixMilli())
}

// ---- sites ----

type Site struct {
	ID, Addr, Domain, TZ  string
	Picked, Created, Seen int64
	Hosts                 []string // more hosts that count besides Domain (hosts.go)
}

const siteCols = `id, addr, domain, tz, picked, created, seen, hosts`

var errSiteExists = errors.New("that site is already here")

func (s *Store) addSite(addr, domain, tz string, now time.Time) (Site, error) {
	var n int
	s.db.QueryRow(`SELECT count(*) FROM sites WHERE addr=? AND domain=?`, addr, domain).Scan(&n)
	if n > 0 {
		return Site{}, errSiteExists
	}
	st := Site{ID: newToken(6), Addr: addr, Domain: domain, TZ: tz, Created: now.UnixMilli()}
	_, err := s.db.Exec(`INSERT INTO sites (id, addr, domain, tz, created) VALUES (?, ?, ?, ?, ?)`, st.ID, st.Addr, st.Domain, st.TZ, st.Created)
	return st, err
}

func scanSites(rows *sql.Rows, err error) ([]Site, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Site
	for rows.Next() {
		var st Site
		var hosts string
		if err := rows.Scan(&st.ID, &st.Addr, &st.Domain, &st.TZ, &st.Picked, &st.Created, &st.Seen, &hosts); err != nil {
			return nil, err
		}
		st.Hosts = strings.Fields(hosts)
		out = append(out, st)
	}
	return out, rows.Err()
}

// sitesOf are an account's sites in the order they count: the ones the
// owner picked first, latest pick first, then the oldest. A tier that
// holds fewer than there are counts the first of them.
func (s *Store) sitesOf(addr string) ([]Site, error) {
	return scanSites(s.db.Query(`SELECT `+siteCols+` FROM sites WHERE addr=?
		ORDER BY picked DESC, created, rowid`, addr))
}

func (s *Store) allSites() ([]Site, error) {
	return scanSites(s.db.Query(`SELECT ` + siteCols + ` FROM sites ORDER BY addr, picked DESC, created, rowid`))
}

func (s *Store) pickSite(addr, id string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE sites SET picked=? WHERE id=? AND addr=?`, now.UnixMilli(), id, addr)
	return err
}

// saveSeen writes down when a site's snippet last reported a view.
func (s *Store) saveSeen(id string, ms int64) error {
	_, err := s.db.Exec(`UPDATE sites SET seen=? WHERE id=?`, ms, id)
	return err
}

// setHosts writes down the hosts that count for a site besides its domain.
func (s *Store) setHosts(id string, hosts []string) error {
	_, err := s.db.Exec(`UPDATE sites SET hosts=? WHERE id=?`, strings.Join(hosts, " "), id)
	return err
}

func (s *Store) removeSite(addr, id string) (bool, error) {
	r, err := s.db.Exec(`DELETE FROM sites WHERE id=? AND addr=?`, id, addr)
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n > 0, nil
}

// ---- usage ----

func (s *Store) usage(addr, month string) Usage {
	u := Usage{Month: month}
	s.db.QueryRow(`SELECT views, full, full_tier FROM usage WHERE addr=? AND month=?`, addr, month).Scan(&u.Views, &u.Full, &u.FullTier)
	return u
}

func (s *Store) saveUsage(addr string, u Usage) error {
	_, err := s.db.Exec(`INSERT INTO usage (addr, month, views, full, full_tier) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (addr, month) DO UPDATE SET views=excluded.views, full=excluded.full, full_tier=excluded.full_tier`,
		addr, u.Month, u.Views, u.Full, u.FullTier)
	return err
}
