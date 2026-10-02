package stats

import (
	"context"
	"database/sql"
	"runtime"
	"sync"
	"time"
)

// Live. The hits of the last three days — from the day before
// yesterday's midnight on — are kept in memory as well as in the store,
// fed by Add as each batch commits, so the stretches of hits a report
// reads — today, the 24 hours and the 24 before them, the Today view's
// hours of yesterday, the minutes after a midnight before yesterday is
// kept — are summed here instead of read back: on every core at once, a
// shard of visitors each. A visitor, and so a session, is in one shard, so the
// shards' sums add up, as the days' do. Run loads them at start; until
// it has, for a stretch that begins before them, and past maxLive, the
// store is read as before. Only hits written through this Stats' DB are
// fed: a host that writes hits around it reads the store (no Run).

// maxLive is the most hits a site keeps in memory, at about 180 bytes
// each with their words (v2ex.com's three days, 3 million, are half a
// gigabyte); past it they are read from the store.
var maxLive = 4 << 20

// liveRow is a hit in memory, every word of it a number in the dict.
type liveRow struct {
	id, ts     int64
	vid, sid   int32
	col        [11]int32 // countDay's columns: path, ref, chan, utm_campaign, country, region, city, lang, device, browser, os
	entry, bot bool
}

// dict numbers the words of the hits in memory.
type dict struct {
	ids   map[string]int32
	words []string
}

func (d *dict) id(s string) int32 {
	if n, ok := d.ids[s]; ok {
		return n
	}
	n := int32(len(d.words))
	d.ids[s] = n
	d.words = append(d.words, s)
	return n
}

func (d *dict) row(h Hit, id int64) liveRow {
	r := liveRow{id: id, ts: h.TS, vid: d.id(h.VID), sid: d.id(h.SID), entry: h.Entry, bot: h.Bot}
	for i, w := range [11]string{h.Path, h.Ref, h.Channel, h.UTMCampaign, h.Country, h.Region, h.City, h.Lang, h.Device, h.Browser, h.OS} {
		r.col[i] = d.id(w)
	}
	return r
}

// live is a Stats' hits in memory.
type live struct {
	mu      sync.RWMutex
	on      bool  // loaded, and under maxLive
	start   int64 // ms: the first instant held; every hit from it on is here
	rows    []liveRow
	dict    dict
	loading bool        // the store's writes wait in pending meanwhile
	pending []liveEvent // in the order they committed
	queued  int         // the load's hits, counted under the feed lock, and those in pending
	over    bool        // they passed maxLive: the load gives up
}

// liveEvent is a write the store committed while memory loaded: a batch
// with its ids, or a Sweep's cut (ms).
type liveEvent struct {
	hits []Hit
	ids  []int64
	cut  int64
}

// liveFrom is where the hits in memory begin at a moment: the day
// before yesterday's midnight, so the 24 hours before the last 24 are
// there too.
func liveFrom(now time.Time, loc *time.Location) int64 {
	n := now.In(loc)
	return midnight(n.Year(), n.Month(), n.Day()-2, loc).UnixMilli()
}

// fed takes a batch Add has committed, with the ids the store gave it.
// Add holds the store's feed lock from its commit through this, so
// memory takes the store's writes in the order they committed.
func (l *live) fed(hits []Hit, ids []int64, now time.Time, loc *time.Location) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loading {
		if l.queued += len(hits); l.queued > maxLive {
			l.over, l.pending = true, nil
		}
		if !l.over { // a copy: the collector writes its next batch over this one
			l.pending = append(l.pending, liveEvent{hits: append([]Hit(nil), hits...), ids: ids})
		}
		return
	}
	if !l.on {
		return
	}
	l.advance(now, loc)
	l.take(hits, ids)
}

// take adds a batch to memory, or lets memory go when it would pass
// maxLive. The caller holds the write lock.
func (l *live) take(hits []Hit, ids []int64) {
	if len(l.rows)+len(hits) > maxLive {
		l.off()
		return
	}
	for i, h := range hits {
		if h.TS >= l.start {
			l.rows = append(l.rows, l.dict.row(h, ids[i]))
		}
	}
}

