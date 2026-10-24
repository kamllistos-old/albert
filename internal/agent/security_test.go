package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// R20: cookies are sent to every localhost port, so the token cookie is no
// longer accepted, even with the right value.
func TestCookieNoLongerAccepted(t *testing.T) {
	e := newEnv(t, testConfig())
	wantError(t, serve(e.h, request("GET", "/api/health", "", withCookie(testToken))), http.StatusUnauthorized)
	wantError(t, serve(e.h, request("POST", "/api/refresh", "", withCookie(testToken))), http.StatusUnauthorized)
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
			// The native UI is guarded too.
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

func nativeConfig() Config {
	cfg := testConfig()
	cfg.UI = fstest.MapFS{
		"index.html": {Data: []byte("<h1>Albert</h1>")},
		"styles.css": {Data: []byte("body{}")},
	}
	return cfg
}

// mintCode asks for a login code the way "albert up" does.
func mintCode(t *testing.T, h http.Handler) string {
	t.Helper()
	w := serve(h, request("POST", "/api/login-code", ""))
	wantStatus(t, w, http.StatusOK)
	code := decode[map[string]string](t, w)["code"]
	if len(code) != 32 || strings.Trim(code, "0123456789abcdef") != "" {
		t.Fatalf("code = %q, want 32 lowercase hex chars", code)
	}
	return code
}

// exchange trades a code at POST /api/session the way the UI does: from its
// own origin, with the safety header, and with no token yet.
func exchange(h http.Handler, code string, mods ...func(*http.Request)) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"code": code})
	base := []func(*http.Request){
		withoutHeader("Authorization"),
		withHost("localhost:7879"),
		withHeader("Origin", "http://localhost:7879"),
		withHeader("Sec-Fetch-Site", "same-origin"),
	}
	return serve(h, request("POST", "/api/session", string(body), append(base, mods...)...))
}

func wantToken(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	wantStatus(t, w, http.StatusOK)
	tok := decode[map[string]string](t, w)["token"]
	if tok != testToken {
		t.Fatalf("token = %q, want the agent token", tok)
	}
	return tok
}

func wantExchangeRefused(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	msg := wantError(t, w, http.StatusForbidden)
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatalf("refused exchange leaked the token: %s", w.Body.String())
	}
	if want := `This Albert link has expired. Run "albert up" again to open Albert.`; msg != want {
		t.Fatalf("error = %q, want %q", msg, want)
	}
}

func TestNativeIndexSetsNoCookie(t *testing.T) {
	e := newEnv(t, nativeConfig())
	// The browser has no token: "/" serves the UI without asking for one,
	// but never hands it out.
	w := serve(e.h, request("GET", "/", "", withoutHeader("Authorization"), withHost("localhost:7878")))
	wantStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), "<h1>Albert</h1>") {
		t.Fatalf("body = %q, want index.html", w.Body.String())
	}
	if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
		t.Fatalf("GET / set cookies %q", c)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
	w = serve(e.h, request("GET", "/styles.css", "", withoutHeader("Authorization"), withHost("localhost:7878")))
	wantStatus(t, w, http.StatusOK)
	if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
		t.Fatalf("asset set cookies %q", c)
	}
}

func TestSessionExchangeReturnsToken(t *testing.T) {
	for name, cfg := range map[string]Config{"native": nativeConfig(), "docker": testConfig()} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, cfg)
			w := exchange(e.h, mintCode(t, e.h))
			tok := wantToken(t, w)
			if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", cc)
			}
			if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
				t.Fatalf("session exchange set cookies %q", c)
			}
			// The token authenticates API calls as a Bearer header.
			wantStatus(t, serve(e.h, request("GET", "/api/tools", "", withHeader("Authorization", "Bearer "+tok))), http.StatusOK)
		})
	}
}

func TestSessionCodeSingleUse(t *testing.T) {
	e := newEnv(t, nativeConfig())
	code := mintCode(t, e.h)
	wantToken(t, exchange(e.h, code))
	wantExchangeRefused(t, exchange(e.h, code))
}

func TestSessionCodeExpires(t *testing.T) {
	old := loginCodeTTL
	loginCodeTTL = 20 * time.Millisecond
	t.Cleanup(func() { loginCodeTTL = old })
	e := newEnv(t, nativeConfig())
	code := mintCode(t, e.h)
	time.Sleep(40 * time.Millisecond)
	wantExchangeRefused(t, exchange(e.h, code))
}

