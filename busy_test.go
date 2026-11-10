package stats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// waitFor polls until ok, a few seconds at most.
func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("waited 5 s for %s", what)
		}
	}
}

// TestCountingTurn: days are counted one view at a time. While one
// counts and countQueue wait, a report with days to count is ErrBusy —
// 503 from the page and the JSON, with nothing written for it — while
// the desk whose days are kept is served, and the keeper waits rather
// than fail. Once the line has moved, the same report is the hits'.
func TestCountingTurn(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 10)
	want := rawly(t, s, "7d", Filter{Country: "JP"}, nil, now)
	reportJSON(t, s, "7d", Filter{}, nil, now) // the desk's own days, kept

	ctx := context.Background()
	leave, err := s.enter(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var line sync.WaitGroup
	for range countQueue {
		line.Go(func() {
			l, err := s.enter(ctx, false)
			if err != nil {
				t.Errorf("a report in the line: %v", err)
				return
			}
			l()
		})
	}
	waitFor(t, "the line to fill", func() bool { return int(s.queued.Load()) == countQueue })

	if _, err := s.enter(ctx, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("one past the line: %v, want ErrBusy", err)
	}
	jp := Filter{Country: "JP"}
	if _, err := s.build(spanOf("7d", now, time.UTC), jp, nil, now); !errors.Is(err, ErrBusy) {
		t.Fatalf("a view never counted, with the line full: %v, want ErrBusy", err)
	}
	if known, err := s.db.viewKnown(ctx, viewOf(jp)); err != nil || known {
		t.Fatalf("a view was made for a report that was turned away (%v, %v)", known, err)
	}
	for _, h := range []http.Handler{s.PageHandler(), s.JSONHandler()} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/stats?range=7d&country=US", nil))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != RetryAfter {
			t.Fatalf("a busy desk answered %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
		}
	}
	// the desk without filters has its days: it counts nothing, and waits on no one
	w := httptest.NewRecorder()
	s.PageHandler().ServeHTTP(w, httptest.NewRequest("GET", "/stats?range=7d", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("the desk whose days are kept answered %d while others count", w.Code)
	}
	// the keeper waits its turn however long the line
	kept := make(chan error, 1)
	go func() {
		l, err := s.enter(ctx, true)
		if err == nil {
			l()
		}
		kept <- err
	}()
	select {
	case err := <-kept:
		t.Fatalf("the keeper did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	leave()
	line.Wait()
	if err := <-kept; err != nil {
		t.Fatalf("the keeper's turn: %v", err)
	}
	if n := s.queued.Load(); n != 0 || len(s.counting) != 0 {
		t.Fatalf("after the line: %d waiting, %d counting", n, len(s.counting))
	}
	if got := reportJSON(t, s, "7d", jp, nil, now); got != want {
		t.Fatalf("once the line moved:\n kept %s\n hits %s", got, want)
	}
}

// TestSameQueryOneBuild: the requests that ask a query while its report
// is being computed are handed that report, and one begun before a
// ResetCache is not kept for those after it.
func TestSameQueryOneBuild(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 10)
	sp, f := spanOf("7d", now, time.UTC), Filter{Country: "JP"}

	reading, hold := make(chan struct{}), make(chan struct{})
	var once sync.Once
	beforeRead = func() {
		once.Do(func() { close(reading); <-hold })
	}
	defer func() { beforeRead = nil }()

	const n = 6
	reps := make([]*Report, n)
	var wg sync.WaitGroup
	ask := func(i int) {
		wg.Go(func() {
			r, err := s.cached(sp, f, nil, now)
			if err != nil {
				t.Errorf("request %d: %v", i, err)
			}
			reps[i] = r
		})
	}
	ask(0)
	<-reading // the first is being computed, and stays so
	for i := 1; i < n; i++ {
		ask(i)
	}
	waitFor(t, "the others to wait on the first one's report", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.inflight) != 1 {
			t.Fatalf("%d reports being computed for one query", len(s.inflight))
		}
		for _, b := range s.inflight {
			return b.joined == n-1
		}
		return false
	})
	close(hold)
	wg.Wait()
	for i, r := range reps {
		if r == nil || r != reps[0] {
			t.Fatalf("request %d was not handed the first one's report", i)
		}
	}
	if again, err := s.cached(sp, f, nil, now); err != nil || again != reps[0] {
		t.Fatalf("the report was not kept for the next request (%v)", err)
	}

	// a report begun before ResetCache: finished for the one who asked, kept for no one
	reading, hold, once = make(chan struct{}), make(chan struct{}), sync.Once{}
	g := Filter{Country: "US"}
	done := make(chan *Report, 1)
	go func() {
		r, _ := s.cached(sp, g, nil, now)
		done <- r
	}()
	<-reading
	s.ResetCache()
	close(hold)
	first := <-done
	if first == nil {
		t.Fatal("the report begun before the reset was not finished")
	}
	if after, err := s.cached(sp, g, nil, now); err != nil || after == first {
		t.Fatalf("a report begun before ResetCache was kept past it (%v)", err)
	}
}