// advance lets go of the oldest day once a midnight has passed, words
// and all. The caller holds the write lock.
func (l *live) advance(now time.Time, loc *time.Location) {
	want := liveFrom(now, loc)
	if !l.on || want <= l.start {
		return
	}
	nd := dict{ids: map[string]int32{}}
	to := make([]int32, len(l.dict.words))
	for i := range to {
		to[i] = -1
	}
	re := func(n int32) int32 {
		if to[n] < 0 {
			to[n] = nd.id(l.dict.words[n])
		}
		return to[n]
	}
	var rows []liveRow
	for _, r := range l.rows {
		if r.ts < want {
			continue
		}
		r.vid, r.sid = re(r.vid), re(r.sid)
		for i := range r.col {
			r.col[i] = re(r.col[i])
		}
		rows = append(rows, r)
	}
	l.rows, l.dict, l.start = rows, nd, want
}

// swept lets go of the hits a Sweep took: memory now begins where it
// cut, if that is later. Sweep holds the feed lock too.
func (l *live) swept(before int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loading {
		if !l.over {
			l.pending = append(l.pending, liveEvent{cut: before})
		}
		return
	}
	l.cut(before)
}

// cut lets go of the hits before a time. The caller holds the write lock.
func (l *live) cut(before int64) {
	if !l.on || before <= l.start {
		return
	}
	var rows []liveRow
	for _, r := range l.rows {
		if r.ts >= before {
			rows = append(rows, r)
		}
	}
	l.rows, l.start = rows, before
}

func (l *live) off() {
	l.on, l.rows, l.dict = false, nil, dict{}
}

// running says memory is loaded or loading.
func (l *live) running() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.on || l.loading
}

// liveLoaded runs before the load takes its snapshot (true) and after
// (false), for a test to write hits there.
var liveLoaded func(before bool)

