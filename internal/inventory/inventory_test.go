package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kamllistos/albert/internal/provider"
)

// fakeProvider is a configurable provider.Provider for tests.
type fakeProvider struct {
	name        string
	unavailable bool
	delay       time.Duration // List and Outdated sleep this long, ignoring ctx

	mu        sync.Mutex
	tools     []provider.Tool
	updates   []provider.Update
	listErr   error
	outErr    error
	listCalls int
	outCalls  int
}

func (f *fakeProvider) Name() string    { return f.name }
func (f *fakeProvider) Available() bool { return !f.unavailable }

func (f *fakeProvider) List(ctx context.Context) ([]provider.Tool, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]provider.Tool(nil), f.tools...), nil
}

func (f *fakeProvider) Outdated(ctx context.Context) ([]provider.Update, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outCalls++
	var le *provider.LookupError
	if f.outErr != nil && !errors.As(f.outErr, &le) {
		return nil, f.outErr
	}
	// Like the registry providers: failed lookups come back with the
	// updates that were found.
	return append([]provider.Update(nil), f.updates...), f.outErr
}

func (f *fakeProvider) setOutdated(ups []provider.Update, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates, f.outErr = ups, err
}

func (f *fakeProvider) Upgrade(context.Context, []string, io.Writer) error { return nil }
func (f *fakeProvider) Install(context.Context, string, io.Writer) error   { return nil }
func (f *fakeProvider) Search(context.Context, string) ([]provider.Result, error) {
	return nil, nil
}
func (f *fakeProvider) WatchPaths() []string { return nil }

func (f *fakeProvider) setTools(ts ...provider.Tool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tools = ts
}

func (f *fakeProvider) calls() (list, out int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.outCalls
}

func tool(name, version string) provider.Tool {
	return provider.Tool{Name: name, Version: version}
}

func findTool(t *testing.T, s Snapshot, source, name string) provider.Tool {
	t.Helper()
	for _, tl := range s.Tools {
		if tl.Source == source && tl.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %s/%s not in snapshot %+v", source, name, s.Tools)
	return provider.Tool{}
}

func findStatus(t *testing.T, s Snapshot, name string) ProviderStatus {
	t.Helper()
	for _, st := range s.Providers {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("provider %s not in snapshot %+v", name, s.Providers)
	return ProviderStatus{}
}

func setTimeout(t *testing.T, v *time.Duration, d time.Duration) {
	t.Helper()
	old := *v
	*v = d
	t.Cleanup(func() { *v = old })
}

func TestScanAllParallelAndSorted(t *testing.T) {
	a := &fakeProvider{name: "npm", delay: 200 * time.Millisecond}
	a.setTools(tool("zx", "1"), tool("ajv", "2"))
	b := &fakeProvider{name: "brew", delay: 200 * time.Millisecond}
	b.setTools(tool("wget", "1"), tool("bat", "1"))
	inv := New([]provider.Provider{a, b}, "")

	start := time.Now()
	inv.ScanAll(context.Background())
	if d := time.Since(start); d > 350*time.Millisecond {
		t.Fatalf("ScanAll took %v; providers were not listed in parallel", d)
	}

	s := inv.Snapshot()
	var got []string
	for _, tl := range s.Tools {
		got = append(got, tl.Source+"/"+tl.Name)
	}
	want := []string{"brew/bat", "brew/wget", "npm/ajv", "npm/zx"}
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tools = %v, want %v", got, want)
		}
	}
	if len(s.Providers) != 2 || s.Providers[0].Name != "npm" || s.Providers[1].Name != "brew" {
		t.Fatalf("providers not in input order: %+v", s.Providers)
	}
	for _, st := range s.Providers {
		if !st.Available || st.Error != "" || st.LastScan.IsZero() {
			t.Fatalf("bad status %+v", st)
		}
	}
}

