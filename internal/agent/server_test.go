package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kamllistos/albert/internal/inventory"
	"github.com/kamllistos/albert/internal/jobs"
	"github.com/kamllistos/albert/internal/provider"
)

func TestMain(m *testing.M) {
	logf = func(string, ...any) {} // keep test output clean
	os.Exit(m.Run())
}

const (
	testToken = "0123456789abcdef0123456789abcdef"
	testHost  = "localhost:7879"
)

// fakeProvider is a provider.Provider with canned data that records what it
// was asked to do.
type fakeProvider struct {
	name        string
	unavailable bool
	tools       []provider.Tool
	updates     []provider.Update
	results     []provider.Result
	searchErr   error

	mu        sync.Mutex
	listCalls int
	upgrades  [][]string
	installs  []string
}

func (f *fakeProvider) Name() string    { return f.name }
func (f *fakeProvider) Available() bool { return !f.unavailable }
func (f *fakeProvider) List(context.Context) ([]provider.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return append([]provider.Tool(nil), f.tools...), nil
}
func (f *fakeProvider) Outdated(context.Context) ([]provider.Update, error) {
	return append([]provider.Update(nil), f.updates...), nil
}
func (f *fakeProvider) Upgrade(_ context.Context, names []string, out io.Writer) error {
	f.mu.Lock()
	f.upgrades = append(f.upgrades, names)
	f.mu.Unlock()
	io.WriteString(out, "upgrading\n")
	return nil
}
func (f *fakeProvider) Install(_ context.Context, name string, out io.Writer) error {
	f.mu.Lock()
	f.installs = append(f.installs, name)
	f.mu.Unlock()
	io.WriteString(out, "installing "+name+"\n")
	return nil
}
func (f *fakeProvider) Search(context.Context, string) ([]provider.Result, error) {
	return f.results, f.searchErr
}
func (f *fakeProvider) WatchPaths() []string { return nil }

func (f *fakeProvider) lists() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeProvider) installed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.installs...)
}

func testConfig() Config {
	return Config{
		Token:        testToken,
		AllowedHosts: []string{"localhost:7879", "127.0.0.1:7879", "localhost:7878", "127.0.0.1:7878"},
		UIOrigins:    []string{"http://localhost:7879", "http://127.0.0.1:7879"},
		Version:      "1.2.3",
	}
}

// env is a server over fake providers.
type env struct {
	h   http.Handler
	inv *inventory.Inventory
	jm  *jobs.Manager
}

func newEnv(t *testing.T, cfg Config, ps ...provider.Provider) *env {
	t.Helper()
	inv := inventory.New(ps, "")
	jm := jobs.NewManager(inv)
	t.Cleanup(jm.Close)
	return &env{h: NewServer(cfg, inv, jm), inv: inv, jm: jm}
}

// scanned is newEnv plus a full scan and outdated check.
func scanned(t *testing.T, ps ...provider.Provider) *env {
	t.Helper()
	e := newEnv(t, testConfig(), ps...)
	e.inv.ScanAll(context.Background())
	e.inv.CheckOutdated(context.Background(), "")
	return e
}

// request builds a request that passes every security check: allowed Host,
// Bearer token and, for POST, the X-Albert-Request header. mods run last.
func request(method, path, body string, mods ...func(*http.Request)) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = testHost
	r.Header.Set("Authorization", "Bearer "+testToken)
	if method == http.MethodPost {
		r.Header.Set("X-Albert-Request", "1")
		r.Header.Set("Content-Type", "application/json")
	}
	for _, m := range mods {
		m(r)
	}
	return r
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, code int) {
	t.Helper()
	if w.Code != code {
		t.Fatalf("status = %d, want %d; body %s", w.Code, code, w.Body.String())
	}
}

