package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kamllistos/albert/internal/inventory"
	"github.com/kamllistos/albert/internal/provider"
)

// fakeProvider is a test-only provider whose Upgrade and Install are
// supplied by the test.
type fakeProvider struct {
	name    string
	upgrade func(ctx context.Context, names []string, out io.Writer) error
	install func(ctx context.Context, name string, out io.Writer) error
	remove  func(ctx context.Context, name string, out io.Writer) error

	mu        sync.Mutex
	version   string
	latest    string // "" means nothing outdated
	listCalls int
	outCalls  int
}

func (f *fakeProvider) Name() string    { return f.name }
func (f *fakeProvider) Available() bool { return true }

func (f *fakeProvider) List(context.Context) ([]provider.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return []provider.Tool{{Name: "cowsay", Version: f.version}}, nil
}

func (f *fakeProvider) Outdated(context.Context) ([]provider.Update, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outCalls++
	if f.latest == "" {
		return nil, nil
	}
	return []provider.Update{{Name: "cowsay", Latest: f.latest}}, nil
}

func (f *fakeProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if f.upgrade != nil {
		return f.upgrade(ctx, names, out)
	}
	return nil
}

func (f *fakeProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if f.install != nil {
		return f.install(ctx, name, out)
	}
	return nil
}

func (f *fakeProvider) Remove(ctx context.Context, name string, out io.Writer) error {
	if f.remove != nil {
		return f.remove(ctx, name, out)
	}
	return nil
}

func (f *fakeProvider) Search(context.Context, string) ([]provider.Result, error) { return nil, nil }
func (f *fakeProvider) WatchPaths() []string                                      { return nil }

func (f *fakeProvider) set(version, latest string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version, f.latest = version, latest
}

func (f *fakeProvider) calls() (list, out int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls, f.outCalls
}

