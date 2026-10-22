package agent

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kamllistos/albert/internal/provider"
)

func withHeader(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func withoutHeader(k string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Del(k) }
}

func withHost(h string) func(*http.Request) {
	return func(r *http.Request) { r.Host = h }
}

func withCookie(v string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: "albert_token", Value: v})
	}
}

func TestMissingToken401(t *testing.T) {
	e := newEnv(t, testConfig())
	for _, path := range []string{"/api/health", "/api/tools", "/api/events", "/api/jobs"} {
		wantError(t, serve(e.h, request("GET", path, "", withoutHeader("Authorization"))), http.StatusUnauthorized)
	}
	wantError(t, serve(e.h, request("POST", "/api/refresh", "", withoutHeader("Authorization"))), http.StatusUnauthorized)
}

func TestWrongToken401(t *testing.T) {
	e := newEnv(t, testConfig())
	bad := []func(*http.Request){
		withHeader("Authorization", "Bearer wrong"),
		withHeader("Authorization", "Bearer "+testToken+"x"),
		withHeader("Authorization", "Bearer "+testToken[:len(testToken)-1]),
		withHeader("Authorization", "Bearer "),
		withHeader("Authorization", "Basic "+testToken),
		withHeader("Authorization", testToken),
		withCookie("wrong"),
		withCookie(""),
	}
	for i, m := range bad {
		w := serve(e.h, request("GET", "/api/health", "", m))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("case %d: status = %d, want 401", i, w.Code)
		}
	}
}

func TestBearerSchemeCaseInsensitive(t *testing.T) {
	e := newEnv(t, testConfig())
	w := serve(e.h, request("GET", "/api/health", "", withHeader("Authorization", "bearer "+testToken)))
	wantStatus(t, w, http.StatusOK)
}

func TestCookieTokenAccepted(t *testing.T) {
	e := newEnv(t, testConfig())
	wantStatus(t, serve(e.h, request("GET", "/api/health", "", withCookie(testToken))), http.StatusOK)
	wantStatus(t, serve(e.h, request("POST", "/api/refresh", "", withCookie(testToken))), http.StatusAccepted)
}

func TestEmptyConfiguredTokenDeniesAll(t *testing.T) {
	cfg := testConfig()
	cfg.Token = ""
	e := newEnv(t, cfg)
	for _, m := range []func(*http.Request){
		withHeader("Authorization", "Bearer "),
		withCookie(""),
		withoutHeader("Authorization"),
	} {
		wantError(t, serve(e.h, request("GET", "/api/health", "", m)), http.StatusUnauthorized)
	}
}

func TestForeignHost403(t *testing.T) {
	cfg := testConfig()
	cfg.UI = fstest.MapFS{"index.html": {Data: []byte("<h1>Albert</h1>")}}
	e := newEnv(t, cfg)
	for _, host := range []string{"evil.com:7879", "evil.com", "localhost:9999", "localhost", "127.0.0.1", "localhost.:7879", ""} {
		t.Run(host, func(t *testing.T) {
			wantError(t, serve(e.h, request("GET", "/api/health", "", withHost(host))), http.StatusForbidden)
			// Host is checked before the token: no token, still 403.
			wantError(t, serve(e.h, request("GET", "/api/health", "", withHost(host), withoutHeader("Authorization"))), http.StatusForbidden)
			// The native UI (which hands out the token cookie) is guarded too.
			w := serve(e.h, request("GET", "/", "", withHost(host), withoutHeader("Authorization")))
			wantError(t, w, http.StatusForbidden)
			if c := w.Header().Get("Set-Cookie"); c != "" {
				t.Fatalf("foreign host got a cookie: %s", c)
			}
		})
	}
	for _, host := range []string{"localhost:7879", "127.0.0.1:7879", "LOCALHOST:7878", "127.0.0.1:7878"} {
		wantStatus(t, serve(e.h, request("GET", "/api/health", "", withHost(host))), http.StatusOK)
	}
}

func TestForeignOrigin403(t *testing.T) {
	e := newEnv(t, testConfig(), &fakeProvider{name: "brew"})
	for _, origin := range []string{"http://evil.com", "http://localhost:3000", "https://localhost:7879", "http://localhost:7878", "null", "http://localhost:7879.evil.com"} {
		wantError(t, serve(e.h, request("GET", "/api/health", "", withHeader("Origin", origin))), http.StatusForbidden)
		wantError(t, serve(e.h, request("POST", "/api/install", `{"source":"brew","name":"jq"}`, withHeader("Origin", origin))), http.StatusForbidden)
	}
	// Origin is checked before the token.
	wantError(t, serve(e.h, request("GET", "/api/health", "", withHeader("Origin", "http://evil.com"), withoutHeader("Authorization"))), http.StatusForbidden)
	// Browsers flag cross-site requests even when they omit Origin.
	for _, site := range []string{"cross-site", "same-site"} {
		wantError(t, serve(e.h, request("GET", "/api/tools", "", withHeader("Sec-Fetch-Site", site))), http.StatusForbidden)
	}
	// The UI's own origin is fine.
	for _, origin := range []string{"http://localhost:7879", "http://127.0.0.1:7879"} {
		wantStatus(t, serve(e.h, request("GET", "/api/health", "", withHeader("Origin", origin), withHeader("Sec-Fetch-Site", "same-origin"))), http.StatusOK)
	}
}

