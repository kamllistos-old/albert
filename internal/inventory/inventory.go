// Package inventory keeps the merged list of installed tools from every
// provider, with update badges, new-tool detection and an on-disk cache.
package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kamllistos/albert/internal/provider"
)

// ListTimeout and OutdatedTimeout bound each provider's List and Outdated call.
var ListTimeout, OutdatedTimeout = 30 * time.Second, 120 * time.Second

// newFor is how long a tool counts as New after it was first seen.
const newFor = 24 * time.Hour

// ProviderStatus is the state of one provider.
type ProviderStatus struct {
	Name      string    `json:"name"`
	Available bool      `json:"available"`
	Error     string    `json:"error"`
	Checking  bool      `json:"checking"`
	LastScan  time.Time `json:"lastScan"`
}

// Snapshot is a consistent copy of the inventory.
type Snapshot struct {
	Tools           []provider.Tool  `json:"tools"`
	Providers       []ProviderStatus `json:"providers"`
	CheckingUpdates bool             `json:"checkingUpdates"`
}

// source is the inventory's state for one provider. Guarded by Inventory.mu.
type source struct {
	p         provider.Provider
	available bool
	lastScan  time.Time
	listErr   string
	outErr    string
	tools     []provider.Tool            // as listed, Source set
	latest    map[string]provider.Update // from the last successful Outdated (or cache)
	seeded    bool                       // had a successful List in this process
	checking  int                        // CheckOutdated calls in flight

	// Sequence numbers so a slow, older call never overwrites a newer result.
	listSeq, listApplied uint64
	outSeq, outApplied   uint64
}

// Inventory is safe for concurrent use. It never holds its mutex while
// calling a provider.
type Inventory struct {
	cachePath string
	srcs      []*source          // input order; the slice is never modified
	byName    map[string]*source // never modified after New

	saveMu sync.Mutex // serializes cache writes

	mu        sync.Mutex
	firstRun  bool // no usable cache: tools a source lists first are not New
	firstSeen map[string]time.Time
	subs      map[int]chan struct{}
	nextSub   int
}

// cacheFile is the on-disk cache format.
type cacheFile struct {
	FirstSeen map[string]string            `json:"firstSeen"`
	Latest    map[string]map[string]string `json:"latest"`
}

// New returns an inventory over ps. cachePath "" means no persistence.
func New(ps []provider.Provider, cachePath string) *Inventory {
	i := &Inventory{
		cachePath: cachePath,
		byName:    make(map[string]*source, len(ps)),
		firstSeen: map[string]time.Time{},
		subs:      map[int]chan struct{}{},
	}
	for _, p := range ps {
		s := &source{p: p, available: p.Available()}
		i.srcs = append(i.srcs, s)
		i.byName[p.Name()] = s
	}
	i.firstRun = !i.load()
	return i
}

// load reads the cache. It reports false if there is no usable cache: the
// file is missing, unreadable or corrupt, or persistence is off.
func (i *Inventory) load() bool {
	if i.cachePath == "" {
		return false
	}
	data, err := os.ReadFile(i.cachePath)
	if err != nil {
		return false
	}
	var c cacheFile
	if err := json.Unmarshal(data, &c); err != nil {
		return false
	}
	for k, v := range c.FirstSeen {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			i.firstSeen[k] = t
		}
	}
	for name, vers := range c.Latest {
		s, ok := i.byName[name]
		if !ok {
			continue
		}
		s.latest = make(map[string]provider.Update, len(vers))
		for tool, v := range vers {
			s.latest[tool] = provider.Update{Name: tool, Latest: v}
		}
	}
	return true
}

// save writes the cache atomically. Errors are ignored: the cache is only an
// optimisation.
func (i *Inventory) save() {
	if i.cachePath == "" {
		return
	}
	i.saveMu.Lock()
	defer i.saveMu.Unlock()

	i.mu.Lock()
	c := cacheFile{
		FirstSeen: make(map[string]string, len(i.firstSeen)),
		Latest:    map[string]map[string]string{},
	}
	for k, t := range i.firstSeen {
		c.FirstSeen[k] = t.UTC().Format(time.RFC3339)
	}
	for _, s := range i.srcs {
		if s.latest == nil {
			continue
		}
		vers := make(map[string]string, len(s.latest))
		for name, u := range s.latest {
			vers[name] = u.Latest
		}
		c.Latest[s.p.Name()] = vers
	}
	i.mu.Unlock()

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = writeAtomic(i.cachePath, data)
}

// writeAtomic writes data to a temp file in path's directory, then renames
// it over path.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".cache-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// ScanAll lists every available provider in parallel.
func (i *Inventory) ScanAll(ctx context.Context) {
	timeout := ListTimeout
	var wg sync.WaitGroup
	for _, s := range i.srcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = i.scan(ctx, s, timeout)
		}()
	}
	wg.Wait()
}

// Scan lists one provider again. A List failure is returned and also shown
// in that provider's status.
func (i *Inventory) Scan(ctx context.Context, name string) error {
	s, ok := i.byName[name]
	if !ok {
		return fmt.Errorf("unknown source %q", name)
	}
	return i.scan(ctx, s, ListTimeout)
}