func newManager(t *testing.T, ps ...*fakeProvider) *Manager {
	t.Helper()
	var all []provider.Provider
	for _, p := range ps {
		all = append(all, p)
	}
	m := NewManager(inventory.New(all, ""))
	t.Cleanup(m.Close)
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func waitDone(t *testing.T, m *Manager, id string) Job {
	t.Helper()
	var j Job
	waitFor(t, "job "+id+" to finish", func() bool {
		var ok bool
		j, ok = m.Get(id)
		return ok && (j.Status == StatusSuccess || j.Status == StatusFailed)
	})
	return j
}

func status(m *Manager, id string) string {
	j, _ := m.Get(id)
	return j.Status
}

func TestJobSuccessRescans(t *testing.T) {
	p := &fakeProvider{name: "brew", version: "1.0", latest: "2.0"}
	p.upgrade = func(_ context.Context, names []string, out io.Writer) error {
		fmt.Fprintln(out, "upgrading", strings.Join(names, ","))
		p.set("2.0", "")
		return nil
	}
	inv := inventory.New([]provider.Provider{p}, "")
	m := NewManager(inv)
	defer m.Close()
	inv.ScanAll(context.Background())
	inv.CheckOutdated(context.Background(), "")
	if tl := inv.Snapshot().Tools; len(tl) != 1 || !tl[0].Outdated {
		t.Fatalf("setup: want one outdated tool, got %+v", tl)
	}
	l0, o0 := p.calls()

	ch, unsub := m.Subscribe()
	defer unsub()
	id, err := m.Submit("brew", "update", []string{"cowsay"})
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 8 {
		t.Errorf("id %q: want 8 hex chars", id)
	}
	j := waitDone(t, m, id)

	if j.Status != StatusSuccess || j.Message != "" {
		t.Errorf("got status %q message %q", j.Status, j.Message)
	}
	if j.Output != "upgrading cowsay\n" {
		t.Errorf("output %q", j.Output)
	}
	if j.Kind != "update" || j.Source != "brew" || len(j.Targets) != 1 || j.Targets[0] != "cowsay" {
		t.Errorf("job fields %+v", j)
	}
	if j.Started.IsZero() || j.Ended.IsZero() || j.Ended.Before(j.Started) {
		t.Errorf("times started=%v ended=%v", j.Started, j.Ended)
	}
	l1, o1 := p.calls()
	if l1 != l0+1 || o1 != o0+1 {
		t.Errorf("rescan: list %d->%d, outdated %d->%d; want one each", l0, l1, o0, o1)
	}
	// The inventory is already fresh by the time Done is visible.
	if tl := inv.Snapshot().Tools; len(tl) != 1 || tl[0].Version != "2.0" || tl[0].Outdated {
		t.Errorf("inventory after job: %+v", tl)
	}
	// And the Done event is only emitted after the rescan.
	var done *Event
	for e := range ch {
		if e.Done {
			e := e
			done = &e
			break
		}
	}
	if done == nil || done.Job == nil || done.Job.Status != StatusSuccess || done.JobID != id {
		t.Errorf("done event %+v", done)
	}
}

func TestFailedJobRescansToo(t *testing.T) {
	p := &fakeProvider{name: "brew", version: "1.0"}
	p.upgrade = func(context.Context, []string, io.Writer) error { return errors.New("boom") }
	m := newManager(t, p)
	id, _ := m.Submit("brew", "update", nil)
	j := waitDone(t, m, id)
	if j.Status != StatusFailed || j.Message != "boom" {
		t.Errorf("got %q %q", j.Status, j.Message)
	}
	if l, o := p.calls(); l != 1 || o != 1 {
		t.Errorf("rescan after failure: list %d outdated %d", l, o)
	}
}

func TestSameSourceSerialized(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 4)
	p := &fakeProvider{name: "brew"}
	p.upgrade = func(_ context.Context, names []string, _ io.Writer) error {
		started <- names[0]
		<-release
		return nil
	}
	m := newManager(t, p)
	id1, _ := m.Submit("brew", "update", []string{"a"})
	if got := <-started; got != "a" {
		t.Fatalf("first started %q", got)
	}
	id2, _ := m.Submit("brew", "update", []string{"b"})
	id3, _ := m.Submit("brew", "update", []string{"c"})

	time.Sleep(50 * time.Millisecond)
	if s := status(m, id1); s != StatusRunning {
		t.Errorf("job 1 is %q, want running", s)
	}
	if s := status(m, id2); s != StatusQueued {
		t.Errorf("job 2 is %q, want queued", s)
	}
	select {
	case n := <-started:
		t.Fatalf("%q started while job 1 was running", n)
	default:
	}

	release <- struct{}{}
	waitDone(t, m, id1)
	if got := <-started; got != "b" { // queued jobs run in submit order
		t.Errorf("second started %q, want b", got)
	}
	if s := status(m, id3); s != StatusQueued {
		t.Errorf("job 3 is %q, want queued", s)
	}
	release <- struct{}{}
	waitDone(t, m, id2)
	if got := <-started; got != "c" {
		t.Errorf("third started %q, want c", got)
	}
	release <- struct{}{}
	waitDone(t, m, id3)
}

func TestBrewAndCaskShareOneQueue(t *testing.T) {
	// Both run Homebrew, which refuses a second concurrent process.
	release := make(chan struct{})
	started := make(chan string, 2)
	mk := func(name string) *fakeProvider {
		return &fakeProvider{name: name, upgrade: func(ctx context.Context, _ []string, _ io.Writer) error {
			started <- name
			select {
			case <-release:
			case <-ctx.Done(): // so a failing test still shuts down
			}
			return nil
		}}
	}
	m := newManager(t, mk("brew"), mk("cask"))
	id1, _ := m.Submit("brew", "update", nil)
	if got := <-started; got != "brew" {
		t.Fatalf("first started %q", got)
	}
	id2, _ := m.Submit("cask", "update", nil)
	time.Sleep(50 * time.Millisecond)
	select {
	case n := <-started:
		t.Fatalf("%q started while the brew job was running", n)
	default:
	}
	if s := status(m, id2); s != StatusQueued {
		t.Errorf("cask job is %q, want queued", s)
	}
	release <- struct{}{}
	waitDone(t, m, id1)
	if got := <-started; got != "cask" {
		t.Errorf("second started %q, want cask", got)
	}
	release <- struct{}{}
	if j := waitDone(t, m, id2); j.Source != "cask" {
		t.Errorf("job source = %q, want cask", j.Source)
	}
}

func TestDifferentSourcesParallel(t *testing.T) {
	release := make(chan struct{})
	started := make(chan string, 2)
	mk := func(name string) *fakeProvider {
		return &fakeProvider{name: name, upgrade: func(context.Context, []string, io.Writer) error {
			started <- name
			<-release
			return nil
		}}
	}
	m := newManager(t, mk("brew"), mk("npm"))
	id1, _ := m.Submit("brew", "update", nil)
	id2, _ := m.Submit("npm", "update", nil)
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("both sources should be running at once")
		}
	}
	if status(m, id1) != StatusRunning || status(m, id2) != StatusRunning {
		t.Errorf("statuses %q %q", status(m, id1), status(m, id2))
	}
	close(release)
	waitDone(t, m, id1)
	waitDone(t, m, id2)
}

