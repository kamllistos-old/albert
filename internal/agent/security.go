package agent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// maxBody is the largest request body the API accepts.
	maxBody = 64 << 10
	// sessionPath is the one /api route that needs no token: it is where the
	// UI trades a one-time code for it.
	sessionPath = "/api/session"
	// refusedEvery is the most often a refused request is logged.
	refusedEvery = 10 * time.Second
	// maxLoginCodes is how many unused login codes are kept.
	maxLoginCodes = 8
)

// loginCodeTTL is how long a login code works.
var loginCodeTTL = 60 * time.Second

// The checks below stop a web page the user happens to visit, or a
// DNS-rebinding attacker, from driving the agent:
//
//   - Host must be one of ours, so a rebound name (evil.com -> 127.0.0.1)
//     is refused before anything else, including the native UI.
//   - Origin, when present, must be the UI's; browsers also send
//     Sec-Fetch-Site, and a cross-site or same-site (other localhost port)
//     request is refused even without Origin.
//   - The token must match, sent as "Authorization: Bearer". There are no
//     cookies: browsers send those to every port on localhost. "albert up"
//     mints a one-time code (POST /api/login-code, which needs the token)
//     and opens the UI with it in the URL fragment, which never reaches a
//     server or a Referer. The page trades it at POST /api/session, the only
//     route without a token check, and keeps the token in localStorage,
//     which only the UI's own origin (scheme, host and port) can read.
//   - State-changing requests need X-Albert-Request: 1. Pages on other
//     origins cannot set it without a CORS preflight, and the agent never
//     answers one: it sends no CORS headers at all.

// guardHost refuses requests whose Host is not one of ours. It wraps every
// route.
func (s *server) guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		if !slices.ContainsFunc(s.cfg.AllowedHosts, func(a string) bool { return strings.EqualFold(a, r.Host) }) {
			s.refused.logf("refused request for host %q: not one of Albert's addresses", r.Host)
			writeError(w, http.StatusForbidden, "Albert only answers requests sent to its own address.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guardAPI applies the origin, token, header and size checks, in that order,
// to /api requests. Only POST /api/session (exactly) skips the token check.
func (s *server) guardAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			s.refused.logf("refused %s %s from origin %q (fetch site %q)", r.Method, r.URL.Path, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
			writeError(w, http.StatusForbidden, "Albert only accepts requests from its own window.")
			return
		}
		exempt := r.Method == http.MethodPost && r.URL.Path == sessionPath
		if !exempt && !s.validToken(r) {
			s.refused.logf("refused %s %s: missing or wrong token", r.Method, r.URL.Path)
			writeError(w, http.StatusUnauthorized, "Albert couldn't confirm this request came from you. Restart Albert with \"albert up\" and try again.")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Albert-Request") != "1" {
			s.refused.logf("refused %s %s: no X-Albert-Request header", r.Method, r.URL.Path)
			writeError(w, http.StatusForbidden, "This request is missing Albert's safety check, so it was blocked.")
			return
		}
		if r.ContentLength > maxBody {
			writeError(w, http.StatusRequestEntityTooLarge, "This request is too large.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether the request may come from the UI: any Origin
// must be a UI origin, and the browser must not flag it as coming from
// another site. Non-browser clients (curl, the CLI) send neither header.
func (s *server) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return false
	}
	origins := r.Header.Values("Origin")
	switch len(origins) {
	case 0:
		return true
	case 1:
		return slices.Contains(s.cfg.UIOrigins, origins[0])
	}
	return false
}

// validToken checks the Bearer token in constant time. Cookies are ignored,
// and an empty configured token matches nothing.
func (s *server) validToken(r *http.Request) bool {
	if s.cfg.Token == "" {
		return false
	}
	scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	got := strings.TrimSpace(tok)
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
}

// uiHandler serves the native UI, GET and HEAD only. It never hands out the
// token: the page gets that only through POST /api/session.
func (s *server) uiHandler(files http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "Albert's window can only be viewed.")
			return
		}
		files.ServeHTTP(w, r)
	})
}

// loginCode mints a one-time code for POST /api/session. Callers must
// already hold the token, so only "albert up" (or another token holder) can
// open a browser session.
func (s *server) loginCode(w http.ResponseWriter, r *http.Request) {
	code, err := s.codes.mint()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Albert couldn't create a sign-in link.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

// expiredLink is the error for any code that is missing, wrong, used or
// expired.
const expiredLink = `This Albert link has expired. Run "albert up" again to open Albert.`

// session trades a one-time code for the token. It is the only /api route
// without a token check (the page has none yet); the origin and
// X-Albert-Request checks still apply, so another site cannot call it.
func (s *server) session(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !s.codes.redeem(req.Code) {
		s.refused.logf("refused session: missing, wrong, used or expired code")
		writeError(w, http.StatusForbidden, expiredLink)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": s.cfg.Token})
}

// loginCodes holds the outstanding one-time login codes, oldest first.
type loginCodes struct {
	mu    sync.Mutex
	codes []issuedCode
}

type issuedCode struct {
	code    string
	expires time.Time
}

// mint returns a new random code, evicting the oldest beyond maxLoginCodes.
func (l *loginCodes) mint() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	code := hex.EncodeToString(b[:])
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(time.Now())
	l.codes = append(l.codes, issuedCode{code: code, expires: time.Now().Add(loginCodeTTL)})
	if n := len(l.codes) - maxLoginCodes; n > 0 {
		l.codes = append([]issuedCode(nil), l.codes[n:]...)
	}
	return code, nil
}

// redeem reports whether code is outstanding and unexpired, and if so uses it
// up. Every outstanding code is compared in constant time.
func (l *loginCodes) redeem(code string) bool {
	if code == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(time.Now())
	found := -1
	for i, c := range l.codes {
		if subtle.ConstantTimeCompare([]byte(code), []byte(c.code)) == 1 {
			found = i
		}
	}
	if found < 0 {
		return false
	}
	l.codes = append(l.codes[:found], l.codes[found+1:]...)
	return true
}

func (l *loginCodes) pruneLocked(now time.Time) {
	kept := l.codes[:0]
	for _, c := range l.codes {
		if now.Before(c.expires) {
			kept = append(kept, c)
		}
	}
	l.codes = kept
}

// outstanding is how many unexpired codes are waiting to be used.
func (l *loginCodes) outstanding() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(time.Now())
	return len(l.codes)
}

// refusalLog logs refused requests at most once every refusedEvery, with a
// count of the ones skipped, so a hostile page cannot flood albert.log.
type refusalLog struct {
	mu      sync.Mutex
	last    time.Time
	skipped int
}

func (l *refusalLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && time.Since(l.last) < refusedEvery {
		l.skipped++
		return
	}
	msg := fmt.Sprintf(format, args...)
	if l.skipped > 0 {
		msg += fmt.Sprintf(" (and %d more refused since the last report)", l.skipped)
	}
	l.last, l.skipped = time.Now(), 0
	logf("%s", msg)
}
