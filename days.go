package stats

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Days. A visitor is the day's hash and a session is the visitor's, so
// nobody is in two days: every number of a span is the sum of its days'.
// A day that has ended is counted once, in one pass over its hits, into
// parts — its summary, its hours, each ranked list in full — kept in
// hits_days, a packed row a part, under the view it was counted for
// (the filters, "" for none); a report over many days reads those
// instead of the days' hits, and sums them itself. Today, the
// minutes after a midnight while a hit may still be on its way to the
// store, and the ragged end of a span that does not begin at a midnight
// (24 hours) are read from the hits as before. Run keeps the days of the
// view without filters ahead of the desk; a filtered view's days are
// counted the first time a report asks. Over Options.Reader every
// query of a report, and every day being counted, runs on a connection
// of its own.

const (
	// sealDelay is how long after its midnight a day is counted: a hit
	// is written within a second or two of its time, and one that waits
	// longer drops the kept days it falls in (Add).
	sealDelay = 5 * time.Minute
	// longestDay is longer than any day is, at any clock change (Troll,
	// in Antarctica, had one of 26 hours).
	longestDay = int64(48 * time.Hour / time.Millisecond)
	// daysVersion is the counting's: a change to what a part holds
	// bumps it, and the kept days are counted again.
	daysVersion = "1"
	// maxViews is how many filtered views keep their days; past it the
	// one read longest ago gives its days back. viewIdle is how long a
	// filtered view keeps them unread. markEvery is how often a read
	// is written down.
	maxViews  = 32
	viewIdle  = 7 * 24 * time.Hour
	markEvery = 10 * time.Minute
	// countQueue is how many reports may wait for their turn to count
	// (enter); one more is told the desk is busy.
	countQueue = 8
)

// the parts of a day besides its lists, which go by their dimension
const (
	partSummary = "summary"
	partHours   = "hours"
)

// keepBatch is how many cells of counted days are written in one
// transaction: a commit is a sync of the disk, and a view never
// counted before has every day of its span to write. A filtered
// view's year is one commit; a busy site's whole day is its own.
// (A variable for a test to make small.)
var keepBatch = 1 << 16

// beforeRead runs between keep's counting and its read, for a test to
// change the kept days there.
var beforeRead func()

var (
	// errChanged is a report that found a kept day gone while it read:
	// hits were written under it, and it is counted again.
	errChanged = errors.New("stats: the kept days changed while they were read")
	// ErrBusy is a report that would have had days to count while
	// countQueue reports already wait to count theirs: nothing was
	// counted for it, and the same request a little later is served.
	// A host answers it with 503 (ServePage and ServeJSON do).
	ErrBusy = errors.New("stats: busy counting other views")
)

// daySpan is one day, [start, end) in ms.
type daySpan struct{ start, end int64 }

// cell is one kept number: a key and its count, and an hour's visitors.
type cell struct {
	k    string
	n, m int64
}

// counts are a stretch's numbers, from its kept days and its own hits.
type counts struct {
	tally
	hours  []HourRow
	firsts []first                   // where visitors begin, for the chart's day and month buckets
	lists  map[string]map[string]int // by dimension, every key
}

// first is n visitors at a moment: a visitor's first hit (n 1), or a
// kept day's midnight with every visitor of the day.
type first struct {
	at int64
	n  int
}

// want is what a report reads over a stretch beside its summary.
type want struct {
	hours, firsts bool
	dims          []string
}

func (w want) parts() []string {
	p := []string{partSummary}
	if w.hours {
		p = append(p, partHours)
	}
	return append(p, w.dims...)
}

// allParts is every part a day has: what the view without filters keeps.
func allParts() []string {
	p := []string{partSummary, partHours}
	for _, l := range listDims {
		p = append(p, l.dim)
	}
	sort.Strings(p[2:])
	return p
}

// viewOf is the name a filter's days are kept under: its dimensions,
// without the span; "" for none.
func viewOf(f Filter) string {
	f.From, f.To = 0, 0
	if f == (Filter{}) {
		return ""
	}
	b, _ := json.Marshal(f)
	return string(b)
}

