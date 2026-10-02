package main

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// The hosts a site counts. A site counts the pages of its domain, and
// of more hosts its owner lists on its settings page: a name
// (blog.example.com, example.github.io), or a name under "*." for that
// name and every name below it. Each also counts its www. name, as the
// domain does, since every host is read through hostOf.

// maxHosts is how many hosts a site may list besides its domain.
const maxHosts = 20

// hostEntry reads what the owner typed as a host to list: a name, a
// whole address pasted from the browser, or "*." and a name.
func hostEntry(in string) (string, bool) {
	in = strings.TrimSpace(in)
	if rest, ok := strings.CutPrefix(in, "*."); ok {
		d, ok := domainOf(rest)
		return "*." + d, ok
	}
	return domainOf(in)
}

// under says whether host is base or a name below it.
func under(host, base string) bool {
	return host == base || strings.HasSuffix(host, "."+base)
}

// counts says whether a page on host (as hostOf reads it) is the site's.
func (s Site) counts(host string) bool {
	if host == s.Domain {
		return true
	}
	for _, h := range s.Hosts {
		if base, ok := strings.CutPrefix(h, "*."); ok {
			if under(host, base) {
				return true
			}
		} else if host == h {
			return true
		}
	}
	return false
}

// covered says whether a host entry would add nothing: its name counts
// already, or a "*." entry above it holds every name it would.
func (s Site) covered(entry string) bool {
	base, wild := strings.CutPrefix(entry, "*.")
	if !wild {
		return s.counts(entry)
	}
	for _, h := range s.Hosts {
		if b, ok := strings.CutPrefix(h, "*."); ok && under(base, b) {
			return true
		}
	}
	return false
}

// pathOf is how a view's page is listed on the desk: by its path on the
// site's domain, and by host and path on another of its hosts, so the
// front page of a blog is not the front page of the shop.
func (s Site) pathOf(host, path string) string {
	if path == "" {
		path = "/"
	}
	if host == s.Domain {
		return path
	}
	return host + path
}

// ownRef is a referrer from one of the site's own hosts: a step inside
// the site, which is no source, as a link within one host is not.
func (s Site) ownRef(ref string) bool {
	u, err := url.Parse(ref)
	return err == nil && u.Host != "" && s.counts(hostOf(u.Host))
}

type hostRow struct {
	Name, Note string
}

// hostRows are the site's hosts as its settings list them.
func hostRows(s Site) []hostRow {
	var out []hostRow
	for _, h := range s.Hosts {
		r := hostRow{Name: h}
		if strings.HasPrefix(h, "*.") {
			r.Note = "Every subdomain"
		}
		out = append(out, r)
	}
	return out
}

// backTo answers a settings form: the settings again, with the word
// that says what happened, at the section that was pressed.
func backTo(w http.ResponseWriter, r *http.Request, id, key, word, anchor string) {
	http.Redirect(w, r, "/sites/"+id+"/settings?"+key+"="+url.QueryEscape(word)+"#"+anchor, http.StatusSeeOther)
}

// editHosts runs one change to a site's hosts for its owner, and writes
// it down. change returns the new list, or a fault word.
func (a *App) editHosts(w http.ResponseWriter, r *http.Request, done string, change func(Site, string) ([]string, string)) {
	addr := a.owner(w, r)
	if addr == "" {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	in := r.FormValue("host")
	id := r.PathValue("id")
	a.mu.Lock()
	defer a.mu.Unlock()
	e := a.ownSite(addr, id)
	if e == nil {
		back(w, r, "e", "gone")
		return
	}
	hosts, fault := change(e.site, in)
	if fault != "" {
		backTo(w, r, id, "e", fault, "hosts")
		return
	}
	if err := a.store.setHosts(id, hosts); err != nil {
		a.log("hosts: %v", err)
		backTo(w, r, id, "e", "failed", "hosts")
		return
	}
	e.site.Hosts = hosts
	if ac := a.accts[addr]; ac != nil {
		ac.sites, _ = a.store.sitesOf(addr)
	}
	backTo(w, r, id, "m", done, "hosts")
}

// POST /v1/sites/{id}/hosts — count one more host.
func (a *App) handleAddHost(w http.ResponseWriter, r *http.Request) {
	a.editHosts(w, r, "hostadded", func(s Site, in string) ([]string, string) {
		h, ok := hostEntry(in)
		switch {
		case !ok:
			return nil, "host"
		case s.covered(h):
			return nil, "counts"
		case len(s.Hosts) >= maxHosts:
			return nil, "hostmax"
		}
		return append(slices.Clone(s.Hosts), h), ""
	})
}

// POST /v1/sites/{id}/hosts/remove — stop counting a host.
func (a *App) handleRemoveHost(w http.ResponseWriter, r *http.Request) {
	a.editHosts(w, r, "hostremoved", func(s Site, in string) ([]string, string) {
		i := slices.Index(s.Hosts, in)
		if i < 0 {
			return nil, "nohost"
		}
		return slices.Delete(slices.Clone(s.Hosts), i, i+1), ""
	})
}
