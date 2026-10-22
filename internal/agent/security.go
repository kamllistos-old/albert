package agent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// maxBody is the largest request body the API accepts.
	maxBody = 64 << 10
	// tokenCookie carries the token for the native UI.
	tokenCookie = "albert_token"
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
//     is refused before anything else, including /login and the native UI.
//   - Origin, when present, must be the UI's; browsers also send
//     Sec-Fetch-Site, and a cross-site or same-site (other localhost port)
//     request is refused even without Origin.
//   - The token must match (Bearer header, or the cookie). Browsers get the
//     cookie only by trading a one-time code from POST /api/login-code
//     (which itself needs the token) at GET /login; nothing hands out the
//     token to an unauthenticated caller.
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
// to /api requests.
func (s *server) guardAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			s.refused.logf("refused %s %s from origin %q (fetch site %q)", r.Method, r.URL.Path, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
			writeError(w, http.StatusForbidden, "Albert only accepts requests from its own window.")
			return
		}
		if !s.validToken(r) {
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

// validToken checks the Bearer token, or, when there is no Authorization
// header, the token cookie. It compares in constant time, and an empty
// configured token matches nothing.
func (s *server) validToken(r *http.Request) bool {
	if s.cfg.Token == "" {
		return false
	}
	var got string
	if auth := r.Header.Get("Authorization"); auth != "" {
		scheme, tok, ok := strings.Cut(auth, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return false
		}
		got = strings.TrimSpace(tok)
	} else if c, err := r.Cookie(tokenCookie); err == nil {
		got = c.Value
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
}

// uiHandler serves the native UI, GET and HEAD only. It never sets the token
// cookie: browsers get that only through /login.
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

// loginCode mints a one-time code for /login. Callers must already hold the
// token, so only "albert up" (or another token holder) can open a browser
// session.
func (s *server) loginCode(w http.ResponseWriter, r *http.Request) {
	code, err := s.codes.mint()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Albert couldn't create a sign-in link.")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

// expiredPage is shown for any login code that is missing, wrong, used or
// expired.
const expiredPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Albert</title></head>
<body><p>This Albert link has expired. Run &quot;albert up&quot; again to open Albert.</p></body></html>
`

// login trades a one-time code for the token cookie (HttpOnly,
// SameSite=Strict) and sends the browser to the UI. It sits behind the Host
// check only: the browser has no token yet.
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	// HEAD (link previews, prefetchers) must not use up the code.
	if r.Method != http.MethodGet || !s.codes.redeem(r.URL.Query().Get("code")) {
		s.refused.logf("refused login: missing, wrong, used or expired code")
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, expiredPage)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    s.cfg.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
