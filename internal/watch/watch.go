// Package watch turns bursts of filesystem events in package-manager
// directories into a single debounced notification per source.
package watch

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// fired identifies a debounce timer expiry; gen discards stale expiries from
// timers that were reset after they had already started firing.
type fired struct {
	source string
	gen    uint64
}

// Watch watches the top-level entries of each directory in paths (source ->
// dirs) and calls onChange(source) once, debounce after the last event in a
// burst. A directory listed under several sources notifies all of them.
// Missing directories, empty strings and duplicates are skipped silently.
// Watch blocks until ctx is done; onChange is only ever called from the
// calling goroutine and never after Watch returns. It returns an error only
// if the underlying watcher cannot be created.
func Watch(ctx context.Context, paths map[string][]string, debounce time.Duration, onChange func(source string)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	// dir -> sources interested in it (deduplicated).
	byDir := map[string][]string{}
	for source, dirs := range paths {
		for _, d := range dirs {
			if d == "" {
				continue
			}
			d = filepath.Clean(d)
			if !contains(byDir[d], source) {
				byDir[d] = append(byDir[d], source)
			}
		}
	}
	for d := range byDir {
		if err := w.Add(d); err != nil {
			delete(byDir, d) // missing or unreadable: skip silently
		}
	}

	timers := map[string]*time.Timer{}
	gens := map[string]uint64{}
	expired := make(chan fired)
	done := make(chan struct{})
	defer func() {
		close(done) // releases any timer callback blocked sending to expired
		for _, t := range timers {
			t.Stop()
		}
	}()

	arm := func(source string) {
		gens[source]++
		f := fired{source, gens[source]}
		send := func() {
			select {
			case expired <- f:
			case <-done:
			}
		}
		if t, ok := timers[source]; ok {
			t.Stop()
		}
		timers[source] = time.AfterFunc(debounce, send)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if ev.Op == fsnotify.Chmod {
				continue // attribute-only changes are noise
			}
			for _, s := range byDir[filepath.Dir(ev.Name)] {
				arm(s)
			}
		case <-w.Errors:
			// Overflows and transient errors: nothing actionable; keep watching.
		case f := <-expired:
			if f.gen != gens[f.source] {
				continue // superseded by a later event
			}
			if ctx.Err() != nil {
				return nil
			}
			onChange(f.source)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