func (i *Inventory) scan(ctx context.Context, s *source, timeout time.Duration) error {
	name := s.p.Name()
	avail := s.p.Available()

	i.mu.Lock()
	if !avail {
		s.available = false
		s.tools, s.listErr, s.outErr = nil, "", ""
		i.notifyLocked()
		i.mu.Unlock()
		return fmt.Errorf("%s is not installed", name)
	}
	s.available = true
	s.listSeq++
	seq := s.listSeq
	i.mu.Unlock()

	tools, err := call(ctx, timeout, s.p.List)
	if ctx.Err() != nil {
		// The caller gave up (e.g. shutdown); keep what we had.
		return ctx.Err()
	}
	err = describe(name, err)

	i.mu.Lock()
	if seq < s.listApplied {
		// A scan that started later has already been applied.
		i.mu.Unlock()
		return err
	}
	s.listApplied = seq
	s.lastScan = time.Now()
	changed := false
	if err != nil {
		s.tools, s.listErr = nil, err.Error()
	} else {
		s.listErr = ""
		s.tools = make([]provider.Tool, len(tools))
		now := time.Now()
		for k, t := range tools {
			t.Source = name
			s.tools[k] = t
			key := name + "/" + t.Name
			if _, ok := i.firstSeen[key]; ok {
				continue
			}
			seen := now
			if i.firstRun && !s.seeded {
				seen = time.Time{}
			}
			i.firstSeen[key] = seen
			changed = true
		}
		s.seeded = true
	}
	i.notifyLocked()
	i.mu.Unlock()

	if changed {
		i.save()
	}
	return err
}

// CheckOutdated asks one provider, or every available provider when name is
// "", for updates, in parallel. Results are kept and reapplied after every
// Scan. A failure is shown in that provider's status; its tools stay listed.
func (i *Inventory) CheckOutdated(ctx context.Context, name string) {
	targets := i.srcs
	if name != "" {
		s, ok := i.byName[name]
		if !ok {
			return
		}
		targets = []*source{s}
	}
	timeout := OutdatedTimeout
	var wg sync.WaitGroup
	for _, s := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i.checkOutdated(ctx, s, timeout)
		}()
	}
	wg.Wait()
}

func (i *Inventory) checkOutdated(ctx context.Context, s *source, timeout time.Duration) {
	name := s.p.Name()
	avail := s.p.Available()

	i.mu.Lock()
	s.available = avail
	if !avail {
		i.notifyLocked()
		i.mu.Unlock()
		return
	}
	s.checking++
	s.outSeq++
	seq := s.outSeq
	i.notifyLocked()
	i.mu.Unlock()

	ups, err := call(ctx, timeout, s.p.Outdated)
	err = describe(name, err)

	i.mu.Lock()
	s.checking--
	changed := false
	if ctx.Err() == nil && seq > s.outApplied {
		s.outApplied = seq
		if err != nil {
			s.outErr = err.Error()
		} else {
			s.outErr = ""
			s.latest = make(map[string]provider.Update, len(ups))
			for _, u := range ups {
				s.latest[u.Name] = u
			}
			changed = true
		}
	}
	i.notifyLocked()
	i.mu.Unlock()

	if changed {
		i.save()
	}
}

// call runs fn with its own timeout and returns as soon as the timeout
// expires, even if fn ignores its context.
func call[T any](ctx context.Context, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn(ctx)
		ch <- result{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		select {
		case r := <-ch:
			return r.v, r.err
		default:
		}
		var zero T
		return zero, ctx.Err()
	}
}

// describe turns a timeout into a plain-English error.
func describe(name string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s took too long to respond", name)
	}
	return err
}

// Snapshot returns the current state. Tools are sorted by Source, then Name;
// providers are in the order given to New.
func (i *Inventory) Snapshot() Snapshot {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := time.Now()
	snap := Snapshot{
		Tools:     []provider.Tool{},
		Providers: make([]ProviderStatus, 0, len(i.srcs)),
	}
	for _, s := range i.srcs {
		st := ProviderStatus{
			Name:      s.p.Name(),
			Available: s.available,
			Error:     s.listErr,
			Checking:  s.checking > 0,
			LastScan:  s.lastScan,
		}
		if st.Error == "" {
			st.Error = s.outErr
		}
		snap.Providers = append(snap.Providers, st)
		snap.CheckingUpdates = snap.CheckingUpdates || st.Checking

		for _, t := range s.tools {
			if u, ok := s.latest[t.Name]; ok {
				t.Latest = u.Latest
				if t.Description == "" {
					t.Description = u.Description
				}
				if t.Homepage == "" {
					t.Homepage = u.Homepage
				}
			}
			t.Outdated = t.Latest != "" && t.Latest != t.Version
			seen := i.firstSeen[t.Source+"/"+t.Name]
			t.New = !seen.IsZero() && now.Sub(seen) < newFor
			snap.Tools = append(snap.Tools, t)
		}
	}
	sort.Slice(snap.Tools, func(a, b int) bool {
		ta, tb := snap.Tools[a], snap.Tools[b]
		if ta.Source != tb.Source {
			return ta.Source < tb.Source
		}
		return ta.Name < tb.Name
	})
	return snap
}

// Provider returns the provider with the given name.
func (i *Inventory) Provider(name string) (provider.Provider, bool) {
	s, ok := i.byName[name]
	if !ok {
		return nil, false
	}
	return s.p, true
}

// Subscribe returns a channel that receives a value after any change. The
// channel holds at most one pending notification; sends never block. The
// returned func unsubscribes and closes the channel; it is safe to call more
// than once.
func (i *Inventory) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	i.mu.Lock()
	id := i.nextSub
	i.nextSub++
	i.subs[id] = ch
	i.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			i.mu.Lock()
			delete(i.subs, id)
			close(ch)
			i.mu.Unlock()
		})
	}
}

func (i *Inventory) notifyLocked() {
	for _, ch := range i.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
