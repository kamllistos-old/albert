package runner

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Fake is a Runner for tests. It never runs anything. It is safe for
// concurrent use; set the exported fields before use and read Calls only after
// the calls have finished.
type Fake struct {
	Responses map[string]Result // key: name + " " + strings.Join(args, " ")
	Missing   map[string]bool   // LookPath fails for these
	Calls     []string          // keys of every Run/Stream call, in order

	mu sync.Mutex
}

func fakeKey(name string, args []string) string {
	return name + " " + strings.Join(args, " ")
}

func (f *Fake) respond(name string, args []string) Result {
	key := fakeKey(name, args)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, key)
	if res, ok := f.Responses[key]; ok {
		return res
	}
	return Result{ExitCode: 127, Stderr: []byte("fake: no response for " + key)}
}

// Run implements Runner.
func (f *Fake) Run(_ context.Context, name string, args ...string) (Result, error) {
	return f.respond(name, args), nil
}

// Stream implements Runner. It writes Stdout, then Stderr, to out.
func (f *Fake) Stream(_ context.Context, out io.Writer, name string, args ...string) (int, error) {
	res := f.respond(name, args)
	if _, err := out.Write(res.Stdout); err != nil {
		return res.ExitCode, err
	}
	if _, err := out.Write(res.Stderr); err != nil {
		return res.ExitCode, err
	}
	return res.ExitCode, nil
}

// LookPath implements Runner.
func (f *Fake) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Missing[name] {
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	return "/fake/bin/" + name, nil
}

var (
	_ Runner = Exec{}
	_ Runner = (*Fake)(nil)
)
