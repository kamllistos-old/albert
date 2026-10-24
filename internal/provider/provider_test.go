package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

func TestValidName(t *testing.T) {
	good := []string{"jq", "@vercel/ncc", "python@3.12", "golang.org/x/tools/cmd/goimports", "ruff", "a+b", "node_modules-x", strings.Repeat("a", 214)}
	for _, s := range good {
		if !ValidName(s) {
			t.Errorf("ValidName(%q) = false, want true", s)
		}
	}
	bad := []string{"", "--force", "-rf", "a b", "x;rm", "$(id)", strings.Repeat("a", 215), ".hidden", "a\nb", "_x"}
	for _, s := range bad {
		if ValidName(s) {
			t.Errorf("ValidName(%q) = true, want false", s)
		}
	}
}

func TestFirstLine(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"\n  \n":                "",
		"boom\nsecond":          "boom",
		"\n\n  spaced out  \nx": "spaced out",
	}
	for in, want := range cases {
		if got := firstLine([]byte(in)); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCmdErr(t *testing.T) {
	err := cmdErr("brew", runner.Result{Stderr: []byte("\nError: no such formula\nmore"), ExitCode: 1})
	if err == nil || err.Error() != "brew: Error: no such formula" {
		t.Fatalf("got %v", err)
	}
	err = cmdErr("npm", runner.Result{ExitCode: 7})
	if err == nil || err.Error() != "npm: exit code 7" {
		t.Fatalf("got %v", err)
	}
}

func TestSentinelErrors(t *testing.T) {
	if errors.Is(ErrUnsupported, ErrReadOnly) {
		t.Fatal("sentinels must be distinct")
	}
}

func TestDefaultsNonNil(t *testing.T) {
	if got := Defaults(&runner.Fake{}, nil); got == nil {
		t.Fatal("Defaults must return a non-nil slice")
	}
}

func itemName(n int) string { return string(rune('a' + n)) }

func TestLookupAll(t *testing.T) {
	items := make([]int, 40)
	for i := range items {
		items[i] = i
	}
	var cur, peak int32
	var mu sync.Mutex
	got, err := LookupAll(context.Background(), "x", items, 8, itemName, func(ctx context.Context, n int) (Update, bool, error) {
		c := atomic.AddInt32(&cur, 1)
		mu.Lock()
		if c > peak {
			peak = c
		}
		mu.Unlock()
		defer atomic.AddInt32(&cur, -1)
		// Hold the slot so that, were the bound missing, all 40 lookups
		// would overlap and the peak would blow past the limit.
		time.Sleep(30 * time.Millisecond)
		if n%2 == 1 {
			return Update{}, false, nil
		}
		return Update{Name: string(rune('a' + n/2))}, true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("got %d updates, want 20", len(got))
	}
	if peak != 8 {
		t.Fatalf("peak concurrency = %d, want exactly the limit 8", peak)
	}
	// Order follows input order.
	for i, u := range got {
		if u.Name != string(rune('a'+i)) {
			t.Fatalf("result %d = %q, order not preserved: %+v", i, u.Name, got)
		}
	}
}

func TestLookupAllReportsFailedLookups(t *testing.T) {
	// b fails, c is up to date, d has an update: the update is still
	// returned, alongside an error naming exactly the failed item.
	got, err := LookupAll(context.Background(), "npm", []int{1, 2, 3}, 2, itemName, func(ctx context.Context, n int) (Update, bool, error) {
		switch n {
		case 1:
			return Update{}, false, errors.New("registry down")
		case 2:
			return Update{}, false, nil
		}
		return Update{Name: itemName(n), Latest: "2"}, true, nil
	})
	if len(got) != 1 || got[0].Name != "d" {
		t.Fatalf("updates = %+v, want just d", got)
	}
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want *LookupError", err)
	}
	if le.Source != "npm" || le.Total != 3 || strings.Join(le.Failed, ",") != "b" || le.All() {
		t.Errorf("LookupError = %+v", le)
	}
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Errorf("message = %q, want it to say how many could not be checked", err)
	}
}

func TestLookupAllEveryLookupFailed(t *testing.T) {
	got, err := LookupAll(context.Background(), "uv", []int{0, 1}, 2, itemName, func(context.Context, int) (Update, bool, error) {
		return Update{}, false, errors.New("offline")
	})
	var le *LookupError
	if len(got) != 0 || !errors.As(err, &le) || !le.All() {
		t.Fatalf("got %+v, %v; want no updates and an all-failed LookupError", got, err)
	}
	if err.Error() != "uv: couldn't reach the package registry to check for updates" {
		t.Errorf("message = %q", err)
	}
}

func TestLookupAllStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int32
	got, err := LookupAll(ctx, "x", []int{1, 2, 3}, 2, itemName, func(ctx context.Context, n int) (Update, bool, error) {
		atomic.AddInt32(&calls, 1)
		return Update{Name: "x"}, true, nil
	})
	if len(got) != 0 || calls != 0 {
		t.Fatalf("cancelled ctx should do no work: got %d updates, %d calls", len(got), calls)
	}
	// Lookups that never ran are unknown, not "up to date".
	var le *LookupError
	if !errors.As(err, &le) || len(le.Failed) != 3 {
		t.Fatalf("err = %v, want all 3 reported as failed", err)
	}
}

func TestLookupAllEmpty(t *testing.T) {
	got, err := LookupAll(context.Background(), "x", []int(nil), 0, itemName, func(context.Context, int) (Update, bool, error) { return Update{}, true, nil })
	if len(got) != 0 || err != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// outageRegistry returns a client whose every registry answers 503, like a
// registry outage.
func outageRegistry(t *testing.T) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return &registry.Client{HTTP: srv.Client(), PyPIURL: srv.URL, NPMURL: srv.URL, CratesURL: srv.URL, GoProxyURL: srv.URL}
}

// requireAllFailed checks that Outdated reported a registry outage as an
// error rather than "no updates".
func requireAllFailed(t *testing.T, ups []Update, err error) {
	t.Helper()
	var le *LookupError
	if !errors.As(err, &le) || !le.All() {
		t.Fatalf("err = %v, want an all-failed *LookupError", err)
	}
	if len(ups) != 0 {
		t.Fatalf("updates = %+v, want none", ups)
	}
}

func TestJSONTagsLowerCamel(t *testing.T) {
	for _, v := range []any{Update{}, Result{}, Tool{}} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if k == "" || k[0] < 'a' || k[0] > 'z' {
				t.Errorf("%T has non-lowerCamel json key %q", v, k)
			}
		}
		if len(m) == 0 {
			t.Errorf("%T serialized no fields", v)
		}
	}
}

// searchFake is a Provider whose only interesting method is Search.
type searchFake struct {
	name        string
	unavailable bool
	results     []Result
	err         error
	hang        bool // Search blocks forever, ignoring ctx
	called      atomic.Bool
}

func (f *searchFake) Name() string                                       { return f.name }
func (f *searchFake) Available() bool                                    { return !f.unavailable }
func (f *searchFake) List(context.Context) ([]Tool, error)               { return nil, nil }
func (f *searchFake) Outdated(context.Context) ([]Update, error)         { return nil, nil }
func (f *searchFake) Upgrade(context.Context, []string, io.Writer) error { return nil }
func (f *searchFake) Install(context.Context, string, io.Writer) error   { return nil }
func (f *searchFake) WatchPaths() []string                               { return nil }
func (f *searchFake) Search(ctx context.Context, q string) ([]Result, error) {
	f.called.Store(true)
	if f.hang {
		select {} // ignores ctx on purpose
	}
	return f.results, f.err
}

func TestSearchAll(t *testing.T) {
	ps := []Provider{
		&searchFake{name: "brew", results: []Result{{Name: "jq", Source: "brew"}, {Name: "jql", Source: "brew"}}},
		&searchFake{name: "cargo", err: ErrUnsupported},
		&searchFake{name: "mas", err: errors.New("mas: boom")},
		&searchFake{name: "gem", unavailable: true, results: []Result{{Name: "nope"}}},
		&searchFake{name: "npm", results: []Result{{Name: "jq-cli"}}}, // Source left empty
	}
	got := SearchAll(context.Background(), ps, "jq", time.Second)
	want := []string{"brew/jq", "brew/jql", "npm/jq-cli"}
	if len(got) != len(want) {
		t.Fatalf("SearchAll = %+v, want %v", got, want)
	}
	for i, r := range got {
		if r.Source+"/"+r.Name != want[i] {
			t.Fatalf("SearchAll = %+v, want %v", got, want)
		}
	}
	if ps[3].(*searchFake).called.Load() {
		t.Error("unavailable provider was searched")
	}
}

func TestSearchAllTimeoutBounded(t *testing.T) {
	ps := []Provider{
		&searchFake{name: "brew", hang: true},
		&searchFake{name: "npm", results: []Result{{Name: "x", Source: "npm"}}},
	}
	start := time.Now()
	got := SearchAll(context.Background(), ps, "x", 100*time.Millisecond)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("SearchAll took %v with a hung provider; want about the timeout", d)
	}
	if len(got) != 1 || got[0].Name != "x" {
		t.Fatalf("SearchAll = %+v, want the npm result only", got)
	}
}