func TestSourceSetDefensively(t *testing.T) {
	p := &fakeProvider{name: "pipx"}
	p.setTools(provider.Tool{Name: "black", Version: "1", Source: "wrong"})
	inv := New([]provider.Provider{p}, "")
	inv.ScanAll(context.Background())
	if got := inv.Snapshot().Tools[0].Source; got != "pipx" {
		t.Fatalf("Source = %q, want pipx", got)
	}
}

func TestFailingProviderIsolated(t *testing.T) {
	bad := &fakeProvider{name: "bad", listErr: errors.New("boom")}
	good := &fakeProvider{name: "good"}
	good.setTools(tool("jq", "1.7"))
	inv := New([]provider.Provider{bad, good}, "")
	inv.ScanAll(context.Background())

	s := inv.Snapshot()
	if st := findStatus(t, s, "bad"); st.Error == "" {
		t.Fatalf("bad provider has no error: %+v", st)
	}
	if st := findStatus(t, s, "good"); st.Error != "" {
		t.Fatalf("good provider has error: %+v", st)
	}
	if len(s.Tools) != 1 || s.Tools[0].Name != "jq" {
		t.Fatalf("tools = %+v, want only jq", s.Tools)
	}
	if err := inv.Scan(context.Background(), "bad"); err == nil {
		t.Fatal("Scan of failing provider returned nil error")
	}
}

func TestFailedRescanClearsTools(t *testing.T) {
	p := &fakeProvider{name: "brew"}
	p.setTools(tool("jq", "1.7"))
	inv := New([]provider.Provider{p}, "")
	inv.ScanAll(context.Background())
	p.mu.Lock()
	p.listErr = errors.New("boom")
	p.mu.Unlock()
	if err := inv.Scan(context.Background(), "brew"); err == nil {
		t.Fatal("want error")
	}
	if s := inv.Snapshot(); len(s.Tools) != 0 || findStatus(t, s, "brew").Error == "" {
		t.Fatalf("snapshot after failed rescan = %+v", s)
	}
}

func TestListTimeoutIsolated(t *testing.T) {
	setTimeout(t, &ListTimeout, 50*time.Millisecond)
	slow := &fakeProvider{name: "slow", delay: time.Second}
	slow.setTools(tool("x", "1"))
	fast := &fakeProvider{name: "fast"}
	fast.setTools(tool("y", "1"))
	inv := New([]provider.Provider{slow, fast}, "")

	start := time.Now()
	inv.ScanAll(context.Background())
	if d := time.Since(start); d >= 500*time.Millisecond {
		t.Fatalf("ScanAll took %v, want < 500ms", d)
	}
	s := inv.Snapshot()
	if st := findStatus(t, s, "slow"); st.Error == "" {
		t.Fatalf("slow provider has no error: %+v", st)
	}
	if len(s.Tools) != 1 || s.Tools[0].Name != "y" {
		t.Fatalf("tools = %+v, want only y", s.Tools)
	}
}

func TestOutdatedTimeoutIsolated(t *testing.T) {
	slow := &fakeProvider{name: "slow"}
	slow.setTools(tool("x", "1"))
	fast := &fakeProvider{name: "fast", updates: []provider.Update{{Name: "y", Latest: "2"}}}
	fast.setTools(tool("y", "1"))
	inv := New([]provider.Provider{slow, fast}, "")
	inv.ScanAll(context.Background())

	setTimeout(t, &OutdatedTimeout, 50*time.Millisecond)
	slow.delay = time.Second
	start := time.Now()
	inv.CheckOutdated(context.Background(), "")
	if d := time.Since(start); d >= 500*time.Millisecond {
		t.Fatalf("CheckOutdated took %v, want < 500ms", d)
	}
	s := inv.Snapshot()
	if st := findStatus(t, s, "slow"); st.Error == "" || st.Checking {
		t.Fatalf("slow status = %+v", st)
	}
	if !findTool(t, s, "fast", "y").Outdated {
		t.Fatal("fast/y not outdated")
	}
	if len(s.Tools) != 2 {
		t.Fatalf("outdated failure removed tools: %+v", s.Tools)
	}
	if s.CheckingUpdates {
		t.Fatal("CheckingUpdates still true")
	}
}

func TestFirstScanMarksNothingNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	p := &fakeProvider{name: "brew"}
	p.setTools(tool("jq", "1.7"), tool("wget", "1.24"))
	inv := New([]provider.Provider{p}, path)
	inv.ScanAll(context.Background())
	for _, tl := range inv.Snapshot().Tools {
		if tl.New {
			t.Fatalf("%s marked New on first run", tl.Name)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
}

func TestCorruptCacheTreatedAsFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{name: "brew"}
	p.setTools(tool("jq", "1.7"))
	inv := New([]provider.Provider{p}, path)
	inv.ScanAll(context.Background())
	if findTool(t, inv.Snapshot(), "brew", "jq").New {
		t.Fatal("tool marked New after corrupt cache")
	}
}

func TestLaterToolMarkedNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	p := &fakeProvider{name: "brew"}
	p.setTools(tool("jq", "1.7"))
	inv := New([]provider.Provider{p}, path)
	inv.ScanAll(context.Background())

	p.setTools(tool("jq", "1.7"), tool("ripgrep", "14"))
	if err := inv.Scan(context.Background(), "brew"); err != nil {
		t.Fatal(err)
	}
	s := inv.Snapshot()
	if findTool(t, s, "brew", "jq").New {
		t.Fatal("jq marked New")
	}
	if !findTool(t, s, "brew", "ripgrep").New {
		t.Fatal("ripgrep not marked New")
	}

	inv2 := New([]provider.Provider{p}, path)
	inv2.ScanAll(context.Background())
	s = inv2.Snapshot()
	if findTool(t, s, "brew", "jq").New {
		t.Fatal("jq marked New after reload")
	}
	if !findTool(t, s, "brew", "ripgrep").New {
		t.Fatal("ripgrep not New after reload")
	}
}

func TestOldFirstSeenNotNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	old := time.Now().Add(-25 * time.Hour).UTC().Format(time.RFC3339)
	data := `{"firstSeen": {"brew/jq": "` + old + `"}, "latest": {}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{name: "brew"}
	p.setTools(tool("jq", "1.7"))
	inv := New([]provider.Provider{p}, path)
	inv.ScanAll(context.Background())
	if findTool(t, inv.Snapshot(), "brew", "jq").New {
		t.Fatal("tool first seen 25h ago marked New")
	}
}

func TestRescanKeepsLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	p := &fakeProvider{name: "brew", updates: []provider.Update{{Name: "jq", Latest: "1.8"}}}
	p.setTools(tool("jq", "1.7"), tool("wget", "1.24"))
	inv := New([]provider.Provider{p}, path)
	inv.ScanAll(context.Background())
	inv.CheckOutdated(context.Background(), "brew")

	if err := inv.Scan(context.Background(), "brew"); err != nil {
		t.Fatal(err)
	}
	s := inv.Snapshot()
	jq := findTool(t, s, "brew", "jq")
	if !jq.Outdated || jq.Latest != "1.8" {
		t.Fatalf("jq after rescan = %+v", jq)
	}
	if findTool(t, s, "brew", "wget").Outdated {
		t.Fatal("wget outdated")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		FirstSeen map[string]string            `json:"firstSeen"`
		Latest    map[string]map[string]string `json:"latest"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("cache is not valid JSON: %v\n%s", err, raw)
	}
	if c.Latest["brew"]["jq"] != "1.8" {
		t.Fatalf("cache latest = %v", c.Latest)
	}
	if ts, err := time.Parse(time.RFC3339, c.FirstSeen["brew/jq"]); err != nil || !ts.IsZero() {
		t.Fatalf("cache firstSeen[brew/jq] = %q (%v), want zero RFC3339 time", c.FirstSeen["brew/jq"], err)
	}

	// The cache restores the badge in a fresh inventory without CheckOutdated.
	inv2 := New([]provider.Provider{p}, path)
	inv2.ScanAll(context.Background())
	if jq := findTool(t, inv2.Snapshot(), "brew", "jq"); !jq.Outdated || jq.Latest != "1.8" {
		t.Fatalf("jq after reload = %+v", jq)
	}

	// After an upgrade the version matches Latest, so it is no longer outdated.
	p.setTools(tool("jq", "1.8"), tool("wget", "1.24"))
	if err := inv.Scan(context.Background(), "brew"); err != nil {
		t.Fatal(err)
	}
	if findTool(t, inv.Snapshot(), "brew", "jq").Outdated {
		t.Fatal("jq outdated after upgrade")
	}
}