// wantError checks a JSON error response.
func wantError(t *testing.T, w *httptest.ResponseRecorder, code int) string {
	t.Helper()
	wantStatus(t, w, code)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body := decode[map[string]string](t, w)
	if body["error"] == "" {
		t.Fatalf("body %s has no error message", w.Body.String())
	}
	return body["error"]
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// updateFixture: brew and pip have a writable outdated tool, gem only a
// read-only one, npm nothing outdated.
func updateFixture() []provider.Provider {
	return []provider.Provider{
		&fakeProvider{name: "brew",
			tools:   []provider.Tool{{Name: "wget", Version: "1.0"}},
			updates: []provider.Update{{Name: "wget", Latest: "1.1"}}},
		&fakeProvider{name: "pip",
			tools:   []provider.Tool{{Name: "black", Version: "23.0"}},
			updates: []provider.Update{{Name: "black", Latest: "24.0"}}},
		&fakeProvider{name: "gem",
			tools:   []provider.Tool{{Name: "rails", Version: "7.0", ReadOnly: true}},
			updates: []provider.Update{{Name: "rails", Latest: "7.1"}}},
		&fakeProvider{name: "npm",
			tools: []provider.Tool{{Name: "zx", Version: "8.0"}}},
	}
}

func jobSources(t *testing.T, e *env, w *httptest.ResponseRecorder) []string {
	t.Helper()
	wantStatus(t, w, http.StatusOK)
	resp := decode[struct {
		Jobs []string `json:"jobs"`
	}](t, w)
	var srcs []string
	for _, id := range resp.Jobs {
		j, ok := e.jm.Get(id)
		if !ok {
			t.Fatalf("job %s not found", id)
		}
		srcs = append(srcs, j.Source)
	}
	slices.Sort(srcs)
	return srcs
}

func TestHealth(t *testing.T) {
	e := newEnv(t, testConfig())
	w := serve(e.h, request("GET", "/api/health", ""))
	wantStatus(t, w, http.StatusOK)
	got := decode[map[string]any](t, w)
	if got["ok"] != true || got["version"] != "1.2.3" {
		t.Fatalf("health = %v, want ok true and version 1.2.3", got)
	}
}

func TestTools(t *testing.T) {
	e := scanned(t, &fakeProvider{name: "brew",
		tools:   []provider.Tool{{Name: "jq", Version: "1.6"}},
		updates: []provider.Update{{Name: "jq", Latest: "1.7"}}})
	w := serve(e.h, request("GET", "/api/tools", ""))
	wantStatus(t, w, http.StatusOK)

	raw := decode[map[string]json.RawMessage](t, w)
	for _, k := range []string{"tools", "providers", "checkingUpdates"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("response has no %q key: %s", k, w.Body.String())
		}
	}
	snap := decode[inventory.Snapshot](t, w)
	if len(snap.Tools) != 1 || snap.Tools[0].Name != "jq" || !snap.Tools[0].Outdated || snap.Tools[0].Latest != "1.7" {
		t.Fatalf("tools = %+v, want jq outdated to 1.7", snap.Tools)
	}
}

func TestUpdateAllSkipsPipAndReadOnly(t *testing.T) {
	e := scanned(t, updateFixture()...)
	w := serve(e.h, request("POST", "/api/update", `{"all":true}`))
	if got, want := jobSources(t, e, w), []string{"brew"}; !slices.Equal(got, want) {
		t.Fatalf("jobs for %v, want %v", got, want)
	}
}

func TestUpdateAllIncludePip(t *testing.T) {
	e := scanned(t, updateFixture()...)
	w := serve(e.h, request("POST", "/api/update", `{"all":true,"includePip":true}`))
	if got, want := jobSources(t, e, w), []string{"brew", "pip"}; !slices.Equal(got, want) {
		t.Fatalf("jobs for %v, want %v", got, want)
	}
}

func TestUpdateAllNothingToDo(t *testing.T) {
	e := scanned(t, &fakeProvider{name: "npm", tools: []provider.Tool{{Name: "zx", Version: "8.0"}}})
	w := serve(e.h, request("POST", "/api/update", `{"all":true}`))
	wantStatus(t, w, http.StatusOK)
	if got := strings.TrimSpace(w.Body.String()); got != `{"jobs":[]}` {
		t.Fatalf("body = %s, want {\"jobs\":[]}", got)
	}
}