func TestPostWithoutHeader403(t *testing.T) {
	brew := &fakeProvider{name: "brew"}
	e := newEnv(t, testConfig(), brew)
	for _, m := range []func(*http.Request){withoutHeader("X-Albert-Request"), withHeader("X-Albert-Request", "0"), withHeader("X-Albert-Request", "true")} {
		wantError(t, serve(e.h, request("POST", "/api/install", `{"source":"brew","name":"jq"}`, m)), http.StatusForbidden)
		wantError(t, serve(e.h, request("POST", "/api/update", `{"all":true}`, m)), http.StatusForbidden)
		wantError(t, serve(e.h, request("POST", "/api/refresh", "", m)), http.StatusForbidden)
	}
	if n := len(e.jm.List()); n != 0 {
		t.Fatalf("%d jobs submitted without the header", n)
	}
	if n := brew.lists(); n != 0 {
		t.Fatalf("refresh ran without the header")
	}
}

func TestNoCORSHeaders(t *testing.T) {
	e := newEnv(t, testConfig(), &fakeProvider{name: "brew"})
	preflight := func(origin string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("Origin", origin)
			r.Header.Set("Access-Control-Request-Method", "POST")
			r.Header.Set("Access-Control-Request-Headers", "x-albert-request, content-type")
		}
	}
	reqs := []*http.Request{
		request("OPTIONS", "/api/update", "", preflight("http://evil.com")),
		request("OPTIONS", "/api/update", "", preflight("http://localhost:7879")),
		request("OPTIONS", "/api/update", "", preflight("http://localhost:7879"), withoutHeader("Authorization")),
		request("GET", "/api/tools", "", withHeader("Origin", "http://localhost:7879")),
		request("GET", "/api/tools", "", withHeader("Origin", "http://evil.com")),
	}
	for i, r := range reqs {
		w := serve(e.h, r)
		if w.Code == http.StatusOK && r.Method == "OPTIONS" {
			t.Errorf("case %d: OPTIONS answered 200", i)
		}
		for k := range w.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("case %d: response has CORS header %s: %s", i, k, w.Header().Get(k))
			}
		}
	}
}

func TestBodyTooLarge413(t *testing.T) {
	e := newEnv(t, testConfig(), &fakeProvider{name: "brew"})
	big := `{"source":"brew","name":"jq","pad":"` + strings.Repeat("a", 70<<10) + `"}`
	wantError(t, serve(e.h, request("POST", "/api/install", big)), http.StatusRequestEntityTooLarge)
	// Without a Content-Length the limit still applies while reading.
	r := request("POST", "/api/install", big)
	r.ContentLength = -1
	wantError(t, serve(e.h, r), http.StatusRequestEntityTooLarge)
	if n := len(e.jm.List()); n != 0 {
		t.Fatalf("%d jobs submitted", n)
	}
}

func TestNativeIndexSetsStrictCookie(t *testing.T) {
	cfg := testConfig()
	cfg.UI = fstest.MapFS{
		"index.html": {Data: []byte("<h1>Albert</h1>")},
		"styles.css": {Data: []byte("body{}")},
	}
	e := newEnv(t, cfg)
	// The browser has no token yet: "/" must not require one.
	w := serve(e.h, request("GET", "/", "", withoutHeader("Authorization"), withHost("localhost:7878")))
	wantStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), "<h1>Albert</h1>") {
		t.Fatalf("body = %q, want index.html", w.Body.String())
	}
	res := http.Response{Header: w.Header()}
	var c *http.Cookie
	for _, ck := range res.Cookies() {
		if ck.Name == "albert_token" {
			c = ck
		}
	}
	if c == nil {
		t.Fatalf("no albert_token cookie; Set-Cookie = %q", w.Header().Values("Set-Cookie"))
	}
	if c.Value != testToken || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie = %+v, want token value, HttpOnly, SameSite=Strict, Path=/", c)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}

	// Static assets are served too.
	wantStatus(t, serve(e.h, request("GET", "/styles.css", "", withoutHeader("Authorization"), withHost("localhost:7878"))), http.StatusOK)

	// The cookie the UI received authenticates API calls.
	wantStatus(t, serve(e.h, request("GET", "/api/tools", "", withCookie(c.Value), withHost("localhost:7878"))), http.StatusOK)
}

func TestDockerModeServesNoUI(t *testing.T) {
	e := newEnv(t, testConfig()) // UI nil
	w := serve(e.h, request("GET", "/", "", withoutHeader("Authorization")))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if c := w.Header().Get("Set-Cookie"); c != "" {
		t.Fatalf("docker mode set a cookie: %s", c)
	}
}

func TestUnknownSourceNotEchoedUnsafely(t *testing.T) {
	// Errors are JSON: hostile input comes back encoded, never as raw HTML.
	e := newEnv(t, testConfig(), &fakeProvider{name: "brew"})
	w := serve(e.h, request("POST", "/api/install", `{"source":"<script>","name":"jq"}`))
	wantError(t, w, http.StatusBadRequest)
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("body echoes raw markup: %s", w.Body.String())
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestRefusalLogThrottled(t *testing.T) {
	var lines []string
	old := logf
	logf = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { logf = old })

	e := newEnv(t, testConfig())
	for range 100 {
		serve(e.h, request("GET", "/api/health", "", withHost("evil.com")))
	}
	if len(lines) != 1 || !strings.Contains(lines[0], "evil.com") {
		t.Fatalf("logged %d lines %q, want 1 naming the host", len(lines), lines)
	}
	srv := e.h.(*server)
	srv.refused.mu.Lock()
	srv.refused.last = time.Now().Add(-refusedEvery) // as if the window passed
	srv.refused.mu.Unlock()
	serve(e.h, request("GET", "/api/health", "", withoutHeader("Authorization")))
	if len(lines) != 2 || !strings.Contains(lines[1], "99 more") {
		t.Fatalf("second report = %q, want it to count the 99 skipped", lines)
	}
}

var _ provider.Provider = (*fakeProvider)(nil)