func TestRegistryOutageKeepsPreviousLatest(t *testing.T) {
	p := &fakeProvider{name: "npm", updates: []provider.Update{{Name: "eslint", Latest: "9.1"}}}
	p.setTools(tool("eslint", "9.0"), tool("tsc", "5.0"))
	inv := New([]provider.Provider{p}, "")
	inv.ScanAll(context.Background())
	inv.CheckOutdated(context.Background(), "npm")

	// Offline: every lookup fails.
	p.setOutdated(nil, &provider.LookupError{Source: "npm", Failed: []string{"eslint", "tsc"}, Total: 2})
	inv.CheckOutdated(context.Background(), "npm")
	s := inv.Snapshot()
	if eslint := findTool(t, s, "npm", "eslint"); !eslint.Outdated || eslint.Latest != "9.1" {
		t.Fatalf("eslint = %+v, want the earlier update kept", eslint)
	}
	if st := findStatus(t, s, "npm"); st.Error == "" {
		t.Fatal("provider status should report that updates couldn't be checked")
	}

	// Back online: the error clears.
	p.setOutdated([]provider.Update{{Name: "eslint", Latest: "9.1"}}, nil)
	inv.CheckOutdated(context.Background(), "npm")
	if st := findStatus(t, inv.Snapshot(), "npm"); st.Error != "" {
		t.Fatalf("error after a good check = %q", st.Error)
	}
}

func TestPartialLookupFailureMergesLatest(t *testing.T) {
	p := &fakeProvider{name: "npm", updates: []provider.Update{
		{Name: "eslint", Latest: "9.1"},
		{Name: "prettier", Latest: "3.1"},
	}}
	p.setTools(tool("eslint", "9.0"), tool("prettier", "3.0"), tool("tsc", "5.0"))
	inv := New([]provider.Provider{p}, "")
	inv.ScanAll(context.Background())
	inv.CheckOutdated(context.Background(), "npm")

	// prettier's lookup fails; eslint is now current; tsc has a new update.
	p.setTools(tool("eslint", "9.1"), tool("prettier", "3.0"), tool("tsc", "5.0"))
	p.setOutdated([]provider.Update{{Name: "tsc", Latest: "5.1"}},
		&provider.LookupError{Source: "npm", Failed: []string{"prettier"}, Total: 3})
	inv.Scan(context.Background(), "npm")
	inv.CheckOutdated(context.Background(), "npm")
	s := inv.Snapshot()
	if pr := findTool(t, s, "npm", "prettier"); !pr.Outdated || pr.Latest != "3.1" {
		t.Errorf("prettier = %+v, want its earlier latest kept", pr)
	}
	if tsc := findTool(t, s, "npm", "tsc"); !tsc.Outdated || tsc.Latest != "5.1" {
		t.Errorf("tsc = %+v, want the fresh update", tsc)
	}
	if es := findTool(t, s, "npm", "eslint"); es.Outdated {
		t.Errorf("eslint = %+v, want up to date", es)
	}
	if st := findStatus(t, s, "npm"); st.Error == "" {
		t.Error("provider status should say some tools couldn't be checked")
	}
}

