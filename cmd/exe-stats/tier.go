package main

import "time"

// Tier is one step of the holding ladder: hold at least Hold tokens and
// count up to Views page views a month over at most Sites sites.
type Tier struct {
	Name  string `json:"name"`
	Hold  int64  `json:"hold"`  // whole tokens
	Sites int    `json:"sites"` // how many sites count
	Views int64  `json:"views"` // combined page views a calendar month (UTC)
}

// Rules are the tiers and the three numbers of the grace design.
type Rules struct {
	Tiers      []Tier        // ascending by Hold
	Grace      time.Duration // how long a short balance keeps its tier (7 days)
	GraceEvery time.Duration // one grace in this long (30 days)
	OverFactor int64         // a first month over the cap counts up to this many caps (2)
	Unit       int64         // raw units in one token: 10^decimals
}

// tierOf is the tier a raw balance covers, -1 when it covers none.
func (ru Rules) tierOf(raw int64) int {
	t := -1
	for i, tr := range ru.Tiers {
		if raw >= tr.Hold*ru.Unit {
			t = i
		}
	}
	return t
}

// Account is what the server keeps of a wallet between checks.
type Account struct {
	Addr       string
	Balance    int64 // raw units, at the last check that the RPC answered
	Checked    int64 // ms; 0: never
	NextCheck  int64 // ms
	Tier       int   // the tier in force: -1 none
	ShortSince int64 // ms; the check that first found the balance under the tier, 0: not short
	GraceBegan int64 // ms; when the last grace began, for one grace in GraceEvery
	Created    int64
}

// check applies a balance the RPC answered. A balance that covers the
// tier in force (or a higher one) takes that tier at once. One that
// falls short keeps the tier for Grace, once in GraceEvery; after the
// grace, or on a second drop inside GraceEvery, the account moves to the
// tier the balance does cover. A check the RPC could not answer never
// reaches here: it changes nothing.
func (ru Rules) check(a Account, raw int64, now time.Time) Account {
	ms := now.UnixMilli()
	a.Balance, a.Checked = raw, ms
	covers := ru.tierOf(raw)
	switch {
	case covers >= a.Tier:
		a.Tier, a.ShortSince = covers, 0
	case a.ShortSince == 0:
		if a.GraceBegan != 0 && ms-a.GraceBegan < ru.GraceEvery.Milliseconds() {
			a.Tier = covers // the second drop in the window: no grace
		} else {
			a.ShortSince, a.GraceBegan = ms, ms
		}
	case ms-a.ShortSince >= ru.Grace.Milliseconds():
		a.Tier, a.ShortSince = covers, 0
	}
	return a
}

// graceEnds is when a short account's counting stops or steps down.
func (ru Rules) graceEnds(a Account) time.Time {
	return time.UnixMilli(a.ShortSince).Add(ru.Grace)
}

// missing is how many raw units a short account needs back.
func (ru Rules) missing(a Account) int64 {
	if a.Tier < 0 || a.Tier >= len(ru.Tiers) {
		return 0
	}
	return max(0, ru.Tiers[a.Tier].Hold*ru.Unit-a.Balance)
}

// Usage is a month's counted page views of one account, over all its
// sites. Full says the month reached its end: the views passed the cap,
// or a capped month had a view dropped at it. FullTier is the tier it
// happened under, so a higher tier next month starts clean.
type Usage struct {
	Month    string // 2006-01, UTC
	Views    int64
	Full     bool
	FullTier int
}

// limit is how many views this month counts for an account on tier t:
// twice the cap, so a first month over is forgiven; the cap itself when
// last month was full under this tier or a higher one.
func (ru Rules) limit(t int, prev Usage) (limit int64, capped bool) {
	top := ru.Tiers[t].Views
	if prev.Full && prev.FullTier >= t {
		return top, true
	}
	return top * ru.OverFactor, false
}

// count decides one page view: whether it is counted, and the month's
// usage after it.
func (ru Rules) count(t int, cur, prev Usage) (Usage, bool) {
	if t < 0 || t >= len(ru.Tiers) {
		return cur, false
	}
	limit, capped := ru.limit(t, prev)
	if cur.Views >= limit {
		if !cur.Full {
			cur.Full, cur.FullTier = true, t
		}
		return cur, false
	}
	cur.Views++
	if !capped && cur.Views > ru.Tiers[t].Views && !cur.Full {
		cur.Full, cur.FullTier = true, t
	}
	return cur, true
}

// The states of the grace table. An account has one on the holding side
// and one on the views side.
const (
	StateGood   = "good"
	StateNone   = "none"   // no tier and no sites: nothing to pause
	StateShort  = "short"  // under the tier, inside the grace
	StatePaused = "paused" // under the lowest tier: nothing counts, all of it readable
	StateOver   = "over"   // past the cap in a forgiven month
	StateCapped = "capped" // a second month in a row: counting stops at the cap
)

// holding is the account's state on the holding side.
func holding(a Account, sites int) string {
	switch {
	case a.ShortSince != 0:
		return StateShort
	case a.Tier < 0 && sites > 0:
		return StatePaused
	case a.Tier < 0:
		return StateNone
	}
	return StateGood
}

// views is the account's state on the views side.
func (ru Rules) views(t int, cur, prev Usage) string {
	if t < 0 || t >= len(ru.Tiers) {
		return StateGood
	}
	_, capped := ru.limit(t, prev)
	top := ru.Tiers[t].Views
	switch {
	case capped && cur.Views >= top:
		return StateCapped
	case !capped && cur.Views > top:
		return StateOver
	}
	return StateGood
}

func monthOf(t time.Time) string { return t.UTC().Format("2006-01") }

func prevMonthOf(t time.Time) string {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format("2006-01")
}