func TestUpdateNamed(t *testing.T) {
	e := scanned(t, updateFixture()...)
	w := serve(e.h, request("POST", "/api/update", `{"source":"brew","names":["wget"]}`))
	if got, want := jobSources(t, e, w), []string{"brew"}; !slices.Equal(got, want) {
		t.Fatalf("jobs for %v, want %v", got, want)
	}
	j := e.jm.List()[0]
	if j.Kind != jobs.KindUpdate || !slices.Equal(j.Targets, []string{"wget"}) {
		t.Fatalf("job = %+v, want update of [wget]", j)
	}
}

func TestUpdateBadRequests(t *testing.T) {
	cases := map[string]string{
		"bad json":       `{"source":`,
		"not an object":  `[1,2]`,
		"unknown field":  `{"source":"brew","names":["wget"],"force":true}`,
		"trailing data":  `{"source":"brew","names":["wget"]} {}`,
		"bad name":       `{"source":"brew","names":["--force"]}`,
		"bad second":     `{"source":"brew","names":["wget","x;rm"]}`,
		"unknown source": `{"source":"apt","names":["wget"]}`,
		"no names":       `{"source":"brew"}`,
		"no source":      `{"names":["wget"]}`,
		"all and names":  `{"all":true,"source":"brew","names":["wget"]}`,
		"empty":          ``,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			e := scanned(t, updateFixture()...)
			wantError(t, serve(e.h, request("POST", "/api/update", body)), http.StatusBadRequest)
			if n := len(e.jm.List()); n != 0 {
				t.Fatalf("%d jobs submitted for a bad request", n)
			}
		})
	}
}

func TestInstall(t *testing.T) {
	brew := &fakeProvider{name: "brew"}
	e := scanned(t, brew)
	w := serve(e.h, request("POST", "/api/install", `{"source":"brew","name":"jq"}`))
	if got := jobSources(t, e, w); !slices.Equal(got, []string{"brew"}) {
		t.Fatalf("jobs for %v, want [brew]", got)
	}
	eventually(t, "install to run", func() bool { return slices.Equal(brew.installed(), []string{"jq"}) })
}

func TestInstallRejectsBadName(t *testing.T) {
	for _, name := range []string{"--force", "-rf", "a b", "x;rm", ""} {
		t.Run(name, func(t *testing.T) {
			brew := &fakeProvider{name: "brew"}
			e := scanned(t, brew)
			body, _ := json.Marshal(map[string]string{"source": "brew", "name": name})
			wantError(t, serve(e.h, request("POST", "/api/install", string(body))), http.StatusBadRequest)
			if n := len(e.jm.List()); n != 0 {
				t.Fatalf("%d jobs submitted for name %q", n, name)
			}
			if got := brew.installed(); len(got) != 0 {
				t.Fatalf("provider asked to install %v", got)
			}
		})
	}
}

func TestInstallUnknownSource400(t *testing.T) {
	e := scanned(t, &fakeProvider{name: "brew"})
	msg := wantError(t, serve(e.h, request("POST", "/api/install", `{"source":"apt","name":"jq"}`)), http.StatusBadRequest)
	if !strings.Contains(msg, "apt") {
		t.Errorf("error %q does not name the source", msg)
	}
	if n := len(e.jm.List()); n != 0 {
		t.Fatalf("%d jobs submitted", n)
	}
}