func TestSessionRejectsBadCodes(t *testing.T) {
	e := newEnv(t, nativeConfig())
	code := mintCode(t, e.h)
	bad := []string{"", "nope", code[:31], code + "0", strings.Repeat("0", 32)}
	if up := strings.ToUpper(code); up != code {
		bad = append(bad, up)
	}
	for _, b := range bad {
		wantExchangeRefused(t, exchange(e.h, b))
	}
	// Bodies that are not a code at all.
	for _, body := range []string{"", "{}", `{"code":1}`, `{"code":"` + code + `","x":1}`, "not json"} {
		w := serve(e.h, request("POST", "/api/session", body, withoutHeader("Authorization")))
		if w.Code == http.StatusOK {
			t.Fatalf("body %q was accepted", body)
		}
	}
	// The real code still works after all those misses.
	wantToken(t, exchange(e.h, code))
}

func TestSessionForeignHostOrOriginRefused(t *testing.T) {
	e := newEnv(t, nativeConfig())
	code := mintCode(t, e.h)
	refused := [][]func(*http.Request){
		{withHost("evil.com:7879")},
		{withHost("evil.com")},
		{withHost("")},
		{withHeader("Origin", "http://evil.com")},
		{withHeader("Origin", "http://localhost:3000")},
		{withHeader("Origin", "null")},
		{withHeader("Sec-Fetch-Site", "cross-site")},
		{withHeader("Sec-Fetch-Site", "same-site")},
	}
	for i, mods := range refused {
		w := exchange(e.h, code, mods...)
		wantError(t, w, http.StatusForbidden)
		if strings.Contains(w.Body.String(), testToken) {
			t.Fatalf("case %d leaked the token", i)
		}
	}
	// The code was not consumed by the refused attempts.
	wantToken(t, exchange(e.h, code))
}

func TestSessionNeedsSafetyHeader(t *testing.T) {
	e := newEnv(t, nativeConfig())
	code := mintCode(t, e.h)
	w := exchange(e.h, code, withoutHeader("X-Albert-Request"))
	wantError(t, w, http.StatusForbidden)
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("leaked the token")
	}
	wantToken(t, exchange(e.h, code))
}

// The token check is skipped for POST /api/session and nothing else.
func TestSessionOnlyRouteExemptFromToken(t *testing.T) {
	e := newEnv(t, nativeConfig())
	code := mintCode(t, e.h)
	for _, r := range []*http.Request{
		request("GET", "/api/session", "", withoutHeader("Authorization")),
		request("POST", "/api/session/", `{"code":"`+code+`"}`, withoutHeader("Authorization")),
		request("POST", "/api/sessions", `{"code":"`+code+`"}`, withoutHeader("Authorization")),
		request("POST", "/api/login-code", "", withoutHeader("Authorization")),
		request("GET", "/api/tools", "", withoutHeader("Authorization")),
	} {
		if w := serve(e.h, r); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token: status %d, want 401", r.Method, r.URL.Path, w.Code)
		}
	}
	wantToken(t, exchange(e.h, code))
}

func TestLoginRouteRemoved(t *testing.T) {
	e := newEnv(t, nativeConfig())
	code := mintCode(t, e.h)
	w := serve(e.h, request("GET", "/login?code="+code, "", withoutHeader("Authorization")))
	if w.Code == http.StatusSeeOther || len(w.Header().Values("Set-Cookie")) != 0 {
		t.Fatalf("/login still logs in: status %d, cookies %q", w.Code, w.Header().Values("Set-Cookie"))
	}
	// It did not use up the code either.
	wantToken(t, exchange(e.h, code))
}

func TestLoginCodeEndpointGuarded(t *testing.T) {
	e := newEnv(t, nativeConfig())
	wantError(t, serve(e.h, request("POST", "/api/login-code", "", withoutHeader("Authorization"))), http.StatusUnauthorized)
	wantError(t, serve(e.h, request("POST", "/api/login-code", "", withHeader("Authorization", "Bearer wrong"))), http.StatusUnauthorized)
	wantError(t, serve(e.h, request("POST", "/api/login-code", "", withoutHeader("X-Albert-Request"))), http.StatusForbidden)
	wantError(t, serve(e.h, request("POST", "/api/login-code", "", withHeader("Origin", "http://evil.com"))), http.StatusForbidden)
	wantError(t, serve(e.h, request("POST", "/api/login-code", "", withHost("evil.com:7879"))), http.StatusForbidden)
	if n := e.h.(*server).codes.outstanding(); n != 0 {
		t.Fatalf("%d codes minted by refused requests", n)
	}
}

func TestLoginCodesCappedOldestEvicted(t *testing.T) {
	e := newEnv(t, nativeConfig())
	var codes []string
	for range maxLoginCodes + 1 {
		codes = append(codes, mintCode(t, e.h))
	}
	if n := e.h.(*server).codes.outstanding(); n != maxLoginCodes {
		t.Fatalf("%d codes outstanding, want %d", n, maxLoginCodes)
	}
	wantExchangeRefused(t, exchange(e.h, codes[0])) // evicted
	for _, c := range codes[1:] {
		wantToken(t, exchange(e.h, c))
	}
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