// loadLive reads the last three days' hits into memory, in one read of
// the store whose snapshot is taken under the feed lock: every write
// the store committed before it is in the read, and every one after
// waits in pending, in order, and is played over it — no write in both,
// none in neither.
func (s *Stats) loadLive(ctx context.Context) error {
	l, loc := s.live, s.coll.Location()
	start := liveFrom(time.Now(), loc)
	if liveLoaded != nil {
		liveLoaded(true)
	}
	// the connection first, the feed lock after: with no Reader the
	// writer's one connection is the read's, and Add holds the lock
	// waiting on it
	tx, err := s.rd.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s.db.feedMu.Lock()
	l.mu.Lock()
	l.loading, l.pending, l.queued, l.over = true, nil, 0, false
	l.mu.Unlock()
	var n int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hits WHERE ts >= ?`, start).Scan(&n) // the snapshot
	l.mu.Lock()
	l.queued = n
	l.mu.Unlock()
	s.db.feedMu.Unlock()
	installed := false
	defer func() {
		if !installed {
			l.mu.Lock()
			l.loading, l.pending, l.queued, l.over = false, nil, 0, false
			l.mu.Unlock()
		}
	}()
	if err != nil {
		return err
	}
	if n > maxLive {
		s.opt.Log("stats: %d hits in three days, over %d: they are read from the store", n, maxLive)
		return nil
	}
	if liveLoaded != nil {
		liveLoaded(false)
	}
	q, err := tx.QueryContext(ctx, `SELECT id, ts, vid, sid, entry, bot, path, ref, chan, utm_campaign,
		country, region, city, lang, device, browser, os FROM hits WHERE ts >= ?`, start)
	if err != nil {
		return err
	}
	defer q.Close()
	d := dict{ids: map[string]int32{}}
	rows := make([]liveRow, 0, n)
	var id, ts, entry, bot int64
	var vid, sid sql.RawBytes
	var col [11]sql.RawBytes
	for q.Next() {
		if err := q.Scan(&id, &ts, &vid, &sid, &entry, &bot, &col[0], &col[1], &col[2], &col[3],
			&col[4], &col[5], &col[6], &col[7], &col[8], &col[9], &col[10]); err != nil {
			return err
		}
		r := liveRow{id: id, ts: ts, vid: d.id(string(vid)), sid: d.id(string(sid)), entry: entry == 1, bot: bot == 1}
		for i := range col {
			r.col[i] = d.id(string(col[i]))
		}
		rows = append(rows, r)
	}
	if err := q.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows, l.dict, l.start, l.on = rows, d, start, true
	if l.over {
		l.off()
	}
	for _, e := range l.pending {
		if !l.on {
			break
		}
		if e.hits == nil {
			l.cut(e.cut)
		} else {
			l.take(e.hits, e.ids)
		}
	}
	l.loading, l.pending, l.queued, l.over, installed = false, nil, 0, false, true
	l.advance(time.Now(), loc)
	return nil
}

// the lists a pass sums: the visitor lists first, in passVisitors' order
var liveDims = []string{"path", "country", "region", "city", "lang", "device", "browser", "os",
	"source", "channel", "entry", "campaign", "exit", "crawler", "botpath"}

const (
	lvSource = 8 + iota
	lvChannel
	lvEntry
	lvCampaign
	lvExit
	lvCrawler
	lvBotpath
	lvDims
)

// liveQuery is a filter in the dict's numbers.
type liveQuery struct {
	cols    [11]int32 // -1: any
	bots    bool      // crawlers' hits only (Bot "all" or a name)
	botName int32     // -1: any crawler
	empty   int32     // the number of "", -1 when no hit has it
	none    bool      // a word no hit in memory has: nothing matches
}

func (l *live) query(f Filter) liveQuery {
	q := liveQuery{botName: -1, empty: -1}
	if n, ok := l.dict.ids[""]; ok {
		q.empty = n
	}
	for i, v := range [11]string{f.Path, f.Source, f.Channel, f.Campaign, f.Country, f.Region, f.City, f.Lang, f.Device, f.Browser, f.OS} {
		q.cols[i] = -1
		if v != "" {
			n, ok := l.dict.ids[v]
			q.cols[i], q.none = n, q.none || !ok
		}
	}
	switch f.Bot {
	case "":
	case "all":
		q.bots = true
	default:
		n, ok := l.dict.ids[f.Bot]
		q.bots, q.botName, q.none = true, n, q.none || !ok
	}
	return q
}

// liveSession is a session as a pass sees it.
type liveSession struct {
	n, min, max, last int64
	exit              int32
}

// liveShard is one shard's sums: the visitors whose number falls to it.
type liveShard struct {
	t      tally
	vids   map[int32]int64 // a visitor's first hit
	sess   map[int32]*liveSession
	hours  map[int64]int
	hvis   map[uint64]struct{} // (hour since the stretch began, visitor)
	counts [lvDims]map[int32]int
	pairs  [8]map[uint64]struct{} // a visitor list's (word, visitor)
}

// liveOver sums [a, b) under a filter from memory, when every hit of it
// is there: what tally, Hours, VisitorFirst and rank say of it, every
// list whole.
func (s *Stats) liveOver(f Filter, a, b int64, w want) (*counts, bool) {
	if s.noLive {
		return nil, false
	}
	l, loc, now := s.live, s.coll.Location(), time.Now()
	l.mu.RLock()
	stale := l.on && liveFrom(now, loc) > l.start
	l.mu.RUnlock()
	if stale {
		l.mu.Lock()
		l.advance(now, loc)
		l.mu.Unlock()
	}
	l.mu.RLock()
	if !l.on || a < l.start {
		l.mu.RUnlock()
		return nil, false
	}
	rows, words, q := l.rows, l.dict.words, l.query(f)
	l.mu.RUnlock()

	c := &counts{lists: map[string]map[string]int{}}
	var need [lvDims]bool
	for _, d := range w.dims {
		c.lists[d] = map[string]int{}
		for i, n := range liveDims {
			need[i] = need[i] || n == d
		}
	}
	if q.none {
		return c, true
	}
	n := runtime.GOMAXPROCS(0)
	if len(rows) < 1<<14 {
		n = 1
	}
	base := a / 3600000
	shards := make([]liveShard, n)
	var wg sync.WaitGroup
	for k := range shards {
		wg.Go(func() {
			sh := &shards[k]
			sh.vids, sh.sess, sh.hours, sh.hvis = map[int32]int64{}, map[int32]*liveSession{}, map[int64]int{}, map[uint64]struct{}{}
			for i := range sh.counts {
				sh.counts[i] = map[int32]int{}
			}
			for i := range sh.pairs {
				sh.pairs[i] = map[uint64]struct{}{}
			}
			for i := range rows {
				r := &rows[i]
				if int(uint32(r.vid))%n != k || r.ts < a || r.ts >= b {
					continue
				}
				match := true
				for col, want := range q.cols {
					if want >= 0 && r.col[col] != want {
						match = false
						break
					}
				}
				if !match {
					continue
				}
				if q.bots {
					if !r.bot || (q.botName >= 0 && r.col[9] != q.botName) {
						continue
					}
				}
				if r.bot { // the Bots lists, over crawlers whatever the view
					if need[lvCrawler] {
						sh.counts[lvCrawler][r.col[9]]++
					}
					if need[lvBotpath] {
						sh.counts[lvBotpath][r.col[0]]++
					}
					if !q.bots {
						continue
					}
				}
				sh.add(r, &need, q.empty, base)
			}
			sh.t.Visitors, sh.t.Sessions = len(sh.vids), len(sh.sess)
			for _, ss := range sh.sess {
				if ss.n == 1 {
					sh.t.Bounces++
				}
				sh.t.durMS += ss.max - ss.min
				if need[lvExit] {
					sh.counts[lvExit][ss.exit]++
				}
			}
		})
	}
	wg.Wait()
	hours, hvis := map[int64]int{}, map[int64]int{}
	for k := range shards {
		sh := &shards[k]
		c.add(sh.t)
		for h, pv := range sh.hours {
			hours[h] += pv
		}
		for pair := range sh.hvis {
			hvis[base+int64(pair>>32)]++
		}
		if w.firsts {
			for _, ts := range sh.vids {
				c.firsts = append(c.firsts, first{ts, 1})
			}
		}
		for i, d := range liveDims {
			if need[i] {
				into := c.lists[d]
				for word, n := range sh.counts[i] {
					into[words[word]] += n
				}
			}
		}
	}
	if w.hours {
		for h, pv := range hours {
			c.hours = append(c.hours, HourRow{Hour: h, Pageviews: pv, Visitors: hvis[h]})
		}
	}
	return c, true
}

// add is one of the view's hits, as tally, Hours and rank count it.
func (sh *liveShard) add(r *liveRow, need *[lvDims]bool, empty int32, base int64) {
	sh.t.Pageviews++
	if f, ok := sh.vids[r.vid]; !ok || r.ts < f {
		sh.vids[r.vid] = r.ts
	}
	ss := sh.sess[r.sid]
	if ss == nil {
		ss = &liveSession{min: r.ts, max: r.ts}
		sh.sess[r.sid] = ss
	}
	ss.n++
	ss.min, ss.max = min(ss.min, r.ts), max(ss.max, r.ts)
	if r.id >= ss.last {
		ss.last, ss.exit = r.id, r.col[0]
	}
	h := r.ts / 3600000
	sh.hours[h]++
	sh.hvis[uint64(h-base)<<32|uint64(uint32(r.vid))] = struct{}{}
	if r.entry {
		if need[lvSource] {
			sh.counts[lvSource][r.col[1]]++
		}
		if need[lvChannel] {
			sh.counts[lvChannel][r.col[2]]++
		}
		if need[lvEntry] {
			sh.counts[lvEntry][r.col[0]]++
		}
		if need[lvCampaign] && r.col[3] != empty {
			sh.counts[lvCampaign][r.col[3]]++
		}
	}
	for i, v := range passVisitors {
		if !need[i] {
			continue
		}
		word := r.col[v.col]
		if word == empty {
			continue
		}
		pair := uint64(uint32(word))<<32 | uint64(uint32(r.vid))
		if _, ok := sh.pairs[i][pair]; !ok {
			sh.pairs[i][pair] = struct{}{}
			sh.counts[i][word]++
		}
	}
}
