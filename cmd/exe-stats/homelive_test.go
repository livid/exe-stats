package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The homepage's stream: the site's day at once, and again when a view
// comes, the month's count with it; nothing for a stranger.
func TestHomeLive(t *testing.T) {
	defer func(d time.Duration) { liveTick = d }(liveTick)
	liveTick = 50 * time.Millisecond
	h := newHarness(t)
	w := newWallet()
	h.bal.set(w.addr, 10000)
	h.signIn(w)
	h.form(w, "/v1/sites", url.Values{"domain": {"shop.example"}})
	id := h.siteID(w, "shop.example")
	srv := httptest.NewServer(h.srv)
	defer srv.Close()
	if resp, err := http.Get(srv.URL + "/v1/home/live"); err != nil || resp.StatusCode != 401 {
		t.Fatalf("a stranger: %v %v", resp.StatusCode, err)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/home/live", nil)
	req.AddCookie(w.cookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	events := make(chan liveHome, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: ") && name == "home":
				var lh liveHome
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &lh)
				events <- lh
			}
		}
	}()
	next := func(what string) liveHome {
		select {
		case lh := <-events:
			return lh
		case <-time.After(5 * time.Second):
			t.Fatalf("no event: %s", what)
		}
		return liveHome{}
	}
	first := next("the first")
	if len(first.Sites) != 1 || first.Sites[0].ID != id || first.Sites[0].Today != "0 views today · 0 online" || first.Sites[0].Views != 0 || len(first.Sites[0].Bars) < 23 || first.Used != "0" {
		t.Fatalf("the first event: %+v", first)
	}
	if got := h.beacon(id, "https://shop.example/", "", "https://shop.example"); got != beaconCounted {
		t.Fatalf("beacon: %q", got)
	}
	// the month counts the view at once; the day once the site's writer
	// lands it, within a second: two events, or one
	after := next("after a view")
	for after.Sites[0].Today != "1 view today · 1 online" {
		after = next("the day with the view")
	}
	if after.Used != "1" || after.Sites[0].Views != 1 {
		t.Fatalf("after a view: %+v", after)
	}
	lit := 0
	for _, b := range after.Sites[0].Bars {
		if b.H > 0 {
			lit++
			if b.Class != "now" {
				t.Fatalf("the view's bar is not the hour still filling: %+v", b)
			}
		}
	}
	if lit != 1 {
		t.Fatalf("%d bars lit", lit)
	}
	select {
	case lh := <-events:
		t.Fatalf("an event with nothing changed: %+v", lh)
	case <-time.After(300 * time.Millisecond):
	}
}