func TestSudoMessage(t *testing.T) {
	const want = `This needs your password. Run "albert update %s" in Terminal.`
	for _, out := range []string{"sudo: a password is required\n", "sudo: a terminal is required to read the password", "Password:"} {
		for _, tc := range []struct {
			names  []string
			target string
		}{{[]string{"firefox", "other"}, "firefox"}, {nil, "--all --source cask"}} {
			p := &fakeProvider{name: "cask"}
			p.upgrade = func(_ context.Context, _ []string, w io.Writer) error {
				io.WriteString(w, out)
				return errors.New("exit status 1")
			}
			m := newManager(t, p)
			id, _ := m.Submit("cask", "update", tc.names)
			j := waitDone(t, m, id)
			if j.Status != StatusFailed || j.Message != fmt.Sprintf(want, tc.target) {
				t.Errorf("output %q targets %v: got %q %q", out, tc.names, j.Status, j.Message)
			}
		}
	}
}

func TestSudoMessageUpdateAllPip(t *testing.T) {
	// "albert update --all" skips pip unless --include-pip is given.
	p := &fakeProvider{name: "pip"}
	p.upgrade = func(_ context.Context, _ []string, w io.Writer) error {
		io.WriteString(w, "sudo: a password is required\n")
		return errors.New("exit status 1")
	}
	m := newManager(t, p)
	id, _ := m.Submit("pip", "update", nil)
	const want = `This needs your password. Run "albert update --all --source pip --include-pip" in Terminal.`
	if j := waitDone(t, m, id); j.Message != want {
		t.Errorf("message = %q, want %q", j.Message, want)
	}
}

// R13: password text alone does not fail a job whose provider succeeded.
func TestPasswordTextWithoutErrorStaysSuccess(t *testing.T) {
	p := &fakeProvider{name: "cask"}
	p.upgrade = func(_ context.Context, _ []string, w io.Writer) error {
		io.WriteString(w, "Password:")
		return nil
	}
	m := newManager(t, p)
	id, _ := m.Submit("cask", "update", []string{"x"})
	if j := waitDone(t, m, id); j.Status != StatusSuccess || j.Message != "" {
		t.Errorf("got %q %q", j.Status, j.Message)
	}
}

// R14: install jobs point at "albert install".
func TestSudoMessageInstall(t *testing.T) {
	p := &fakeProvider{name: "cask"}
	p.install = func(_ context.Context, _ string, w io.Writer) error {
		io.WriteString(w, "sudo: a terminal is required to read the password\n")
		return errors.New("exit status 1")
	}

	m := newManager(t, p)
	id, _ := m.Submit("cask", "install", []string{"firefox"})
	j := waitDone(t, m, id)
	const want = `This needs your password. Run "albert install firefox" in Terminal.`
	if j.Status != StatusFailed || j.Message != want {
		t.Errorf("got %q %q", j.Status, j.Message)
	}
}

// R14b: delete jobs point at "albert delete --yes".
func TestSudoMessageDelete(t *testing.T) {
	p := &fakeProvider{name: "cask"}
	p.remove = func(_ context.Context, _ string, w io.Writer) error {
		io.WriteString(w, "sudo: a terminal is required to read the password\n")
		return errors.New("exit status 1")
	}
	m := newManager(t, p)
	id, _ := m.Submit("cask", "delete", []string{"firefox"})
	j := waitDone(t, m, id)
	const want = `This needs your password. Run "albert delete --yes firefox" in Terminal.`
	if j.Status != StatusFailed || j.Message != want {
		t.Errorf("got %q %q", j.Status, j.Message)
	}
}

func TestSentinelErrorsUseErrorText(t *testing.T) {
	for _, base := range []error{provider.ErrReadOnly, provider.ErrUnsupported} {
		p := &fakeProvider{name: "pip"}
		p.upgrade = func(context.Context, []string, io.Writer) error { return fmt.Errorf("pip: %w", base) }
		m := newManager(t, p)
		id, _ := m.Submit("pip", "update", nil)
		j := waitDone(t, m, id)
		if j.Status != StatusFailed || j.Message != "pip: "+base.Error() {
			t.Errorf("%v: got %q %q", base, j.Status, j.Message)
		}
	}
}

