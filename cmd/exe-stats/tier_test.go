package main

import (
	"testing"
	"time"
)

func testRules() Rules {
	r, err := defaults(Config{}).rules()
	if err != nil {
		panic(err)
	}
	return r
}

const tok = 1000000 // raw units in a token, at six decimals

// The holding side of the grace table: Good, Short for seven days, then
// the lower tier or a pause; a balance back ends it at once; and one
// grace in thirty days.
func TestCheckGrace(t *testing.T) {
	ru := testRules()
	day := 24 * time.Hour
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	a := ru.check(Account{Tier: -1}, 60000*tok, t0)
	if a.Tier != 1 || a.ShortSince != 0 || holding(a, 1) != StateGood {
		t.Fatalf("60,000 tokens: %+v", a)
	}
	// the balance falls to the base tier's: the middle tier holds for 7 days
	a = ru.check(a, 12000*tok, t0.Add(day))
	if a.Tier != 1 || a.ShortSince == 0 || holding(a, 1) != StateShort {
		t.Fatalf("day 0 of the grace: %+v", a)
	}
	if got := ru.missing(a); got != 38000*tok {
		t.Fatalf("missing = %d", got)
	}
	if got := ru.graceEnds(a); !got.Equal(t0.Add(8 * day)) {
		t.Fatalf("grace ends %v", got)
	}
	a = ru.check(a, 12000*tok, t0.Add(7*day))
	if a.Tier != 1 || holding(a, 1) != StateShort {
		t.Fatalf("day 6: still the middle tier, %+v", a)
	}
	a = ru.check(a, 12000*tok, t0.Add(8*day))
	if a.Tier != 0 || a.ShortSince != 0 || holding(a, 1) != StateGood {
		t.Fatalf("day 7: the base tier, %+v", a)
	}
	// back over the middle tier: the old tier at the check that sees it
	a = ru.check(a, 50000*tok, t0.Add(9*day))
	if a.Tier != 1 {
		t.Fatalf("topped up: %+v", a)
	}
	// a second drop inside thirty days of the last grace: no grace
	a = ru.check(a, 100*tok, t0.Add(12*day))
	if a.Tier != -1 || a.ShortSince != 0 || holding(a, 1) != StatePaused || holding(a, 0) != StateNone {
		t.Fatalf("second drop in the window: %+v", a)
	}
	// thirty days after that grace began, a drop is graced again
	a = ru.check(a, 10000*tok, t0.Add(20*day))
	a = ru.check(a, 0, t0.Add(31*day+time.Hour))
	if a.Tier != 0 || holding(a, 1) != StateShort {
		t.Fatalf("a new window: %+v", a)
	}
	// and a balance back inside the grace is Good at once
	a = ru.check(a, 10000*tok, t0.Add(33*day))
	if a.Tier != 0 || a.ShortSince != 0 {
		t.Fatalf("back inside the grace: %+v", a)
	}
}

// The views side: a first month over counts up to twice the cap; the
// month after a full one stops at the cap; a month under it, or a
// higher tier, lifts that.
func TestCountMonths(t *testing.T) {
	ru := testRules()
	run := func(tier int, prev Usage, sent int64) Usage {
		cur := Usage{}
		for range sent {
			cur, _ = ru.count(tier, cur, prev)
		}
		return cur
	}
	sept := run(0, Usage{}, 25000)
	if sept.Views != 20000 || !sept.Full || ru.views(0, sept, Usage{}) != StateOver {
		t.Fatalf("a first month over: %+v %s", sept, ru.views(0, sept, Usage{}))
	}
	if u := run(0, Usage{}, 10000); u.Full || ru.views(0, u, Usage{}) != StateGood {
		t.Fatalf("a month at the cap is not over: %+v", u)
	}
	oct := run(0, sept, 15000)
	if oct.Views != 10000 || !oct.Full || ru.views(0, oct, sept) != StateCapped {
		t.Fatalf("the second month: %+v %s", oct, ru.views(0, oct, sept))
	}
	// still full: November is capped too
	if nov := run(0, oct, 15000); nov.Views != 10000 {
		t.Fatalf("the third month: %+v", nov)
	}
	// a month under the cap, and the one after is forgiven again
	quiet := run(0, sept, 9000)
	if quiet.Full {
		t.Fatalf("a month under the cap: %+v", quiet)
	}
	if dec := run(0, quiet, 15000); dec.Views != 15000 {
		t.Fatalf("after a quiet month: %+v", dec)
	}
	// a higher tier starts clean
	if up := run(1, sept, 150000); up.Views != 150000 || ru.views(1, up, sept) != StateOver {
		t.Fatalf("the middle tier after a full base month: %+v", up)
	}
	// no tier counts nothing
	if u, ok := ru.count(-1, Usage{}, Usage{}); ok || u.Views != 0 {
		t.Fatal("a view counted with no tier")
	}
}

func TestTierOf(t *testing.T) {
	ru := testRules()
	for raw, want := range map[int64]int{0: -1, 9999*tok + 999999: -1, 10000 * tok: 0, 49999 * tok: 0, 50000 * tok: 1, 100000 * tok: 2, 5000000 * tok: 2} {
		if got := ru.tierOf(raw); got != want {
			t.Errorf("tierOf(%d) = %d, want %d", raw, got, want)
		}
	}
}

func TestMonths(t *testing.T) {
	jan := time.Date(2027, 1, 1, 0, 30, 0, 0, time.FixedZone("x", 3600)) // still December in UTC
	if monthOf(jan) != "2026-12" || prevMonthOf(jan) != "2026-11" {
		t.Fatalf("%s %s", monthOf(jan), prevMonthOf(jan))
	}
	if m := prevMonthOf(time.Date(2027, 3, 31, 12, 0, 0, 0, time.UTC)); m != "2027-02" {
		t.Fatalf("the month before March 31 = %s", m)
	}
}
