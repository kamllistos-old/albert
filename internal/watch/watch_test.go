package watch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// collector records onChange calls and lets tests wait on them.
type collector struct {
	mu    sync.Mutex
	calls []string
	ch    chan string
}

func newCollector() *collector { return &collector{ch: make(chan string, 64)} }

func (c *collector) onChange(source string) {
	c.mu.Lock()
	c.calls = append(c.calls, source)
	c.mu.Unlock()
	c.ch <- source
}

func (c *collector) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

// start runs Watch in a goroutine and returns a stop func that cancels and
// waits for Watch to return.
func start(t *testing.T, paths map[string][]string, debounce time.Duration, onChange func(string)) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, paths, debounce, onChange) }()
	// Give the watcher time to register its directories.
	time.Sleep(100 * time.Millisecond)
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(3 * time.Second):
			t.Fatal("Watch did not return after cancel")
			return nil
		}
	}
}

func write(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func expect(t *testing.T, c *collector, want string) {
	t.Helper()
	select {
	case got := <-c.ch:
		if got != want {
			t.Fatalf("onChange(%q), want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for onChange(%q)", want)
	}
}

func TestWatchDebounces(t *testing.T) {
	dir := t.TempDir()
	c := newCollector()
	stop := start(t, map[string][]string{"brew": {dir}}, 200*time.Millisecond, c.onChange)

	for i := 0; i < 5; i++ {
		write(t, dir, string(rune('a'+i)))
		time.Sleep(20 * time.Millisecond)
	}
	expect(t, c, "brew")

	// No second call should follow the single debounced burst.
	time.Sleep(500 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := c.snapshot(); len(got) != 1 || got[0] != "brew" {
		t.Fatalf("calls = %v, want exactly [brew]", got)
	}
}

func TestWatchMissingDirSkipped(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist")
	c := newCollector()
	paths := map[string][]string{"npm": {missing, "", dir, dir}}
	stop := start(t, paths, 50*time.Millisecond, c.onChange)

	write(t, dir, "f")
	expect(t, c, "npm")
	if err := stop(); err != nil {
		t.Fatalf("Watch returned error with a missing dir: %v", err)
	}
}

func TestWatchAllDirsMissing(t *testing.T) {
	c := newCollector()
	stop := start(t, map[string][]string{"x": {"/no/such/dir/albert"}}, 50*time.Millisecond, c.onChange)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if got := c.snapshot(); len(got) != 0 {
		t.Fatalf("unexpected calls: %v", got)
	}
}

func TestWatchSeparateSources(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	c := newCollector()
	stop := start(t, map[string][]string{"a": {dirA}, "b": {dirB}}, 50*time.Millisecond, c.onChange)

	write(t, dirA, "f")
	expect(t, c, "a")
	write(t, dirB, "f")
	expect(t, c, "b")

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	got := c.snapshot()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("calls = %v, want [a b]", got)
	}
}

func TestWatchSharedDirNotifiesBothSources(t *testing.T) {
	dir := t.TempDir()
	c := newCollector()
	stop := start(t, map[string][]string{"a": {dir}, "b": {dir}}, 50*time.Millisecond, c.onChange)

	write(t, dir, "f")
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case s := <-c.ch:
			seen[s] = true
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out; saw %v", seen)
		}
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("seen = %v, want both a and b", seen)
	}
}

func TestWatchNoCallAfterReturn(t *testing.T) {
	dir := t.TempDir()
	c := newCollector()
	stop := start(t, map[string][]string{"a": {dir}}, 300*time.Millisecond, c.onChange)

	write(t, dir, "f") // arms the timer, then cancel before it fires
	time.Sleep(50 * time.Millisecond)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := c.snapshot(); len(got) != 0 {
		t.Fatalf("onChange called after Watch returned: %v", got)
	}
}
