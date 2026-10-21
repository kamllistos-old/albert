package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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

func TestDefaultsEmptyUntilProvidersExist(t *testing.T) {
	if got := Defaults(&runner.Fake{}, nil); got == nil {
		t.Fatal("Defaults must return a non-nil slice")
	}
}

func TestLookupAll(t *testing.T) {
	items := make([]int, 40)
	for i := range items {
		items[i] = i
	}
	var cur, peak int32
	var mu sync.Mutex
	got := LookupAll(context.Background(), items, 8, func(ctx context.Context, n int) (Update, bool) {
		c := atomic.AddInt32(&cur, 1)
		mu.Lock()
		if c > peak {
			peak = c
		}
		mu.Unlock()
		defer atomic.AddInt32(&cur, -1)
		if n%2 == 1 {
			return Update{}, false
		}
		return Update{Name: string(rune('a' + n/2))}, true
	})
	if len(got) != 20 {
		t.Fatalf("got %d updates, want 20", len(got))
	}
	if peak > 8 {
		t.Fatalf("peak concurrency %d exceeds limit 8", peak)
	}
	// Order follows input order.
	for i, u := range got {
		if u.Name != string(rune('a'+i)) {
			t.Fatalf("result %d = %q, order not preserved: %+v", i, u.Name, got)
		}
	}
}

func TestLookupAllStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls int32
	got := LookupAll(ctx, []int{1, 2, 3}, 2, func(ctx context.Context, n int) (Update, bool) {
		atomic.AddInt32(&calls, 1)
		return Update{Name: "x"}, true
	})
	if len(got) != 0 || calls != 0 {
		t.Fatalf("cancelled ctx should do no work: got %d updates, %d calls", len(got), calls)
	}
}

func TestLookupAllEmpty(t *testing.T) {
	if got := LookupAll(context.Background(), []int(nil), 0, func(context.Context, int) (Update, bool) { return Update{}, true }); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}
