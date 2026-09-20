# exe-stats

Server-side analytics for a Go web server's public pages, in one
import: which pages a browser opened, where the visitor came from, from
what country and on what device — counted as the page is served, with no
script and no cookie, and read back as a Mac OS 9 Platinum desk.

![The stats desk: the numbers over a week, the chart, Live, and the ranked lists](screenshot.png)

It was the [exe hub](https://github.com/livid/exe-hub)'s own analytics
and now stands on its own, so the hub and [exe](https://github.com/livid/exe)
draw the same desk over their own readers.

## Use it

```go
db, _ := sql.Open("sqlite", "app.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
db.SetMaxOpenConns(1)                        // SQLite takes one writer
st, err := stats.New(db, stats.Options{Location: time.Local})
go st.Run()

mux.HandleFunc("GET /{$}", st.Counted("home", home))   // a page that counts
mux.Handle("GET /stats", st.PageHandler())             // the desk
mux.Handle("GET /v1/stats", st.JSONHandler())          // the same report as JSON
```

`Handler` mounts the two at the paths the desk's own links use, `/stats`
and `/v1/stats`; mounted anywhere else its links would still point
there. `Counted` records a hit once the handler has answered 200, and only for
a request that was a person opening a page — a `fetch()`, a prefetch, a
404 and the page's own refetches are left out, and a crawler is kept
apart from every human number, under its name.

The host owns the database. This package opens no file and registers no
driver: it is handed a `*sql.DB`, keeps two tables in it (`hits` and
`hits_salt`) and leaves the rest alone, so a server can keep its readers
beside its own records.

The SQL is SQLite's. Hand it a pool the way the snippet does — WAL, a
busy timeout, and one connection — or a busy moment answers
`SQLITE_BUSY` and that second's hits are dropped rather than a request
delayed. A `:memory:` database needs the single connection even more:
with a pool, another connection is another empty database.

## What the desk shows

Visitors, page views, sessions, bounce rate and session time, each
against the span before; a chart of page views and visitors; who is
reading right now; and ranked lists of sources, channels and campaigns,
pages by top, entry and exit, countries, regions, cities and languages,
devices, browsers and systems, and the crawlers with the pages they
crawl. Every state is a URL — the range, a filter, each window's view —
so the page works without script and a view can be shared as a link.
With script the range last pressed is remembered in that browser, and a
view is fetched in place.

## Options

| field | what it does |
|---|---|
| `Location` | where the days begin: the salt rotates at midnight there and the buckets follow it (default UTC) |
| `PathLabel` | names a path in the lists — a hub names a thread by its words, a shop names a product (default: the path) |
| `Title` | the standalone page's window name (default "Stats") |
| `Log` | takes the collector's complaints and unreadable reports (default `log.Printf`) |
| `LiveHeader` | the header a page's own refetch carries, so it is not counted again (default `X-Hub-Live`) |
| `AnyClientKinds` | kinds of page counted whatever the client is, for a page tools read as much as browsers (the hub's agent guide) |
| `HomeURL`, `HomeLabel` | where the desk's close box leads (default `/`, "Home") |

`Stop` ends the writer, having written the hits it still held: call it
before closing the database.

## In your own chrome

A host whose pages have a look of their own renders the desk inside it
instead of taking the standalone page: parse `TemplateHTML()` into its
template set with `Funcs()`, put `statshead` in the document head with
`statscss`, and give the `stats` block what `PageData(r)` returns. That
is how the hub does it.

The blocks are named `statshead`, `statscss`, `stats` and `statsdisc`,
and the stylesheet's selectors all sit under `.sdesk`, so nothing of the
desk reaches the rest of a host's page. What the desk needs from the
host's own chrome is the Platinum window: `.window`, `.titlebar`,
`.tbox`, `.stripe`, `.title`, `.frame`, `.statusbar` and `.btn` — the
standalone page's `statsbase` block is a copy to start from.

## Privacy

A visitor is the day's hash of a secret salt, the address and the
browser. The salt is random, kept for the day so a restart keeps the day
whole, and then deleted — so yesterday's ids can never be recomputed and
no id can be turned back into a person. The address and the user agent
themselves are never stored: what a row holds is a country, a device, a
browser, a page and a time. There is no cookie, and nothing is sent to
anyone else.

Country, region and city come from the proxy's headers when there is one
(`CF-IPCountry`, `CF-Region`, `CF-IPCity`); without them those lists read
"Not known". An hour is counted where its middle falls, so in a zone offset by half
an hour the chart's first bucket may take an hour that began just before
it; the totals are exact either way. `Sweep` drops hits older than a time — a retention policy is
the host's to run, and there is none by default.