func TestOutdatedIgnoresUnlistedAndKeepsReadOnly(t *testing.T) {
	p := &fakeProvider{name: "gem", updates: []provider.Update{
		{Name: "bigdecimal", Latest: "3.1"},
		{Name: "rake", Latest: "13.2"},
	}}
	p.setTools(provider.Tool{Name: "rake", Version: "13.0", ReadOnly: true})
	inv := New([]provider.Provider{p}, "")
	inv.ScanAll(context.Background())
	inv.CheckOutdated(context.Background(), "")
	s := inv.Snapshot()
	if len(s.Tools) != 1 {
		t.Fatalf("tools = %+v, want only rake", s.Tools)
	}
	if rake := s.Tools[0]; !rake.Outdated || !rake.ReadOnly || rake.Latest != "13.2" {
		t.Fatalf("rake = %+v", rake)
	}
}

func TestOutdatedFillsDescriptionOnlyWhenEmpty(t *testing.T) {
	p := &fakeProvider{name: "npm", updates: []provider.Update{
		{Name: "a", Latest: "2", Description: "from update", Homepage: "https://u.example"},
		{Name: "b", Latest: "2", Description: "from update", Homepage: "https://u.example"},
	}}
	p.setTools(
		provider.Tool{Name: "a", Version: "1"},
		provider.Tool{Name: "b", Version: "1", Description: "from list", Homepage: "https://l.example"},
	)
	inv := New([]provider.Provider{p}, "")
	inv.ScanAll(context.Background())
	inv.CheckOutdated(context.Background(), "npm")
	s := inv.Snapshot()
	if a := findTool(t, s, "npm", "a"); a.Description != "from update" || a.Homepage != "https://u.example" {
		t.Fatalf("a = %+v", a)
	}
	if b := findTool(t, s, "npm", "b"); b.Description != "from list" || b.Homepage != "https://l.example" {
		t.Fatalf("b = %+v", b)
	}
}

func TestSubscribeNotified(t *testing.T) {
	p := &fakeProvider{name: "brew"}
	p.setTools(tool("jq", "1"))
	inv := New([]provider.Provider{p}, "")
	ch, cancel := inv.Subscribe()
	inv.ScanAll(context.Background())
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("no notification after ScanAll")
	}
	// Many changes with nobody reading must not block.
	for range 5 {
		if err := inv.Scan(context.Background(), "brew"); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ch:
	default:
		t.Fatal("no pending notification after Scan")
	}
	cancel()
	cancel() // idempotent
	inv.ScanAll(context.Background())
	drained := make(chan struct{})
	go func() {
		for range ch {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("channel still open after cancel")
	}
}

func TestCheckingUpdatesFlag(t *testing.T) {
	p := &fakeProvider{name: "brew"}
	p.setTools(tool("jq", "1"))
	inv := New([]provider.Provider{p}, "")
	inv.ScanAll(context.Background())
	p.delay = 200 * time.Millisecond

	done := make(chan struct{})
	go func() {
		inv.CheckOutdated(context.Background(), "")
		close(done)
	}()
	deadline := time.After(time.Second)
	for {
		s := inv.Snapshot()
		if s.CheckingUpdates && findStatus(t, s, "brew").Checking {
			break
		}
		select {
		case <-deadline:
			t.Fatal("CheckingUpdates never became true")
		case <-time.After(5 * time.Millisecond):
		}
	}
	<-done
	s := inv.Snapshot()
	if s.CheckingUpdates || findStatus(t, s, "brew").Checking {
		t.Fatalf("still checking after CheckOutdated returned: %+v", s)
	}
}

