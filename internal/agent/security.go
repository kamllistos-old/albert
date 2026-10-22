package agent

import (
	"crypto/subtle"
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
	// tokenCookie carries the token for the native UI.
	tokenCookie = "albert_token"
	// refusedEvery is the most often a refused request is logged.
	refusedEvery = 10 * time.Second
)

// The checks below stop a web page the user happens to visit, or a
// DNS-rebinding attacker, from driving the agent:
//
//   - Host must be one of ours, so a rebound name (evil.com -> 127.0.0.1)
//     is refused before anything else, including the native UI that hands
//     out the token cookie.
//   - Origin, when present, must be the UI's; browsers also send
//     Sec-Fetch-Site, and a cross-site or same-site (other localhost port)
//     request is refused even without Origin.
//   - The token must match (Bearer header, or the native UI's cookie).
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

// uiHandler serves the native UI (GET and HEAD only) and gives it the token
// as an HttpOnly, SameSite=Strict cookie, so the page's own requests carry it
// and its scripts never see it.
func (s *server) uiHandler(files http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "Albert's window can only be viewed.")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:     tokenCookie,
			Value:    s.cfg.Token,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		})
		files.ServeHTTP(w, r)
	})
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
