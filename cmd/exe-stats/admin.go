package main

import (
	"net/http"
	"strconv"
)

// Admins: wallet addresses the config names (Config.Admins) that may
// read every account — its page with its live numbers, its sites' desks
// and their icons — and change none of it. A page read as another
// account is that account's own page with every control that changes
// something left off; every request that changes something still asks
// for the owner (owner, ownSite), so an admin's press could only ever
// reach the admin's own account. An admin's own page lists the accounts.

func (a *App) isAdmin(addr string) bool { return addr != "" && a.admins[addr] }

// reader is the account a page that only reads shows: the signed-in
// one, or for an admin the one ?as= names — as says which. The caller
// checks that an account named so exists, as the store would otherwise
// make it.
func (a *App) reader(r *http.Request) (addr string, as bool) {
	addr = a.signedIn(r)
	if q := r.URL.Query().Get("as"); q != "" && q != addr && a.isAdmin(addr) {
		return q, true
	}
	return addr, false
}

// adminRow is an account as an admin's Accounts window lists it.
type adminRow struct {
	Addr, Short string
	Tier        string
	Sites       string
	Views       string // this month's, counted
}

// adminRows are the accounts that hold a tier or have a site, the
// month's busiest first.
func (a *App) adminRows(month string) []adminRow {
	rows, err := a.store.db.Query(`SELECT a.addr, a.tier,
		(SELECT count(*) FROM sites s WHERE s.addr = a.addr),
		COALESCE((SELECT views FROM usage u WHERE u.addr = a.addr AND u.month = ?), 0) AS views
		FROM accounts a WHERE a.tier >= 0 OR EXISTS (SELECT 1 FROM sites s WHERE s.addr = a.addr)
		ORDER BY views DESC, a.created`, month)
	if err != nil {
		a.log("accounts: %v", err)
		return nil
	}
	defer rows.Close()
	var out []adminRow
	for rows.Next() {
		var addr string
		var tier, sites int
		var views int64
		if rows.Scan(&addr, &tier, &sites, &views) != nil {
			continue
		}
		name := "None"
		if tier >= 0 && tier < len(a.rules.Tiers) {
			name = a.rules.Tiers[tier].Name
		}
		out = append(out, adminRow{Addr: addr, Short: ends(addr, 4), Tier: name, Sites: strconv.Itoa(sites), Views: commas(views)})
	}
	return out
}