func TestUnavailableProviderSkipped(t *testing.T) {
	p := &fakeProvider{name: "mas", unavailable: true}
	p.setTools(tool("Xcode", "16"))
	inv := New([]provider.Provider{p}, "")
	inv.ScanAll(context.Background())
	inv.CheckOutdated(context.Background(), "")
	s := inv.Snapshot()
	if st := findStatus(t, s, "mas"); st.Available {
		t.Fatalf("status = %+v, want unavailable", st)
	}
	if list, out := p.calls(); list != 0 || out != 0 {
		t.Fatalf("calls on unavailable provider: list=%d outdated=%d", list, out)
	}
	if len(s.Tools) != 0 {
		t.Fatalf("tools = %+v", s.Tools)
	}
	if err := inv.Scan(context.Background(), "mas"); err == nil {
		t.Fatal("Scan of unavailable provider returned nil error")
	}
}

func TestScanUnknownSource(t *testing.T) {
	inv := New(nil, "")
	if err := inv.Scan(context.Background(), "nope"); err == nil {
		t.Fatal("want error for unknown source")
	}
	if _, ok := inv.Provider("nope"); ok {
		t.Fatal("Provider found unknown source")
	}
	s := inv.Snapshot()
	if s.Tools == nil || s.Providers == nil {
		t.Fatal("snapshot slices must be non-nil for JSON")
	}
}

func TestProviderLookup(t *testing.T) {
	p := &fakeProvider{name: "brew"}
	inv := New([]provider.Provider{p}, "")
	got, ok := inv.Provider("brew")
	if !ok || got != p {
		t.Fatalf("Provider(brew) = %v, %v", got, ok)
	}
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	p := &fakeProvider{name: "brew", updates: []provider.Update{{Name: "jq", Latest: "2"}}}
	p.setTools(tool("jq", "1"))
	q := &fakeProvider{name: "npm"}
	q.setTools(tool("zx", "1"))
	inv := New([]provider.Provider{p, q}, path)
	ch, cancel := inv.Subscribe()
	defer cancel()
	var n atomic.Int32
	go func() {
		for range ch {
			n.Add(1)
		}
	}()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(4)
		go func() { defer wg.Done(); inv.ScanAll(context.Background()) }()
		go func() { defer wg.Done(); _ = inv.Scan(context.Background(), "brew") }()
		go func() { defer wg.Done(); inv.CheckOutdated(context.Background(), "") }()
		go func() { defer wg.Done(); _ = inv.Snapshot() }()
	}
	wg.Wait()
	if jq := findTool(t, inv.Snapshot(), "brew", "jq"); !jq.Outdated {
		t.Fatalf("jq = %+v", jq)
	}
}

func TestUpdatableSources(t *testing.T) {
	s := Snapshot{Tools: []provider.Tool{
		{Name: "jq", Source: "npm", Outdated: true},
		{Name: "wget", Source: "brew", Outdated: true},
		{Name: "openssl", Source: "brew", Outdated: true, Dependency: true},
		{Name: "rails", Source: "gem", Outdated: true, ReadOnly: true},
		{Name: "ruff", Source: "uv"}, // up to date
		{Name: "black", Source: "pip", Outdated: true},
		{Name: "six", Source: "pip", Outdated: true, ReadOnly: true},
		{Name: "lodash", Source: "npm", Outdated: true},
	}}
	if got, want := UpdatableSources(s, false), []string{"brew", "npm"}; !slices.Equal(got, want) {
		t.Errorf("UpdatableSources(includePip=false) = %v, want %v", got, want)
	}
	if got, want := UpdatableSources(s, true), []string{"brew", "npm", "pip"}; !slices.Equal(got, want) {
		t.Errorf("UpdatableSources(includePip=true) = %v, want %v", got, want)
	}
	// A dependency alone still makes its source updatable.
	deps := Snapshot{Tools: []provider.Tool{{Name: "openssl", Source: "brew", Outdated: true, Dependency: true}}}
	if got, want := UpdatableSources(deps, false), []string{"brew"}; !slices.Equal(got, want) {
		t.Errorf("UpdatableSources(deps) = %v, want %v", got, want)
	}
	if got := UpdatableSources(Snapshot{}, true); len(got) != 0 {
		t.Errorf("UpdatableSources(empty) = %v, want none", got)
	}
}