// lockView holds a view's days while they are counted and read, so two
// reports do not count the same day and a view is not dropped from
// under one. The view without filters has a lock of its own, so the
// desk as it opens never waits on a filtered view's counting; the
// filtered views share the other 15: a collision only waits — until
// ctx is done.
func (s *Stats) lockView(ctx context.Context, view string) (unlock func(), err error) {
	ch := s.views[0]
	if view != viewOf(Filter{}) {
		h := fnv.New32a()
		h.Write([]byte(view))
		ch = s.views[1+h.Sum32()%uint32(len(s.views)-1)]
	}
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// enter takes a report's turn to count days from their hits, which is
// the one costly thing a report does: a pass over every hit of each
// day. One view counts at a time — its days already spread over the
// reader's connections — so however many filters are asked at once,
// the host's cores and its database are not all taken by the desk.
// At most countQueue reports wait; the next is ErrBusy at once. The
// keeper (wait) waits its turn however long the line is. A report
// enters through its turn, once for all it counts.
func (s *Stats) enter(ctx context.Context, wait bool) (leave func(), err error) {
	leave = func() { <-s.counting }
	select {
	case s.counting <- struct{}{}:
		return leave, nil
	default:
	}
	if !wait {
		if s.queued.Add(1) > countQueue {
			s.queued.Add(-1)
			return nil, ErrBusy
		}
		defer s.queued.Add(-1)
	}
	select {
	case s.counting <- struct{}{}:
		return leave, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// waits says a view's reports wait their turn to count however long
// the line: the view without filters, which every desk opens on.
func waits(f Filter) bool { return viewOf(f) == viewOf(Filter{}) }

// turn is a report's turn to count (enter): taken when the first of its
// spans finds days to count, shared by the others — a report counts its
// span and the span before under one turn — and given back when the
// report is built.
type turn struct {
	s    *Stats
	wait bool        // the keeper's, and the view without filters': however long the line
	has  atomic.Bool // taken — asked with a view held, so without mu, which take holds while it waits in line
	mu   sync.Mutex
	out  func() // leaves the turn; nil until it is taken
	err  error  // why it was not taken
}

// take takes the turn if the report has not yet; a report turned away
// once is turned away for good.
func (t *turn) take(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.out != nil || t.err != nil {
		return t.err
	}
	t.out, t.err = t.s.enter(ctx, t.wait)
	t.has.Store(t.err == nil)
	return t.err
}

func (t *turn) taken() bool { return t.has.Load() }

// done gives the turn back, if it was taken.
func (t *turn) done() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.out != nil {
		t.has.Store(false)
		t.out()
		t.out = nil
	}
}

// split cuts [from, to) into the days it holds whole that have ended
// sealDelay ago, and the stretches left at either end, read from the
// hits. A span with no such day is one stretch.
func (s *Stats) split(from, to, now time.Time) (days []daySpan, raw [][2]int64) {
	loc := s.coll.Location()
	cut := now.Add(-s.seal).UnixMilli()
	f := from
	if since := time.UnixMilli(s.since); f.Before(since) {
		f = since // the days before the zone was this one are not kept
	}
	f = f.In(loc)
	m := midnight(f.Year(), f.Month(), f.Day(), loc)
	if m.Before(f) {
		m = midnight(f.Year(), f.Month(), f.Day()+1, loc)
	}
	for {
		l := m.In(loc)
		e := midnight(l.Year(), l.Month(), l.Day()+1, loc)
		if e.After(to) || e.UnixMilli() > cut {
			break
		}
		days = append(days, daySpan{m.UnixMilli(), e.UnixMilli()})
		m = e
	}
	if len(days) == 0 {
		return nil, [][2]int64{{from.UnixMilli(), to.UnixMilli()}}
	}
	if a := from.UnixMilli(); a < days[0].start {
		raw = append(raw, [2]int64{a, days[0].start})
	}
	if b := to.UnixMilli(); days[len(days)-1].end < b {
		raw = append(raw, [2]int64{days[len(days)-1].end, b})
	}
	return days, raw
}

// over is a stretch's numbers under a filter: its whole days from the
// kept parts (counting the ones not kept yet), the rest from the hits
// in memory (live.go) or, when they are not all there, in the store,
// every query on a connection of its own (par). A list is every key
// when the stretch is pieced together, and cut at topMax when it is
// one stretch of hits, as Top is.
func (s *Stats) over(t *turn, f Filter, from, to, now time.Time, w want) (counts, error) {
	c := counts{lists: map[string]map[string]int{}}
	for _, d := range w.dims {
		c.lists[d] = map[string]int{}
	}
	var mu sync.Mutex // c's
	days, raw := s.split(from, to, now)
	var tasks []func() error
	if len(days) > 0 {
		tasks = append(tasks, func() error {
			fresh, err := s.keep(s.ctx, t, f, days, w.parts(), now, func(id, begun int64) error {
				return s.readKept(s.ctx, &c, &mu, id, begun, days, w)
			})
			if err == nil && fresh {
				s.tidy(now) // the cap on filtered views
			}
			return err
		})
	}
	limit := -1
	if len(days) == 0 {
		limit = topMax
	}
	for _, r := range raw {
		tasks = append(tasks, func() error {
			got, ok := s.liveOver(f, r[0], r[1], w)
			if !ok {
				var err error
				if got, err = s.hitsOver(f, r[0], r[1], w, limit); err != nil {
					return err
				}
			}
			mu.Lock()
			c.merge(got)
			mu.Unlock()
			return nil
		})
	}
	return c, s.par(tasks...)
}

// hitsOver reads [a, b) under a filter from the store: tally, Hours,
// VisitorFirst and each list's rank, every query on a connection of its
// own, the lists cut at limit (-1: whole).
func (s *Stats) hitsOver(f Filter, a, b int64, w want, limit int) (*counts, error) {
	c := &counts{lists: map[string]map[string]int{}}
	for _, d := range w.dims {
		c.lists[d] = map[string]int{}
	}
	var mu sync.Mutex // c's
	g := f
	g.From, g.To = a, b
	tasks := []func() error{func() error {
		t, err := s.rd.tally(g)
		mu.Lock()
		c.add(t)
		mu.Unlock()
		return err
	}}
	if w.hours {
		tasks = append(tasks, func() error {
			hs, err := s.rd.Hours(g)
			mu.Lock()
			c.hours = append(c.hours, hs...)
			mu.Unlock()
			return err
		})
	}
	if w.firsts {
		tasks = append(tasks, func() error {
			fs, err := s.rd.VisitorFirst(g)
			mu.Lock()
			for _, ts := range fs {
				c.firsts = append(c.firsts, first{ts, 1})
			}
			mu.Unlock()
			return err
		})
	}
	for _, d := range w.dims {
		tasks = append(tasks, func() error {
			rows, err := s.rd.rank(g, d, limit)
			mu.Lock()
			for _, row := range rows {
				c.lists[d][row.Key] += row.N
			}
			mu.Unlock()
			return err
		})
	}
	return c, s.par(tasks...)
}

// par runs the tasks, at most s.width at once, and returns the first
// error; once one has failed, the ones not begun are not. A width of 1
// runs them one after another, in order.
func (s *Stats) par(tasks ...func() error) error {
	if s.width <= 1 {
		for _, t := range tasks {
			if err := t(); err != nil {
				return err
			}
		}
		return nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var bad error
	failed := func() bool {
		mu.Lock()
		defer mu.Unlock()
		return bad != nil
	}
	sem := make(chan struct{}, s.width)
	for _, t := range tasks {
		sem <- struct{}{}
		if failed() {
			<-sem
			break
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := t(); err != nil {
				mu.Lock()
				if bad == nil {
					bad = err
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return bad
}

// runs cuts days into runs to be summed apart, a run a connection, so
// a long span's kept rows are summed on several at once: from a to b,
// their midnights.
func (s *Stats) runs(days []daySpan) [][2]int64 {
	per := max(7, (len(days)+s.width-1)/s.width)
	var out [][2]int64
	for i := 0; i < len(days); i += per {
		out = append(out, [2]int64{days[i].start, days[min(len(days), i+per)-1].start})
	}
	return out
}

// ranked is a list's keys by count, the most first and then by key —
// Top's order — cut at topMax.
func ranked(m map[string]int) []Row {
	rows := make([]Row, 0, len(m))
	for k, n := range m {
		rows = append(rows, Row{Key: k, N: n})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].N != rows[j].N {
			return rows[i].N > rows[j].N
		}
		return rows[i].Key < rows[j].Key
	})
	if len(rows) > topMax {
		rows = rows[:topMax]
	}
	return rows
}

// keep makes sure a view keeps the given parts of the days, counting a
// day that misses any of them from its hits — every part at once, each
// day on a connection of its own, in its turn (enter) and written
// keepBatch cells a transaction — and then, with the view still held,
// hands its id to read, with the time of the first hit (the days that
// end before it hold nothing). A read that finds a day gone (errChanged)
// counts again, a few times. fresh says the view is new. A report
// whose turn to count does not come is ErrBusy, and has kept nothing:
// a view is made only once its turn has.
//
// The turn is the report's (t; nil is a turn for this call alone), and
// is never waited for with the view held: a look that finds days to
// count lets the view go, takes the turn and looks again. So the ones
// in line hold no view, a view is waited for no longer than its days
// take to count, and nothing waits on the line but the line. The
// keeper, and the view without filters whoever asks — the desk as it
// opens, Today and Yesterday, in the minutes after a midnight before
// the keeper has counted the day — wait however long the line is and
// are never turned away (waits).
func (s *Stats) keep(ctx context.Context, t *turn, f Filter, days []daySpan, parts []string, now time.Time, read func(id, begun int64) error) (fresh bool, err error) {
	view := viewOf(f)
	if t == nil {
		t = &turn{s: s, wait: read == nil || waits(f)}
		defer t.done()
	}
	errTurn := errors.New("days to count, and no turn yet")
	look := func() error {
		unlock, err := s.lockView(ctx, view)
		if err != nil {
			return err
		}
		defer unlock()
		if !t.taken() {
			if known, err := s.rd.viewKnown(ctx, view); err != nil {
				return err
			} else if !known {
				return errTurn // every day of it is to count
			}
		}
		id, made, err := s.db.viewID(ctx, view, now.UnixMilli())
		if err != nil {
			return err
		}
		fresh = fresh || made
		have, err := s.rd.keptParts(ctx, id, days[0].start, days[len(days)-1].start)
		if err != nil {
			return err
		}
		begun, err := s.rd.firstHit(ctx)
		if err != nil {
			return err
		}
		var mu sync.Mutex // counted's, and cells'
		var counted []keptDay
		cells := 0
		write := func(all bool) error { // the counted days, once they hold keepBatch cells
			mu.Lock()
			batch := counted
			if len(batch) == 0 || !all && cells < keepBatch {
				mu.Unlock()
				return nil
			}
			counted, cells = nil, 0
			mu.Unlock()
			return s.db.keepDays(ctx, id, batch)
		}
		var tasks []func() error
		for _, d := range days {
			if d.end <= begun {
				continue // before the first hit there is nothing to count
			}
			missing := false
			for _, p := range parts {
				missing = missing || !have[p][d.start]
			}
			if !missing {
				continue
			}
			tasks = append(tasks, func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				gen := s.db.gen.Load()
				got, err := s.countDay(ctx, f, d)
				if err != nil {
					return err
				}
				mu.Lock()
				counted = append(counted, keptDay{d.start, got, gen})
				for _, cs := range got {
					cells += len(cs)
				}
				mu.Unlock()
				return write(false)
			})
		}
		if len(tasks) > 0 && !t.taken() {
			return errTurn
		}
		if err := s.par(tasks...); err != nil {
			write(true) // the days that were counted are kept all the same
			return err
		}
		if err := write(true); err != nil {
			return err
		}
		if read == nil {
			return nil
		}
		if beforeRead != nil {
			beforeRead()
		}
		return read(id, begun)
	}
	for try := 0; ; {
		err := look()
		switch {
		case err == errTurn:
			if err := t.take(ctx); err != nil {
				return fresh, err
			}
		case errors.Is(err, errChanged) && try < 4:
			try++
		default:
			return fresh, err
		}
	}
}

// countDay counts every part of one day in one pass over its hits —
// what tally, Hours and rank say, read once and summed here: the
// day's 17 queries each walked its hits again.
func (s *Stats) countDay(ctx context.Context, f Filter, d daySpan) (map[string][]cell, error) {
	g := f
	g.From, g.To = d.start, d.end
	w, args := g.clause(true)
	rows, err := s.rd.db.QueryContext(ctx, `SELECT id, ts, vid, sid, entry, bot, path, ref, chan, utm_campaign,
		country, region, city, lang, device, browser, os FROM hits WHERE `+w, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var p dayPass
	p.init()
	var id, ts, entry, bot int64
	var vid, sid sql.RawBytes
	var col [11]sql.RawBytes // path, ref, chan, utm_campaign, then the visitor dimensions after path
	for rows.Next() {
		if err := rows.Scan(&id, &ts, &vid, &sid, &entry, &bot, &col[0], &col[1], &col[2], &col[3],
			&col[4], &col[5], &col[6], &col[7], &col[8], &col[9], &col[10]); err != nil {
			return nil, err
		}
		if f.Bot == "" && bot == 1 {
			p.crawl(col[9], col[0]) // the Bots lists, over crawlers whatever the view
			continue
		}
		if f.Bot != "" {
			p.crawl(col[9], col[0])
		}
		p.add(id, ts, vid, sid, entry == 1, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return p.cells(), nil
}

// dayPass is one day's numbers summed in Go. A visitor, and each key of
// each list, is a small number for the day, so a distinct count is a
// set of pairs of small numbers.
type dayPass struct {
	pageviews int64
	vids      map[string]int32
	sessions  map[string]*session1
	hours     map[int64]int64     // page views by unix hour
	hourVis   map[uint64]struct{} // (hour, visitor)
	counts    map[string]map[string]int64
	keys      map[string]map[string]int32 // a visitor list's keys
	seen      map[string]map[uint64]struct{}
	paths     map[string]int32 // every path the day saw, for a session's exit
	pathNames []string
}

// session1 is a session as one day's pass sees it.
type session1 struct {
	n, min, max, last int64
	exit              int32 // its last page: a path's number (pathNames)
}

// the visitor lists, by the column each is read from (col's index)
var passVisitors = []struct {
	dim string
	col int
}{{"path", 0}, {"country", 4}, {"region", 5}, {"city", 6}, {"lang", 7}, {"device", 8}, {"browser", 9}, {"os", 10}}

func (p *dayPass) init() {
	p.vids, p.sessions = map[string]int32{}, map[string]*session1{}
	p.hours, p.hourVis = map[int64]int64{}, map[uint64]struct{}{}
	p.paths = map[string]int32{}
	p.counts, p.keys, p.seen = map[string]map[string]int64{}, map[string]map[string]int32{}, map[string]map[uint64]struct{}{}
	for _, d := range []string{"crawler", "botpath", "source", "channel", "campaign", "entry", "exit"} {
		p.counts[d] = map[string]int64{}
	}
	for _, v := range passVisitors {
		p.counts[v.dim], p.keys[v.dim], p.seen[v.dim] = map[string]int64{}, map[string]int32{}, map[uint64]struct{}{}
	}
}

func (p *dayPass) crawl(browser, path []byte) {
	p.counts["crawler"][string(browser)]++
	p.counts["botpath"][string(path)]++
}

func (p *dayPass) add(id, ts int64, vid, sid []byte, entry bool, col [11]sql.RawBytes) {
	p.pageviews++
	v, ok := p.vids[string(vid)]
	if !ok {
		v = int32(len(p.vids))
		p.vids[string(vid)] = v
	}
	ss := p.sessions[string(sid)]
	if ss == nil {
		ss = &session1{min: ts, max: ts}
		p.sessions[string(sid)] = ss
	}
	ss.n++
	ss.min, ss.max = min(ss.min, ts), max(ss.max, ts)
	if id >= ss.last {
		pn, ok := p.paths[string(col[0])]
		if !ok {
			pn = int32(len(p.pathNames))
			p.paths[string(col[0])] = pn
			p.pathNames = append(p.pathNames, string(col[0]))
		}
		ss.last, ss.exit = id, pn
	}
	h := ts / 3600000
	p.hours[h]++
	p.hourVis[uint64(h)<<32|uint64(uint32(v))] = struct{}{}
	if entry {
		p.counts["source"][string(col[1])]++
		p.counts["channel"][string(col[2])]++
		p.counts["entry"][string(col[0])]++
		if len(col[3]) > 0 {
			p.counts["campaign"][string(col[3])]++
		}
	}
	for _, d := range passVisitors {
		b := col[d.col]
		if len(b) == 0 {
			continue
		}
		keys := p.keys[d.dim]
		k, ok := keys[string(b)]
		if !ok {
			k = int32(len(keys))
			keys[string(b)] = k
		}
		pair := uint64(uint32(k))<<32 | uint64(uint32(v))
		if _, ok := p.seen[d.dim][pair]; !ok {
			p.seen[d.dim][pair] = struct{}{}
			p.counts[d.dim][string(b)]++
		}
	}
}

// cells are the pass's parts as they are kept.
func (p *dayPass) cells() map[string][]cell {
	out := map[string][]cell{partSummary: {}, partHours: {}} // kept for a day with nothing in it too
	var bounces, dur int64
	for _, ss := range p.sessions {
		if ss.n == 1 {
			bounces++
		}
		dur += ss.max - ss.min
		p.counts["exit"][p.pathNames[ss.exit]]++
	}
	for _, c := range []cell{{"pageviews", p.pageviews, 0}, {"visitors", int64(len(p.vids)), 0},
		{"sessions", int64(len(p.sessions)), 0}, {"bounces", bounces, 0}, {"duration", dur, 0}} {
		if c.n != 0 {
			out[partSummary] = append(out[partSummary], c)
		}
	}
	vis := map[int64]int64{}
	for pair := range p.hourVis {
		vis[int64(pair>>32)]++
	}
	for h, n := range p.hours {
		out[partHours] = append(out[partHours], cell{strconv.FormatInt(h, 10), n, vis[h]})
	}
	for d, m := range p.counts {
		cs := make([]cell, 0, len(m))
		for k, n := range m {
			cs = append(cs, cell{k, n, 0})
		}
		out[d] = cs
	}
	return out
}

// readKept adds a view's kept days to c (under cmu): the summary (and
// each day's visitors at its midnight, for the chart), the hours, the
// lists — each part read and summed in runs of days, a run on a
// connection of its own. A run must find every day that holds hits
// (from begun on): one gone since keep looked is errChanged, and then
// nothing is added, so the next try starts clean.
func (s *Stats) readKept(ctx context.Context, into *counts, cmu *sync.Mutex, id, begun int64, days []daySpan, w want) error {
	c := &counts{lists: map[string]map[string]int{}}
	for _, d := range w.dims {
		c.lists[d] = map[string]int{}
	}
	var mu sync.Mutex // c's
	// the days that can hold hits: from the first that ends after the
	// first hit on — before it, a day kept empty before a sweep is still
	// empty, and is not read
	for len(days) > 0 && days[0].end <= begun {
		days = days[1:]
	}
	if len(days) == 0 {
		return nil
	}
	held := func(a, b int64) int { // the days from a to b
		n := 0
		for _, d := range days {
			if d.start >= a && d.start <= b {
				n++
			}
		}
		return n
	}
	whole := func(n int, err error, a, b int64) error {
		if err == nil && n != held(a, b) {
			return errChanged
		}
		return err
	}
	a, b := days[0].start, days[len(days)-1].start
	tasks := []func() error{func() error {
		var t tally
		var fs []first
		n, err := s.rd.kept(ctx, id, partSummary, a, b, func(day int64, k []byte, n, _ int64) {
			switch string(k) {
			case "pageviews":
				t.Pageviews += int(n)
			case "visitors":
				t.Visitors += int(n)
				fs = append(fs, first{day, int(n)})
			case "sessions":
				t.Sessions += int(n)
			case "bounces":
				t.Bounces += int(n)
			case "duration":
				t.durMS += n
			}
		})
		mu.Lock()
		defer mu.Unlock()
		c.add(t)
		if w.firsts {
			c.firsts = append(c.firsts, fs...)
		}
		return whole(n, err, a, b)
	}}
	for _, r := range s.runs(days) {
		if w.hours {
			tasks = append(tasks, func() error {
				var hs []HourRow
				n, err := s.rd.kept(ctx, id, partHours, r[0], r[1], func(_ int64, k []byte, n, m int64) {
					if h, err := strconv.ParseInt(string(k), 10, 64); err == nil {
						hs = append(hs, HourRow{Hour: h, Pageviews: int(n), Visitors: int(m)})
					}
				})
				mu.Lock()
				c.hours = append(c.hours, hs...)
				mu.Unlock()
				return whole(n, err, r[0], r[1])
			})
		}
		for _, d := range w.dims {
			tasks = append(tasks, func() error {
				got := map[string]int{}
				n, err := s.rd.kept(ctx, id, d, r[0], r[1], func(_ int64, k []byte, n, _ int64) { got[string(k)] += int(n) })
				mu.Lock()
				into := c.lists[d]
				for k, n := range got {
					into[k] += n
				}
				mu.Unlock()
				return whole(n, err, r[0], r[1])
			})
		}
	}
	if err := s.par(tasks...); err != nil {
		return err
	}
	cmu.Lock()
	defer cmu.Unlock()
	into.merge(c)
	return nil
}

// merge adds another stretch's counts to c.
func (c *counts) merge(o *counts) {
	c.add(o.tally)
	c.hours = append(c.hours, o.hours...)
	c.firsts = append(c.firsts, o.firsts...)
	for d, m := range o.lists {
		into := c.lists[d]
		for k, n := range m {
			into[k] += n
		}
	}
}

// kept reads a part of a view over the days from a to b (their
// midnights), a cell at a time, and says how many days it read.
func (d *DB) kept(ctx context.Context, view int64, part string, a, b int64, each func(day int64, k []byte, n, m int64)) (int, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT day, data FROM hits_days WHERE view = ? AND part = ? AND day >= ? AND day <= ?`, view, part, a, b)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	days := 0
	for rows.Next() {
		var day int64
		var data sql.RawBytes
		if err := rows.Scan(&day, &data); err != nil {
			return days, err
		}
		if err := unpack(data, func(k []byte, n, m int64) { each(day, k, n, m) }); err != nil {
			return days, err
		}
		days++
	}
	return days, rows.Err()
}

// pack is a part's cells as kept: for each, the key's length, the key,
// its count and its second number (an hour's visitors), as uvarints.
func pack(cs []cell) []byte {
	b := []byte{} // an empty part is kept too: not NULL
	for _, c := range cs {
		b = binary.AppendUvarint(b, uint64(len(c.k)))
		b = append(b, c.k...)
		b = binary.AppendUvarint(b, uint64(c.n))
		b = binary.AppendUvarint(b, uint64(c.m))
	}
	return b
}

var errPacked = errors.New("stats: a kept day does not read")

func unpack(b []byte, each func(k []byte, n, m int64)) error {
	for len(b) > 0 {
		l, i := binary.Uvarint(b)
		if i <= 0 || uint64(len(b)-i) < l {
			return errPacked
		}
		k := b[i : i+int(l)]
		b = b[i+int(l):]
		n, i := binary.Uvarint(b)
		if i <= 0 {
			return errPacked
		}
		b = b[i:]
		m, i := binary.Uvarint(b)
		if i <= 0 {
			return errPacked
		}
		b = b[i:]
		each(k, int64(n), int64(m))
	}
	return nil
}

// viewID is the id a view's days are kept under, made the first time
// (fresh); a read is written down at most every markEvery.
func (d *DB) viewID(ctx context.Context, filter string, now int64) (id int64, fresh bool, err error) {
	var used int64
	err = d.db.QueryRowContext(ctx, `SELECT id, used FROM hits_views WHERE filter = ?`, filter).Scan(&id, &used)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := d.db.ExecContext(ctx, `INSERT INTO hits_views (filter, used) VALUES (?, ?)`, filter, now)
		if err != nil {
			return 0, false, err
		}
		id, err = res.LastInsertId()
		return id, true, err
	}
	if err != nil {
		return 0, false, err
	}
	if now-used >= markEvery.Milliseconds() {
		_, err = d.db.ExecContext(ctx, `UPDATE hits_views SET used = ? WHERE id = ?`, now, id)
	}
	return id, false, err
}

// keptParts is which parts a view keeps of the days from a to b.
func (d *DB) keptParts(ctx context.Context, view, a, b int64) (map[string]map[int64]bool, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT part, day FROM hits_days WHERE view = ? AND day >= ? AND day <= ?`, view, a, b)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[int64]bool{}
	for rows.Next() {
		var p string
		var day int64
		if err := rows.Scan(&p, &day); err != nil {
			return nil, err
		}
		if out[p] == nil {
			out[p] = map[int64]bool{}
		}
		out[p][day] = true
	}
	return out, rows.Err()
}

// firstHit is the time of the oldest hit, ms; with none, the end of time.
func (d *DB) firstHit(ctx context.Context) (int64, error) {
	var ms sql.NullInt64
	if err := d.db.QueryRowContext(ctx, `SELECT MIN(ts) FROM hits`).Scan(&ms); err != nil {
		return 0, err
	}
	if !ms.Valid {
		return 1<<63 - 1, nil
	}
	return ms.Int64, nil
}

// keptDay is a day as it was counted: its parts, and the store's gen
// from before the count.
type keptDay struct {
	day   int64
	parts map[string][]cell
	gen   int64
}

// keepDays writes counted days' parts for a view, each replacing what
// it held, in one transaction — unless kept days were dropped, or are
// being dropped, since a day's gen was read before it was counted:
// hits were written under it while it was, and it is counted again.
// The writes hold the write lock, so a drop either committed before
// the check, and moved gen or still holds busy, or commits after this
// one and takes the day with it.
func (d *DB) keepDays(ctx context.Context, view int64, days []keptDay) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	put, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO hits_days (view, part, day, data) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer put.Close()
	for _, k := range days {
		for p, cs := range k.parts {
			if _, err := put.ExecContext(ctx, view, p, k.day, pack(cs)); err != nil {
				return err
			}
		}
	}
	if d.busy.Load() != 0 {
		return nil
	}
	gen := d.gen.Load()
	for _, k := range days {
		if k.gen == gen {
			continue
		}
		// counted across a drop: not kept, and what it replaced was dropped
		if _, err := tx.ExecContext(ctx, `DELETE FROM hits_days WHERE view = ? AND day = ?`, view, k.day); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// viewKnown says whether a view has been made (viewID): one that has
// not keeps no day.
func (d *DB) viewKnown(ctx context.Context, filter string) (bool, error) {
	var id int64
	err := d.db.QueryRowContext(ctx, `SELECT id FROM hits_views WHERE filter = ?`, filter).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// dropDays forgets the kept days whose midnight is before ("<") or
// after (">") a time, in every view.
func dropDays(tx *sql.Tx, op string, ms int64) error {
	_, err := tx.Exec(`DELETE FROM hits_days WHERE day `+op+` ?`, ms)
	return err
}

// dropView forgets a view and every day it kept — the view of that id
// and those filters, so a trim that read the list before another one
// dropped it drops nothing.
func (d *DB) dropView(ctx context.Context, id int64, filter string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM hits_views WHERE id = ? AND filter = ?`, id, filter)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM hits_days WHERE view = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// countedBy drops every kept day unless they were counted by this
// version of the counting, at midnights in this zone, and says since
// when the zone has been this one: the days before were salted at
// another zone's midnights, so their visitors may be in two of this
// one's days, and are read from their hits. The zone is its name and
// its clock changes up to now, against the ones the zone last seen
// here had coming (rules, kept rulesAhead ahead and renewed every
// midnight by the keeper): a host's Local that moved is another zone,
// while new rules for days still to come are not. A first look at a
// database takes its hits as this zone's.
func (d *DB) countedBy(loc *time.Location, now time.Time) (since int64, err error) {
	meta := map[string]string{}
	rows, err := d.db.Query(`SELECT k, v FROM hits_meta`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			return 0, err
		}
		meta[k] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	since, _ = strconv.ParseInt(meta["since"], 10, 64)
	upto := now // the changes to compare: up to now, or as far as the ones kept went
	if ruled, err := strconv.ParseInt(meta["ruled"], 10, 64); err == nil && ruled < now.UnixMilli() {
		upto = time.UnixMilli(ruled)
	}
	same := meta["zone"] == loc.String() && meta["rules"] != "" && rules(loc, upto) == before(meta["rules"], upto)
	if meta["zone"] != "" && !same {
		since = now.UnixMilli()
	}
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if !same || meta["version"] != daysVersion {
		for _, q := range []string{`DELETE FROM hits_days`, `DELETE FROM hits_views`} {
			if _, err := tx.Exec(q); err != nil {
				return 0, err
			}
		}
	}
	for k, v := range map[string]string{"zone": loc.String(), "rules": rules(loc, now.Add(rulesAhead)),
		"ruled": strconv.FormatInt(now.Add(rulesAhead).UnixMilli(), 10), "version": daysVersion, "since": strconv.FormatInt(since, 10)} {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO hits_meta (k, v) VALUES (?, ?)`, k, v); err != nil {
			return 0, err
		}
	}
	return since, tx.Commit()
}

// rulesAhead is how far ahead the zone's clock changes are kept.
const rulesAhead = 2 * 365 * 24 * time.Hour

// rules are a zone's clock changes from 2000 up to a time: its offset
// then, and each change's moment with the offset after it.
func rules(loc *time.Location, upto time.Time) string {
	t := time.Date(2000, 1, 1, 0, 0, 0, 0, loc)
	_, off := t.Zone()
	b := []byte(strconv.Itoa(off))
	for {
		_, end := t.ZoneBounds()
		if end.IsZero() || !end.After(t) || !end.Before(upto) {
			return string(b)
		}
		_, off = end.Zone()
		b = fmt.Appendf(b, " %d:%d", end.Unix(), off)
		t = end
	}
}

// before is rules cut at a time: the changes before it.
func before(rules string, t time.Time) string {
	parts := strings.Fields(rules)
	for i, p := range parts[min(1, len(parts)):] {
		at, _, _ := strings.Cut(p, ":")
		if n, err := strconv.ParseInt(at, 10, 64); err != nil || !time.Unix(n, 0).Before(t) {
			return strings.Join(parts[:i+1], " ")
		}
	}
	return rules
}

// renewRules keeps the zone's clock changes ahead of now, so a Local
// that moves while this runs is still caught at the next start.
func (d *DB) renewRules(ctx context.Context, loc *time.Location, now time.Time) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ahead := now.Add(rulesAhead)
	for k, v := range map[string]string{"rules": rules(loc, ahead), "ruled": strconv.FormatInt(ahead.UnixMilli(), 10)} {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO hits_meta (k, v) VALUES (?, ?)`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// keptView is a filtered view: its id, its filters and when it was last read.
type keptView struct {
	id     int64
	filter string
	used   int64
}

// filteredViews lists the filtered views, the one read most recently first.
func (d *DB) filteredViews(ctx context.Context) ([]keptView, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT id, filter, used FROM hits_views WHERE filter <> '' ORDER BY used DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []keptView
	for rows.Next() {
		var v keptView
		if err := rows.Scan(&v.id, &v.filter, &v.used); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// trim gives back the days of the filtered views past maxViews, and of
// those unread for viewIdle.
func (s *Stats) trim(ctx context.Context) error {
	vs, err := s.db.filteredViews(ctx)
	if err != nil {
		return err
	}
	idle := time.Now().Add(-viewIdle).UnixMilli()
	for i, v := range vs {
		if i < maxViews && v.used >= idle {
			continue
		}
		unlock, err := s.lockView(ctx, v.filter)
		if err != nil {
			return err
		}
		err = s.db.dropView(ctx, v.id, v.filter)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// tidy trims the views and forgets the days before the horizon — the
// keeper's, every midnight; a report's when it made a view, and when
// nothing has for a day (no keeper runs without Run).
func (s *Stats) tidy(now time.Time) {
	s.tidied.Store(now.UnixMilli())
	err := s.db.renewRules(s.ctx, s.coll.Location(), now)
	if err == nil {
		err = s.trim(s.ctx)
	}
	if err == nil {
		err = s.dropBefore(s.ctx, horizon(now, s.coll.Location()))
	}
	if err != nil && s.ctx.Err() == nil {
		s.opt.Log("stats: days: %v", err)
	}
}

// horizon is where the oldest span the desk offers begins: the twelve
// months before the twelve months.
func horizon(now time.Time, loc *time.Location) time.Time {
	n := now.In(loc)
	return midnight(n.Year(), n.Month()-23, 1, loc)
}

// keeper is Run's other half: it loads the last three days' hits into
// memory (again after a midnight, if they grew past maxLive), counts the
// finished days of the view without filters back to
// the horizon, newest first, then again a few minutes after each
// midnight; and it gives back the days nothing reads.
// Stop cancels whatever it is doing and waits for it.
func (s *Stats) keeper() {
	defer close(s.kept)
	for {
		if !s.live.running() { // at start, and after a midnight if it let go
			if err := s.loadLive(s.ctx); err != nil && s.ctx.Err() == nil {
				s.opt.Log("stats: live: %v", err)
			}
		}
		now := time.Now()
		if err := s.fill(s.ctx, now); err != nil && s.ctx.Err() == nil {
			s.opt.Log("stats: days: %v", err)
		}
		s.tidy(now)
		loc := s.coll.Location()
		n := now.In(loc)
		next := midnight(n.Year(), n.Month(), n.Day()+1, loc).Add(sealDelay + time.Minute)
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
	}
}

// fill counts the view without filters a week at a time, newest first.
func (s *Stats) fill(ctx context.Context, now time.Time) error {
	days, _ := s.split(horizon(now, s.coll.Location()), now, now)
	parts := allParts()
	for i := len(days); i > 0; i -= 7 {
		if _, err := s.keep(ctx, nil, Filter{}, days[max(0, i-7):i], parts, now, nil); err != nil {
			return err
		}
	}
	return nil
}

// dropBefore forgets the days before a time, in every view.
func (s *Stats) dropBefore(ctx context.Context, t time.Time) error {
	tx, err := s.db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := dropDays(tx, "<", t.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
