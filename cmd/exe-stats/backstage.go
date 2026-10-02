package main

import (
	"net/http"
	"strconv"
)

// Backstage, /backstage/: for the admins alone, every wallet that has
// signed in — its address, what it holds of the mint as last read, and
// how many sites it has — the largest holding first. Like the rest of
// what an admin reads, it changes nothing; to anyone else it is not
// there.

type backstageRow struct {
	Addr, Short string
	Holding     string // whole tokens, or "Not read yet"
	Sites       string
}

type backstageData struct {
	Host, Token string
	Rows        []backstageRow
	Wallets     string // the status line: 6 wallets, 9 sites
	Held        string // and what they hold between them
}

// GET /backstage/
func (a *App) handleBackstage(w http.ResponseWriter, r *http.Request) {
	if !a.isAdmin(a.signedIn(r)) {
		http.NotFound(w, r)
		return
	}
	rows, err := a.store.db.Query(`SELECT a.addr, a.balance, a.checked,
		(SELECT count(*) FROM sites s WHERE s.addr = a.addr)
		FROM accounts a ORDER BY a.checked = 0, a.balance DESC, a.created`)
	if err != nil {
		a.log("backstage: %v", err)
		http.Error(w, "the accounts could not be read", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	d := backstageData{Host: a.host(r), Token: a.cfg.Token}
	var held int64
	sites := 0
	for rows.Next() {
		var addr string
		var balance, checked int64
		var n int
		if err := rows.Scan(&addr, &balance, &checked, &n); err != nil {
			a.log("backstage: %v", err)
			continue
		}
		row := backstageRow{Addr: addr, Short: ends(addr, 6), Holding: "Not read yet", Sites: strconv.Itoa(n)}
		if checked > 0 {
			row.Holding = commas(balance / a.rules.Unit)
			held += balance
		}
		sites += n
		d.Rows = append(d.Rows, row)
	}
	if err := rows.Err(); err != nil {
		a.log("backstage: %v", err)
	}
	d.Wallets = plural(len(d.Rows), "wallet", "wallets") + ", " + plural(sites, "site", "sites")
	d.Held = commas(held/a.rules.Unit) + " " + a.cfg.Token + " held"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := a.tmpl.ExecuteTemplate(w, "backstage", d); err != nil {
		a.log("backstage: %v", err)
	}
}