func TestInstallJob(t *testing.T) {
	var got string
	p := &fakeProvider{name: "uv"}
	p.install = func(_ context.Context, name string, out io.Writer) error {
		got = name
		fmt.Fprintln(out, "installed", name)
		return nil
	}

	m := newManager(t, p)
	id, err := m.Submit("uv", "install", []string{"cowsay"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitDone(t, m, id); j.Status != StatusSuccess || got != "cowsay" || j.Kind != "install" {
		t.Errorf("got %+v, installed %q", j, got)
	}
}

func TestDeleteJob(t *testing.T) {
	var got string
	p := &fakeProvider{name: "uv"}
	p.remove = func(_ context.Context, name string, out io.Writer) error {
		got = name
		fmt.Fprintln(out, "removed", name)
		return nil
	}
	m := newManager(t, p)
	id, err := m.Submit("uv", "delete", []string{"cowsay"})
	if err != nil {
		t.Fatal(err)
	}
	if j := waitDone(t, m, id); j.Status != StatusSuccess || got != "cowsay" || j.Kind != "delete" {
		t.Errorf("got %+v, removed %q", j, got)
	}
}

func TestSubmitValidation(t *testing.T) {
	m := newManager(t, &fakeProvider{name: "brew"})
	tests := []struct {
		name         string
		source, kind string
		names        []string
	}{
		{"flag injection", "brew", "update", []string{"--force"}},
		{"shell chars", "brew", "update", []string{"a;rm"}},
		{"bad name among good", "brew", "update", []string{"ok", "-x"}},
		{"empty name", "brew", "update", []string{""}},
		{"unknown source", "nope", "update", nil},
		{"unknown kind", "brew", "remove", []string{"a"}},
		{"install no name", "brew", "install", nil},
		{"install two names", "brew", "install", []string{"a", "b"}},
		{"install bad name", "brew", "install", []string{"--x"}},
		{"delete no name", "brew", "delete", nil},
		{"delete two names", "brew", "delete", []string{"a", "b"}},
		{"delete bad name", "brew", "delete", []string{"--x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if id, err := m.Submit(tc.source, tc.kind, tc.names); err == nil {
				t.Errorf("accepted, id %q", id)
			}
		})
	}
	if n := len(m.List()); n != 0 {
		t.Errorf("rejected submits left %d jobs", n)
	}
}

// TestSubmitRejectsBadName is the name from the brief's list.
func TestSubmitRejectsBadName(t *testing.T) {
	m := newManager(t, &fakeProvider{name: "brew"})
	if _, err := m.Submit("brew", "update", []string{"--help"}); err == nil {
		t.Error("want error for a name starting with -")
	}
	if _, err := m.Submit("brew", "update", []string{"git", "node@20"}); err != nil {
		t.Errorf("valid names rejected: %v", err)
	}
}

func TestEventsStreamLines(t *testing.T) {
	p := &fakeProvider{name: "brew"}
	p.upgrade = func(_ context.Context, _ []string, w io.Writer) error {
		io.WriteString(w, "one\ntw")
		io.WriteString(w, "o\n\nthree")
		return nil
	}
	m := newManager(t, p)
	ch, unsub := m.Subscribe()
	defer unsub()
	id, _ := m.Submit("brew", "update", nil)

	var lines []string
	var done *Event
	for done == nil {
		select {
		case e := <-ch:
			if e.JobID != id {
				t.Fatalf("event for job %q", e.JobID)
			}
			if e.Done {
				done = &e
			} else {
				lines = append(lines, e.Line)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no done event")
		}
	}
	if want := "one|two||three"; strings.Join(lines, "|") != want {
		t.Errorf("lines %q, want %q", lines, want)
	}
	if done.Job == nil || done.Job.Output != "one\ntwo\n\nthree" || done.Job.Status != StatusSuccess {
		t.Errorf("done job %+v", done.Job)
	}
}

func TestSlowSubscriberNeverMissesDone(t *testing.T) {
	const lines = 1000
	p := &fakeProvider{name: "brew"}
	p.upgrade = func(_ context.Context, _ []string, w io.Writer) error {
		for i := range lines {
			fmt.Fprintf(w, "line %d\n", i)
		}
		return nil
	}
	m := newManager(t, p)
	slow, unsub := m.Subscribe() // nobody reads until the job is over
	defer unsub()
	id1, _ := m.Submit("brew", "update", nil)
	waitDone(t, m, id1) // the job was not blocked by the full subscriber
	id2, _ := m.Submit("brew", "update", nil)
	waitDone(t, m, id2)

	var n int
	done := map[string]bool{}
	for len(slow) > 0 {
		e := <-slow
		if e.Done {
			done[e.JobID] = true
		} else {
			n++
		}
	}
	if !done[id1] || !done[id2] {
		t.Errorf("done events kept: %v; want both jobs", done)
	}
	if n >= 2*lines {
		t.Errorf("kept %d lines, expected some to be dropped", n)
	}
}

func TestUnsubscribe(t *testing.T) {
	m := newManager(t, &fakeProvider{name: "brew"})
	ch, unsub := m.Subscribe()
	unsub()
	unsub() // twice is fine
	if _, ok := <-ch; ok {
		t.Error("channel should be closed")
	}
	id, _ := m.Submit("brew", "update", nil) // must not panic on the closed channel
	waitDone(t, m, id)
}

func TestListAndGetReturnCopies(t *testing.T) {
	m := newManager(t, &fakeProvider{name: "brew"})
	var ids []string
	for i := range 3 {
		id, _ := m.Submit("brew", "update", []string{fmt.Sprint("t", i)})
		ids = append(ids, id)
		waitDone(t, m, id)
	}
	l := m.List()
	if len(l) != 3 || l[0].ID != ids[2] || l[2].ID != ids[0] {
		t.Fatalf("want newest first, got %+v", l)
	}
	l[0].Targets[0] = "changed"
	l[0].Status = "bogus"
	g, _ := m.Get(ids[2])
	g.Targets[0] = "changed"
	if g2, _ := m.Get(ids[2]); g2.Targets[0] != "t2" || g2.Status != StatusSuccess {
		t.Errorf("caller mutation leaked into the manager: %+v", g2)
	}
	if _, ok := m.Get("nonesuch"); ok {
		t.Error("Get found a missing job")
	}
}

func TestKeepsNewest50(t *testing.T) {
	m := newManager(t, &fakeProvider{name: "brew"})
	var first, last string
	for i := range 55 {
		id, _ := m.Submit("brew", "update", nil)
		waitDone(t, m, id)
		if i == 0 {
			first = id
		}
		last = id
	}
	l := m.List()
	if len(l) != 50 || l[0].ID != last {
		t.Errorf("kept %d jobs, newest %q", len(l), l[0].ID)
	}
	if _, ok := m.Get(first); ok {
		t.Error("oldest job should be gone")
	}
}

func TestCloseCancelsRunningAndQueued(t *testing.T) {
	started := make(chan struct{})
	p := &fakeProvider{name: "brew"}
	p.upgrade = func(ctx context.Context, _ []string, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	inv := inventory.New([]provider.Provider{p}, "")
	m := NewManager(inv)
	ch, _ := m.Subscribe()
	id1, _ := m.Submit("brew", "update", nil)
	id2, _ := m.Submit("brew", "update", nil)
	<-started

	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	for _, id := range []string{id1, id2} {
		j, _ := m.Get(id)
		if j.Status != StatusFailed || j.Message == "" {
			t.Errorf("job %s: %q %q", id, j.Status, j.Message)
		}
	}
	if l, o := p.calls(); l != 0 || o != 0 {
		t.Errorf("rescanned during shutdown: list %d outdated %d", l, o)
	}
	var dones int
	for e := range ch { // closed by Close
		if e.Done {
			dones++
		}
	}
	if dones != 2 {
		t.Errorf("got %d done events, want 2", dones)
	}
	if _, err := m.Submit("brew", "update", nil); err == nil {
		t.Error("Submit after Close should fail")
	}
	m.Close() // idempotent
}

func TestProviderPanicFailsJob(t *testing.T) {
	p := &fakeProvider{name: "brew"}
	p.upgrade = func(context.Context, []string, io.Writer) error { panic("oops") }
	m := newManager(t, p)
	id, _ := m.Submit("brew", "update", nil)
	if j := waitDone(t, m, id); j.Status != StatusFailed || !strings.Contains(j.Message, "oops") {
		t.Errorf("got %q %q", j.Status, j.Message)
	}
}
