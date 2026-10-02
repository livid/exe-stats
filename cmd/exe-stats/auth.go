package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Sign in with Solana. The wallet's address is the account: the page
// asks for a challenge naming the address, the wallet signs that text —
// a message, never a transaction — and the server checks the signature
// against the address itself, which is the raw ed25519 key. One
// signature opens a session kept in a cookie, so reading the desk asks
// the wallet for nothing more.

const (
	sessionCookie = "exe_stats"
	sessionLife   = 30 * 24 * time.Hour
	challengeLife = 5 * time.Minute
)

// challenges are the nonces handed out and not yet signed: each is for
// one address and is spent by the sign-in that uses it.
type challenges struct {
	mu sync.Mutex
	m  map[string]challenge // nonce -> what it was issued for
}

type challenge struct {
	addr, message string
	expires       time.Time
}

func (c *challenges) issue(host, addr string, now time.Time) string {
	nonce := newToken(16)
	msg := host + " wants you to sign in with your Solana account:\n" + addr +
		"\n\nSign in to see your sites. This is a message, never a transaction.\n\nNonce: " + nonce +
		"\nIssued At: " + now.UTC().Format(time.RFC3339)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]challenge{}
	}
	for k, v := range c.m {
		if now.After(v.expires) {
			delete(c.m, k)
		}
	}
	if len(c.m) > 10000 {
		return "" // a flood of challenges: no more until some lapse
	}
	c.m[nonce] = challenge{addr, msg, now.Add(challengeLife)}
	return msg
}

// spend takes the challenge a signed message carries, once.
func (c *challenges) spend(message string, now time.Time) (challenge, bool) {
	i := strings.Index(message, "\nNonce: ")
	if i < 0 {
		return challenge{}, false
	}
	nonce, _, _ := strings.Cut(message[i+len("\nNonce: "):], "\n")
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.m[nonce]
	if !ok {
		return challenge{}, false
	}
	delete(c.m, nonce)
	return ch, now.Before(ch.expires) && ch.message == message
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// sameOrigin says a request that changes something came from this
// site's own pages: the cookie is SameSite=Lax already, and this is the
// second lock on the same door.
func (a *App) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host == a.host(r)
	}
	s := r.Header.Get("Sec-Fetch-Site")
	return s == "same-origin" || s == "none"
}

// host is the name the server is reached by: the configured origin's,
// or the request's own on a scratch port.
func (a *App) host(r *http.Request) string {
	if a.originHost != "" {
		return a.originHost
	}
	return r.Host
}

// GET /v1/challenge?address= → the text to sign.
func (a *App) handleChallenge(w http.ResponseWriter, r *http.Request) {
	addr := r.URL.Query().Get("address")
	if _, ok := addressKey(addr); !ok {
		writeJSON(w, 400, map[string]string{"error": "that is not a Solana address"})
		return
	}
	if !a.limit.allow("c:"+clientIP(r), 10, 0.2) {
		writeJSON(w, 429, map[string]string{"error": "too many tries: wait a moment"})
		return
	}
	msg := a.chal.issue(a.host(r), addr, time.Now())
	if msg == "" {
		writeJSON(w, 503, map[string]string{"error": "busy: try again in a minute"})
		return
	}
	writeJSON(w, 200, map[string]string{"message": msg})
}

// POST /v1/signin {address, message, signature (base64)} → the cookie.
func (a *App) handleSignIn(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		writeJSON(w, 403, map[string]string{"error": "sign in from this site's own page"})
		return
	}
	var in struct{ Address, Message, Signature string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "unreadable"})
		return
	}
	now := time.Now()
	key, ok := addressKey(in.Address)
	sig, err := base64.StdEncoding.DecodeString(in.Signature)
	ch, known := a.chal.spend(in.Message, now)
	if !ok || err != nil || len(sig) != ed25519.SignatureSize || !known || ch.addr != in.Address ||
		!ed25519.Verify(ed25519.PublicKey(key), []byte(in.Message), sig) {
		writeJSON(w, 401, map[string]string{"error": "the signature does not match: sign in again"})
		return
	}
	if _, err := a.store.account(in.Address, now); err != nil {
		a.log("account: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not sign in"})
		return
	}
	token, err := a.store.newSession(in.Address, now, sessionLife)
	if err != nil {
		a.log("session: %v", err)
		writeJSON(w, 500, map[string]string{"error": "could not sign in"})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: a.secure, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionLife.Seconds())})
	// the holding is read as the wallet arrives, so the page that
	// follows already knows the tier
	a.checkNow(in.Address, false)
	writeJSON(w, 200, map[string]string{"address": in.Address})
}

// POST /v1/signout
func (a *App) handleSignOut(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "not from here", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.store.endSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: a.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// signedIn is the address behind the request's cookie, or "".
func (a *App) signedIn(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	return a.store.sessionAddr(c.Value, time.Now())
}