func TestSearchMergesAndSkipsUnsupported(t *testing.T) {
	e := scanned(t,
		&fakeProvider{name: "brew", results: []provider.Result{{Name: "jq", Source: "brew"}}},
		&fakeProvider{name: "cargo", searchErr: provider.ErrUnsupported},
		&fakeProvider{name: "gem", unavailable: true, results: []provider.Result{{Name: "jq-gem", Source: "gem"}}},
		&fakeProvider{name: "npm", results: []provider.Result{{Name: "node-jq", Source: "npm"}, {Name: "jq-web", Source: "npm"}}},
	)
	w := serve(e.h, request("GET", "/api/search?q=jq", ""))
	wantStatus(t, w, http.StatusOK)
	resp := decode[struct {
		Results []provider.Result `json:"results"`
	}](t, w)
	var got []string
	for _, r := range resp.Results {
		got = append(got, r.Source+"/"+r.Name)
	}
	want := []string{"brew/jq", "npm/node-jq", "npm/jq-web"}
	if !slices.Equal(got, want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
}

func TestSearchEmptyIsArray(t *testing.T) {
	e := scanned(t, &fakeProvider{name: "brew"})
	w := serve(e.h, request("GET", "/api/search?q=zzz", ""))
	wantStatus(t, w, http.StatusOK)
	if got := strings.TrimSpace(w.Body.String()); got != `{"results":[]}` {
		t.Fatalf("body = %s, want {\"results\":[]}", got)
	}
}

func TestSearchQueryTooShortOrLong(t *testing.T) {
	e := scanned(t, &fakeProvider{name: "brew"})
	for _, q := range []string{"", "j", "%20j%20", strings.Repeat("a", 101)} {
		wantError(t, serve(e.h, request("GET", "/api/search?q="+q, "")), http.StatusBadRequest)
	}
}

func TestJobsEndpoints(t *testing.T) {
	brew := &fakeProvider{name: "brew"}
	e := scanned(t, brew)
	id, err := e.jm.Submit("brew", jobs.KindInstall, []string{"jq"})
	if err != nil {
		t.Fatal(err)
	}

	w := serve(e.h, request("GET", "/api/jobs", ""))
	wantStatus(t, w, http.StatusOK)
	list := decode[struct {
		Jobs []jobs.Job `json:"jobs"`
	}](t, w)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != id {
		t.Fatalf("jobs = %+v, want [%s]", list.Jobs, id)
	}

	w = serve(e.h, request("GET", "/api/jobs/"+id, ""))
	wantStatus(t, w, http.StatusOK)
	if j := decode[jobs.Job](t, w); j.ID != id || j.Kind != jobs.KindInstall {
		t.Fatalf("job = %+v, want install %s", j, id)
	}

	wantError(t, serve(e.h, request("GET", "/api/jobs/nope", "")), http.StatusNotFound)
}

func TestRefresh(t *testing.T) {
	brew := &fakeProvider{name: "brew"}
	npm := &fakeProvider{name: "npm"}
	e := newEnv(t, testConfig(), brew, npm)

	wantStatus(t, serve(e.h, request("POST", "/api/refresh?source=brew", "")), http.StatusAccepted)
	eventually(t, "brew rescan", func() bool { return brew.lists() == 1 })
	if npm.lists() != 0 {
		t.Fatalf("refresh of brew listed npm too")
	}

	wantStatus(t, serve(e.h, request("POST", "/api/refresh", "")), http.StatusAccepted)
	eventually(t, "full rescan", func() bool { return brew.lists() == 2 && npm.lists() == 1 })

	wantError(t, serve(e.h, request("POST", "/api/refresh?source=apt", "")), http.StatusBadRequest)
}

func TestCoalescerRerunsOnceWhileBusy(t *testing.T) {
	var c coalescer
	var mu sync.Mutex
	runs := 0
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	fn := func() {
		started <- struct{}{}
		<-release
		mu.Lock()
		runs++
		mu.Unlock()
	}
	c.run("brew", fn)
	<-started
	c.run("brew", fn) // busy: queued once
	c.run("brew", fn) // busy: merged with the queued run
	close(release)
	c.wait()
	if runs != 2 {
		t.Fatalf("fn ran %d times, want 2 (the running call and one rerun)", runs)
	}
}

// sseClient opens /api/events on a real server and returns a line reader.
func sseClient(t *testing.T, h http.Handler) (*bufio.Reader, func()) {
	t.Helper()
	ts := httptest.NewServer(h)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	req.Host = testHost
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}
	stop := func() {
		cancel()
		resp.Body.Close()
		ts.Close()
	}
	t.Cleanup(stop)
	return bufio.NewReader(resp.Body), stop
}

type sseEvent struct{ name, data string }