// TestKeepDaysStale: of the days written in one transaction, one counted
// before kept days were dropped is not kept — and what it would have
// replaced goes with it — while the ones counted since are; with a drop
// on its way, none is.
func TestKeepDaysStale(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	ctx := context.Background()
	id, _, err := s.db.viewID(ctx, viewOf(Filter{Country: "JP"}), time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	parts := func(n int64) map[string][]cell {
		return map[string][]cell{partSummary: {{"pageviews", n, 0}}, partHours: {}}
	}
	days := func() map[int64]bool {
		t.Helper()
		have, err := s.db.keptParts(ctx, id, 0, 1<<62)
		if err != nil {
			t.Fatal(err)
		}
		return have[partSummary]
	}
	const d1, d2, d3 = 86400000, 2 * 86400000, 3 * 86400000
	old := s.db.gen.Load()
	if err := s.db.keepDays(ctx, id, []keptDay{{d1, parts(1), old}, {d2, parts(2), old}}); err != nil {
		t.Fatal(err)
	}
	if h := days(); !h[d1] || !h[d2] {
		t.Fatalf("days counted under one gen: kept %v", h)
	}
	s.db.dropping()() // kept days were dropped: gen moved
	if err := s.db.keepDays(ctx, id, []keptDay{{d2, parts(20), old}, {d3, parts(3), s.db.gen.Load()}}); err != nil {
		t.Fatal(err)
	}
	if h := days(); !h[d1] || h[d2] || !h[d3] {
		t.Fatalf("a day counted across a drop: kept %v, want the first and the third", h)
	}
	done := s.db.dropping() // and one on its way
	if err := s.db.keepDays(ctx, id, []keptDay{{d2, parts(2), s.db.gen.Load()}}); err != nil {
		t.Fatal(err)
	}
	done()
	if h := days(); h[d2] {
		t.Fatalf("a day kept while days were being dropped: %v", h)
	}
}

// TestDeskLockIsItsOwn: the view without filters waits on no filtered
// view, and no filtered view on it.
func TestDeskLockIsItsOwn(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	for i := 1; i < len(s.views); i++ {
		s.views[i] <- struct{}{} // every filtered view's lock is held
	}
	unlock, err := s.lockView(short(), viewOf(Filter{}))
	if err != nil {
		t.Fatalf("the desk waited on a filtered view: %v", err)
	}
	for i := 1; i < len(s.views); i++ {
		<-s.views[i]
	}
	// with the desk's held, any filter still gets one
	for i := range 200 {
		u, err := s.lockView(short(), viewOf(Filter{Path: fmt.Sprintf("/p/%d", i)}))
		if err != nil {
			t.Fatalf("filter %d waited on the desk: %v", i, err)
		}
		u()
	}
	unlock()
}

// TestKeepBatches: a view's days are written together, and the report
// over them is the hits' whether they fit one transaction, or take
// many — written while other days are still being counted — or each
// is its own.
func TestKeepBatches(t *testing.T) {
	defer func(n int) { keepBatch = n }(keepBatch)
	for _, c := range []struct {
		reader bool
		batch  int
	}{{false, keepBatch}, {true, keepBatch}, {false, 8}, {true, 8}, {true, 1}} {
		reader := c.reader
		keepBatch = c.batch // 8 cells: a transaction every day or two, several at once over a reader
		s := dayStats(t, time.UTC, reader)
		now := time.Now()
		seedDays(t, s.db, time.UTC, now, 40)
		for _, f := range dayFilters {
			want := rawly(t, s, "30d", f, nil, now)
			if got := reportJSON(t, s, "30d", f, nil, now); got != want {
				t.Fatalf("reader %v, %+v:\n kept %s\n hits %s", reader, f, got, want)
			}
			if got := reportJSON(t, s, "30d", f, nil, now); got != want {
				t.Fatalf("reader %v, %+v, read again:\n kept %s\n hits %s", reader, f, got, want)
			}
		}
		var days, views int
		s.db.db.QueryRow(`SELECT COUNT(DISTINCT day), COUNT(DISTINCT view) FROM hits_days`).Scan(&days, &views)
		if days < 25 || views != len(dayFilters) {
			t.Fatalf("reader %v: %d days kept over %d views", reader, days, views)
		}
	}
}

// TestLineHoldsNoView: a report waiting its turn to count holds no view,
// so one whose days are kept — the same view, or one that shares its
// lock — is read while it waits; its turn come, it counts and is the
// hits'.
func TestLineHoldsNoView(t *testing.T) {
	for _, reader := range []bool{false, true} { // with a reader a report's two spans look at once
		lineHoldsNoView(t, reader)
	}
}

func lineHoldsNoView(t *testing.T, reader bool) {
	s := dayStats(t, time.UTC, reader)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 10)
	jp := Filter{Country: "JP"}
	want := rawly(t, s, "7d", jp, nil, now)

	leave, err := s.enter(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() {
		r, err := s.build(spanOf("7d", now, time.UTC), jp, nil, now)
		if err != nil {
			got <- err.Error()
			return
		}
		b, _ := json.Marshal(r)
		got <- string(b)
	}()
	waitFor(t, "the report to stand in line", func() bool { return s.queued.Load() == 1 })
	// its other span, if it looks at once, has let the view go too
	waitFor(t, "the report to hold no view", func() bool {
		for i := range s.views {
			if len(s.views[i]) != 0 {
				return false
			}
		}
		return true
	})
	time.Sleep(50 * time.Millisecond) // and takes none while it waits
	for i := range s.views {
		if len(s.views[i]) != 0 {
			t.Fatalf("reader %v: view lock %d is held by a report that only waits its turn", reader, i)
		}
	}
	if n := s.queued.Load(); n != 1 {
		t.Fatalf("reader %v: one report stands in %d places of the line", reader, n)
	}
	if known, err := s.db.viewKnown(context.Background(), viewOf(jp)); err != nil || known {
		t.Fatalf("the view was made before its turn came (%v, %v)", known, err)
	}
	leave()
	if g := <-got; g != want {
		t.Fatalf("its turn come:\n kept %s\n hits %s", g, want)
	}
	if n := len(s.counting); n != 0 || s.queued.Load() != 0 {
		t.Fatalf("after: %d counting, %d waiting", n, s.queued.Load())
	}
}

// TestCrowdCountsRight: many filters never asked before, asked at once,
// some sharing a view's lock: each report is the hits' or ErrBusy,
// never a view half counted and never a report that waits for ever,
// and the ones turned away are the hits' when asked again alone.
func TestCrowdCountsRight(t *testing.T) {
	for _, reader := range []bool{false, true} {
		s := dayStats(t, time.UTC, reader)
		now := time.Now()
		seedDays(t, s.db, time.UTC, now, 30)
		var fs []Filter
		for i := range 60 {
			fs = append(fs, Filter{Path: fmt.Sprintf("/p/%d", i)})
		}
		for _, c := range []string{"JP", "US", "CN"} {
			for _, d := range []string{"desktop", "mobile"} {
				fs = append(fs, Filter{Country: c, Device: d})
			}
		}
		got := make([]string, len(fs))
		busy := make([]bool, len(fs))
		var wg sync.WaitGroup
		for i, f := range fs {
			wg.Go(func() {
				r, err := s.build(spanOf("30d", now, time.UTC), f, nil, now)
				if errors.Is(err, ErrBusy) {
					busy[i] = true
					return
				}
				if err != nil {
					t.Errorf("%+v: %v", f, err)
					return
				}
				b, _ := json.Marshal(r)
				got[i] = string(b)
			})
		}
		wg.Wait()
		served, away := 0, 0
		for i, f := range fs {
			want := rawly(t, s, "30d", f, nil, now)
			if busy[i] {
				away++
				if again := reportJSON(t, s, "30d", f, nil, now); again != want {
					t.Fatalf("reader %v, %+v, asked again alone:\n kept %s\n hits %s", reader, f, again, want)
				}
				continue
			}
			served++
			if got[i] != want {
				t.Fatalf("reader %v, %+v, in the crowd:\n kept %s\n hits %s", reader, f, got[i], want)
			}
		}
		if served == 0 { // how many are turned away is the scheduler's: none on one core
			t.Fatalf("reader %v: none served, %d turned away", reader, away)
		}
		t.Logf("reader %v: %d served, %d turned away", reader, served, away)
	}
}

// TestDeskWaitsItsTurn: the view without filters is never turned away.
// With one report counting and the line full, the desk and Yesterday,
// their days not yet counted, wait — and are the hits' once the line
// has moved.
func TestDeskWaitsItsTurn(t *testing.T) {
	s := dayStats(t, time.UTC, false)
	now := time.Now()
	seedDays(t, s.db, time.UTC, now, 10)
	want := rawly(t, s, "7d", Filter{}, nil, now)

	ctx := context.Background()
	leave, err := s.enter(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var line sync.WaitGroup
	for range countQueue {
		line.Go(func() {
			if l, err := s.enter(ctx, false); err == nil {
				l()
			}
		})
	}
	waitFor(t, "the line to fill", func() bool { return int(s.queued.Load()) == countQueue })

	desk, yesterday := make(chan string, 1), make(chan error, 1)
	go func() {
		r, err := s.build(spanOf("7d", now, time.UTC), Filter{}, nil, now)
		if err != nil {
			desk <- err.Error()
			return
		}
		b, _ := json.Marshal(r)
		desk <- string(b)
	}()
	go func() {
		_, err := s.Yesterday(now)
		yesterday <- err
	}()
	select {
	case got := <-desk:
		t.Fatalf("the desk did not wait its turn: %s", got)
	case err := <-yesterday:
		t.Fatalf("Yesterday did not wait its turn: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	leave()
	line.Wait()
	if got := <-desk; got != want {
		t.Fatalf("the desk, its turn come:\n kept %s\n hits %s", got, want)
	}
	if err := <-yesterday; err != nil {
		t.Fatalf("Yesterday, its turn come: %v", err)
	}
}
