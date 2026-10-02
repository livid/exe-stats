package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A site's icon on its row of the Sites window: the site's own favicon,
// fetched by this server — never by the reader's browser — from the
// site's home page's <link rel="icon">, else /favicon.ico, and kept in
// app.db for a week (a day when there is none). Only a raster image
// (PNG, ICO, GIF, JPEG, WebP) of at most iconMax counts, and it is
// served back to the owner alone, sandboxed. The domain is the owner's
// to type, so the fetch refuses, at the dial, any address that is not a
// public one: this machine, the tailnet, a home network.

const (
	iconMax   = 256 << 10 // bytes: a bigger image is no icon
	pageMax   = 512 << 10 // of the home page, read for its <link>s
	iconFresh = 7 * 24 * time.Hour
	iconRetry = 24 * time.Hour // when the site had none, or did not answer
	iconBusy  = 4              // fetches at once, over every site
)

// iconState is what the server knows of a site's icon.
type iconState struct {
	read  bool   // the store has been asked
	typ   string // the image's type; "" none
	at    int64  // ms: when it was fetched, or found missing
	busy  bool   // a fetch is on its way
	stamp string // a short hash of the image, for the img's URL
}

// ---- the fetch ----

var errNotPublic = errors.New("not a public address")

// reserved are the blocks that are neither private nor loopback to the
// net package's eye and still no public site's: the tailnet's CGNAT,
// the IPv6 forms that carry an IPv4 address inside (compatible,
// translated, NAT64, 6to4, Teredo), the old site-local block,
// documentation and benchmarking.
var reserved = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001:db8::/32",
		"::/96", "::ffff:0:0:0/96", "2002::/16", "2001::/32", "fec0::/10"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// publicIP says an address is a public one, a site's.
func publicIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range reserved {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// iconPublic is publicIP, and a test's to replace.
var iconPublic = publicIP

// iconClient fetches from the sites: every connection — redirects
// included, whatever the name resolved to — is checked as it is dialled,
// and no proxy from the environment stands between.
var iconClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext: (&net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if ip := net.ParseIP(host); err != nil || ip == nil || !iconPublic(ip) {
				return errNotPublic
			}
			return nil
		}}).DialContext,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  5 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives:      true,
	},
	CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// iconHomes are where a site's home page is asked for: https, then
// http. A test points them at its own server.
var iconHomes = func(domain string) []string {
	return []string{"https://" + domain + "/", "http://" + domain + "/"}
}

// answered is a site's answer that is no image: final when the site
// said so (a 404, a page too big), not when it was only busy (a 5xx).
type answered struct {
	msg   string
	final bool
}

func (e answered) Error() string { return e.msg }

func iconGet(ctx context.Context, u string, max int64) ([]byte, *url.URL, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, "", err
	}
	req.Header.Set("User-Agent", "exe-stats (a site's icon for its owner; https://stats.v2core.com)")
	resp, err := iconClient.Do(req)
	if err != nil {
		return nil, nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, "", answered{fmt.Sprintf("%s: %s", u, resp.Status), resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, nil, "", err
	}
	if int64(len(b)) > max {
		return nil, nil, "", answered{fmt.Sprintf("%s: over %d bytes", u, max), true}
	}
	return b, resp.Request.URL, resp.Header.Get("Content-Type"), nil
}