// readEvent reads the next event (or comment, as name ":") within d.
func readEvent(t *testing.T, rd *bufio.Reader, d time.Duration) sseEvent {
	t.Helper()
	ch := make(chan sseEvent, 1)
	errc := make(chan error, 1)
	go func() {
		var ev sseEvent
		for {
			line, err := rd.ReadString('\n')
			if err != nil {
				errc <- err
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case line == "":
				if ev.name != "" {
					ch <- ev
					return
				}
			case strings.HasPrefix(line, ":"):
				ch <- sseEvent{name: ":", data: strings.TrimSpace(line[1:])}
				return
			case strings.HasPrefix(line, "event: "):
				ev.name = line[len("event: "):]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[len("data: "):]
			}
		}
	}()
	select {
	case ev := <-ch:
		return ev
	case err := <-errc:
		t.Fatalf("reading events: %v", err)
	case <-time.After(d):
		t.Fatalf("no event within %v", d)
	}
	return sseEvent{}
}

// readUntil skips events until one named name arrives.
func readUntil(t *testing.T, rd *bufio.Reader, name string) sseEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ev := readEvent(t, rd, time.Until(deadline)); ev.name == name {
			return ev
		}
	}
	t.Fatalf("no %q event within 2s", name)
	return sseEvent{}
}

func TestEventsStreamsInventory(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "jq", Version: "1.6"}}}
	e := scanned(t, brew)
	rd, _ := sseClient(t, e.h)

	ev := readEvent(t, rd, 2*time.Second)
	if ev.name != "inventory" {
		t.Fatalf("first event = %q, want inventory", ev.name)
	}
	var snap inventory.Snapshot
	if err := json.Unmarshal([]byte(ev.data), &snap); err != nil {
		t.Fatalf("inventory data %q: %v", ev.data, err)
	}
	if len(snap.Tools) != 1 || snap.Tools[0].Name != "jq" {
		t.Fatalf("snapshot tools = %+v, want jq", snap.Tools)
	}

	// A later change is pushed too.
	brew.mu.Lock()
	brew.tools = append(brew.tools, provider.Tool{Name: "wget", Version: "1.0"})
	brew.mu.Unlock()
	if err := e.inv.Scan(context.Background(), "brew"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ev := readUntil(t, rd, "inventory")
		if err := json.Unmarshal([]byte(ev.data), &snap); err != nil {
			t.Fatal(err)
		}
		if len(snap.Tools) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no inventory event with wget")
		}
	}
}

func TestEventsStreamsJobs(t *testing.T) {
	e := scanned(t, &fakeProvider{name: "brew"})
	rd, _ := sseClient(t, e.h)
	readUntil(t, rd, "inventory")

	id, err := e.jm.Submit("brew", jobs.KindInstall, []string{"jq"})
	if err != nil {
		t.Fatal(err)
	}
	ev := readUntil(t, rd, "job")
	var line struct{ ID, Line string }
	if err := json.Unmarshal([]byte(ev.data), &line); err != nil {
		t.Fatal(err)
	}
	if line.ID != id || line.Line != "installing jq" {
		t.Fatalf("job event = %+v, want {%s installing jq}", line, id)
	}
	if !strings.Contains(ev.data, `"id":`) || !strings.Contains(ev.data, `"line":`) {
		t.Fatalf("job event data %s, want lowerCamel id and line keys", ev.data)
	}

	ev = readUntil(t, rd, "jobdone")
	var j jobs.Job
	if err := json.Unmarshal([]byte(ev.data), &j); err != nil {
		t.Fatal(err)
	}
	if j.ID != id || j.Status != jobs.StatusSuccess {
		t.Fatalf("jobdone = %+v, want %s success", j, id)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 20 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = old })

	e := scanned(t, &fakeProvider{name: "brew"})
	rd, _ := sseClient(t, e.h)
	ev := readUntil(t, rd, ":")
	if ev.data != "ping" {
		t.Fatalf("comment = %q, want ping", ev.data)
	}
}

func TestSlowEventClientDoesNotBlockJobs(t *testing.T) {
	e := scanned(t, &fakeProvider{name: "brew"})
	rd, _ := sseClient(t, e.h)
	readUntil(t, rd, "inventory")
	// The client stops reading. Jobs and inventory changes must still go
	// through: publishers never wait for a subscriber.
	for range 300 {
		if _, err := e.jm.Submit("brew", jobs.KindInstall, []string{"jq"}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "jobs to finish", func() bool {
		for _, j := range e.jm.List() {
			if j.Status != jobs.StatusSuccess {
				return false
			}
		}
		return true
	})
}
