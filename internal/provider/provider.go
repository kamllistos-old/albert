// Package provider defines the interface every package-manager source
// implements, plus helpers shared by the implementations.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

// Tool is an installed tool.
type Tool struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Version     string `json:"version"`
	Latest      string `json:"latest"`
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
	Path        string `json:"path"`
	ID          string `json:"id"`
	Outdated    bool   `json:"outdated"`
	New         bool   `json:"new"`
	ReadOnly    bool   `json:"readOnly"`
	Dependency  bool   `json:"dependency"`
}

// Update says a newer version of a tool is available.
type Update struct {
	Name        string `json:"name"`
	Latest      string `json:"latest"`
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
}

// Result is a search hit. ID is used by mas.
type Result struct {
	Name        string `json:"name"`
	Source      string `json:"source"`
	Version     string `json:"version"`
	Description string `json:"description"`
	ID          string `json:"id"`
}

var (
	// ErrUnsupported means the source cannot do what was asked.
	ErrUnsupported = errors.New("not supported by this source")
	// ErrReadOnly means the source's tools are not managed by Albert.
	ErrReadOnly = errors.New("this source is read-only")
)

// Provider is one package-manager source.
type Provider interface {
	Name() string
	Available() bool
	List(ctx context.Context) ([]Tool, error)
	Outdated(ctx context.Context) ([]Update, error)
	// Upgrade upgrades the named tools; empty names means all.
	Upgrade(ctx context.Context, names []string, out io.Writer) error
	Install(ctx context.Context, name string, out io.Writer) error
	Search(ctx context.Context, q string) ([]Result, error)
	WatchPaths() []string
}

const maxNameLen = 214

var validName = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9@._/+-]*$`)

// ValidName reports whether s is safe to pass to a package manager as a tool
// name: it cannot start with "-" (flag injection) or contain shell or
// whitespace characters.
func ValidName(s string) bool {
	return len(s) <= maxNameLen && validName.MatchString(s)
}

// Defaults returns every provider in display order: brew, cask, uv, pipx, pip,
// npm, pnpm, bun, mas, cargo, go, gem.
func Defaults(r runner.Runner, reg *registry.Client) []Provider {
	ps := []Provider{NewBrew(r), NewCask(r), NewUV(r, reg), NewPipx(r, reg), NewPip(r),
		NewNPM(r, reg), NewPnpm(r, reg), NewBun(r, reg),
		NewMas(r), NewCargo(r, reg), NewGo(r, reg), NewGem(r)}
	return ps
}

// firstLine returns the first non-empty line of b, trimmed.
func firstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// cmdErr turns a failed command into an error: the first line of stderr, or
// the exit code if stderr is empty.
func cmdErr(name string, res runner.Result) error {
	if msg := firstLine(res.Stderr); msg != "" {
		return fmt.Errorf("%s: %s", name, msg)
	}
	return fmt.Errorf("%s: exit code %d", name, res.ExitCode)
}

// LookupError reports registry lookups that failed during Outdated: network
// trouble or a registry outage. A package the registry does not have
// (registry.ErrNotFound) is not a failure; it simply has no update. Outdated
// returns a LookupError together with the updates it did find. Failed names
// the tools whose newest version is unknown, so callers can keep what they
// knew about those tools before instead of calling them up to date.
type LookupError struct {
	Source string
	Failed []string // tool names, in input order
	Total  int      // tools that needed a lookup, failed ones included
}

// All reports whether no lookup succeeded.
func (e *LookupError) All() bool { return len(e.Failed) >= e.Total }

func (e *LookupError) Error() string {
	if e.All() {
		return e.Source + ": couldn't reach the package registry to check for updates"
	}
	return fmt.Sprintf("%s: couldn't reach the package registry to check %d of %d tools for updates", e.Source, len(e.Failed), e.Total)
}

// LookupAll runs fn for every item, at most limit at a time, and returns the
// updates fn reported, in input order. fn returns ok=false for an item with
// no update (up to date, not on the registry, or not checkable) and an error
// when the lookup itself failed. It stops starting new lookups once ctx is
// done. If any lookup failed or never ran, the error is a *LookupError naming
// those items (by name(item)), returned together with the updates found.
func LookupAll[T any](ctx context.Context, source string, items []T, limit int, name func(T) string, fn func(context.Context, T) (Update, bool, error)) ([]Update, error) {
	if limit < 1 {
		limit = 1
	}
	type slot struct {
		u       Update
		ok      bool
		err     error
		started bool
	}
	slots := make([]slot, len(items))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
loop:
	for i, it := range items {
		if ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			break loop
		case sem <- struct{}{}:
		}
		slots[i].started = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			slots[i].u, slots[i].ok, slots[i].err = fn(ctx, it)
		}()
	}
	wg.Wait()
	out := make([]Update, 0, len(items))
	var failed []string
	for i, s := range slots {
		switch {
		case !s.started || s.err != nil:
			failed = append(failed, name(items[i]))
		case s.ok:
			out = append(out, s.u)
		}
	}
	if len(failed) > 0 {
		return out, &LookupError{Source: source, Failed: failed, Total: len(items)}
	}
	return out, nil
}

// SearchAll searches every available provider in parallel, each bounded by
// timeout, and returns the hits in provider order. Providers that fail or do
// not support search (ErrUnsupported) are skipped. It returns once every
// provider has answered or the timeout has passed, even if a provider ignores
// its context.
func SearchAll(ctx context.Context, ps []Provider, q string, timeout time.Duration) []Result {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type answer struct {
		i  int
		rs []Result
	}
	ch := make(chan answer, len(ps)) // buffered: a late answer never blocks
	pending := 0
	for i, p := range ps {
		if !p.Available() {
			continue
		}
		pending++
		go func() {
			rs, err := p.Search(ctx, q)
			if err != nil {
				rs = nil
			}
			ch <- answer{i, rs}
		}()
	}
	byProvider := make([][]Result, len(ps))
collect:
	for ; pending > 0; pending-- {
		select {
		case a := <-ch:
			byProvider[a.i] = a.rs
		case <-ctx.Done():
			break collect // the rest took too long
		}
	}
	out := []Result{}
	for i, rs := range byProvider {
		for _, r := range rs {
			if r.Source == "" {
				r.Source = ps[i].Name()
			}
			out = append(out, r)
		}
	}
	return out
}