var (
	linkTag = regexp.MustCompile(`(?is)<link\b[^>]*>`)
	attr    = regexp.MustCompile(`(?is)([a-z-]+)\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)
)

// iconLinks are a page's icons, the likeliest first: one said to be 32
// pixels or more, the smallest such; one of no size; an apple touch
// icon; then /favicon.ico. A vector icon is passed over: it is no
// raster.
func iconLinks(page []byte, base *url.URL) []string {
	type cand struct {
		href string
		rank int
		size int
	}
	var cs []cand
	for _, tag := range linkTag.FindAll(page, -1) {
		a := map[string]string{}
		for _, m := range attr.FindAllSubmatch(tag, -1) {
			a[strings.ToLower(string(m[1]))] = html.UnescapeString(string(m[2]) + string(m[3]) + string(m[4]))
		}
		rel := " " + strings.ToLower(a["rel"]) + " "
		href := strings.TrimSpace(a["href"])
		if href == "" || !strings.Contains(rel, "icon") || strings.Contains(strings.ToLower(a["type"]), "svg") || strings.Contains(rel, "mask-icon") {
			continue
		}
		size := 0 // the smallest of 32 or more it offers, else its largest
		for _, sz := range strings.Fields(strings.ToLower(a["sizes"])) {
			w, _, _ := strings.Cut(sz, "x")
			n, err := strconv.Atoi(w)
			switch {
			case err != nil:
			case n >= 32 && (size < 32 || n < size):
				size = n
			case size < 32 && n > size:
				size = n
			}
		}
		c := cand{href: href, size: size}
		switch {
		case strings.Contains(rel, " apple-touch-icon"):
			c.rank = 3
		case size >= 32:
			c.rank = 0
		case size == 0:
			c.rank = 1
		default:
			c.rank = 2
		}
		cs = append(cs, c)
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].rank != cs[j].rank {
			return cs[i].rank < cs[j].rank
		}
		return cs[i].size < cs[j].size
	})
	var out []string
	seen := map[string]bool{}
	add := func(href string) {
		u, err := base.Parse(href)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || seen[u.String()] || strings.HasSuffix(strings.ToLower(u.Path), ".svg") {
			return
		}
		seen[u.String()] = true
		out = append(out, u.String())
	}
	for _, c := range cs {
		if len(out) < 4 { // the page's own, four at most; /favicon.ico always gets its turn
			add(c.href)
		}
	}
	add("/favicon.ico")
	return out
}

// sniff is a raster image's type by its first bytes, "" for anything else.
func sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte{0, 0, 1, 0}):
		return "image/x-icon"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// fetchIcon is a site's icon, or none: nil and no error only when the
// site answered every ask and none was a raster; a site that was down or
// busy for any of them is an error, so the icon it had is kept.
func fetchIcon(ctx context.Context, domain string) ([]byte, string, error) {
	var links []string
	var last error
	for _, home := range iconHomes(domain) {
		page, base, typ, err := iconGet(ctx, home, pageMax)
		if err != nil {
			last = err
			continue
		}
		if !strings.Contains(strings.ToLower(typ), "html") {
			page = nil
		}
		links, last = iconLinks(page, base), nil
		break
	}
	if last != nil {
		return nil, "", last
	}
	var failed error
	for _, u := range links {
		b, _, _, err := iconGet(ctx, u, iconMax)
		var a answered
		switch {
		case err == nil:
			if typ := sniff(b); typ != "" {
				return b, typ, nil
			}
		case errors.As(err, &a) && a.final:
		default:
			failed = err
		}
	}
	return nil, "", failed
}

// ---- kept ----

func (s *Store) icon(site string) (data []byte, typ string, at int64, err error) {
	err = s.db.QueryRow(`SELECT data, type, at FROM icons WHERE site=?`, site).Scan(&data, &typ, &at)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *Store) iconSeen(site string) (typ string, at int64, sum []byte, err error) {
	err = s.db.QueryRow(`SELECT type, at, sha FROM icons WHERE site=?`, site).Scan(&typ, &at, &sum)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return
}

func (s *Store) saveIcon(site string, data []byte, typ string, at int64) error {
	sum := sha256.Sum256(data)
	_, err := s.db.Exec(`INSERT INTO icons (site, data, type, at, sha) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (site) DO UPDATE SET data=excluded.data, type=excluded.type, at=excluded.at, sha=excluded.sha`,
		site, data, typ, at, sum[:8])
	return err
}

// ---- on the page ----

// iconOf is a site's icon as its row shows it — whether there is one,
// and its stamp — asking for a fresh one when it is due and a fetch is
// free. The caller holds a.mu.
func (a *App) iconOf(e *siteEntry, now time.Time) (bool, string) {
	if a.iconDue(e, now) {
		select {
		case a.iconSem <- struct{}{}:
			a.startIcon(e)
		default: // enough on their way: the next look asks
		}
	}
	return e.icon.typ != "", e.icon.stamp
}

// iconDue reads what the store knows of a site's icon, once, and says a
// fetch is due and none is on its way. The caller holds a.mu.
func (a *App) iconDue(e *siteEntry, now time.Time) bool {
	ic := &e.icon
	if !ic.read {
		typ, at, sum, err := a.store.iconSeen(e.site.ID)
		if err != nil {
			a.log("icon %s: %v", e.site.ID, err)
		}
		ic.read, ic.typ, ic.at, ic.stamp = true, typ, at, fmt.Sprintf("%x", sum)
	}
	due := iconFresh
	if ic.typ == "" {
		due = iconRetry
	}
	return !ic.busy && now.Sub(time.UnixMilli(ic.at)) >= due
}

// startIcon sends a fetch on its way, holding a slot of iconSem it
// gives back. The caller holds a.mu.
func (a *App) startIcon(e *siteEntry) {
	e.icon.busy = true
	go a.refreshIcon(e, e.site.ID, e.site.Domain)
}

// warmIcons asks, at start, for every icon that is due — waiting for a
// slot with a.mu let go, so the fetches on their way can finish — and
// the Sites window has them by the time its owner looks.
func (a *App) warmIcons() {
	a.mu.Lock()
	var all []*siteEntry
	for _, e := range a.sites {
		all = append(all, e)
	}
	a.mu.Unlock()
	go func() {
		for _, e := range all {
			a.iconSem <- struct{}{}
			a.mu.Lock()
			if a.sites[e.site.ID] == e && a.iconDue(e, time.Now()) {
				a.startIcon(e)
			} else {
				<-a.iconSem
			}
			a.mu.Unlock()
		}
	}()
}

// refreshIcon fetches a site's icon and keeps it, or keeps that there
// is none; a site that did not answer is asked again in a day, keeping
// the icon it had.
func (a *App) refreshIcon(e *siteEntry, id, domain string) {
	defer func() { <-a.iconSem }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	data, typ, err := fetchIcon(ctx, domain)
	now := time.Now().UnixMilli()
	a.mu.Lock()
	defer a.mu.Unlock()
	e.icon.busy = false
	if a.sites[id] != e {
		return // removed meanwhile
	}
	if err != nil { // no answer: asked again in a day, the icon it had kept
		e.icon.at = now
		if e.icon.typ != "" {
			e.icon.at = now - (iconFresh - iconRetry).Milliseconds()
		}
		return
	}
	if err := a.store.saveIcon(id, data, typ, now); err != nil {
		a.log("icon %s: %v", id, err)
		return
	}
	sum := sha256.Sum256(data)
	e.icon.typ, e.icon.at, e.icon.stamp = typ, now, fmt.Sprintf("%x", sum[:8])
}

// GET /sites/{id}/icon — the site's icon, for its owner and an admin: a
// raster image served as nothing but one.
func (a *App) handleSiteIcon(w http.ResponseWriter, r *http.Request) {
	addr := a.signedIn(r)
	a.mu.Lock()
	e := a.ownSite(addr, r.PathValue("id"))
	if e == nil && a.isAdmin(addr) {
		e = a.sites[r.PathValue("id")]
	}
	a.mu.Unlock()
	if addr == "" || e == nil {
		http.NotFound(w, r)
		return
	}
	data, typ, _, err := a.store.icon(e.site.ID)
	if err != nil || typ == "" || len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, max-age=604800") // the URL carries the image's stamp
	w.Write(data)
}
