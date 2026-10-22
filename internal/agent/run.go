package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kamllistos/albert/internal/inventory"
	"github.com/kamllistos/albert/internal/jobs"
	"github.com/kamllistos/albert/internal/provider"
	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
	"github.com/kamllistos/albert/internal/watch"
)

// RunOptions configures the agent process.
type RunOptions struct {
	Port, UIPort int
	TokenFile    string
	StateDir     string
	Mode         string // "docker" or "native"
	Version      string
	UI           fs.FS // the web UI; required in native mode, ignored in docker mode
}

const (
	watchDebounce   = 2 * time.Second
	outdatedEvery   = 6 * time.Hour
	shutdownTimeout = 5 * time.Second
)

// newProviders builds the real providers. Tests replace it.
var newProviders = func() []provider.Provider {
	return provider.Defaults(runner.Exec{}, registry.New())
}

// Run serves the API on 127.0.0.1:Port until ctx is done. In the background
// it scans every provider, checks for updates (again every 6h) and rescans a
// provider when its directories change. It returns nil after a clean
// shutdown.
func Run(ctx context.Context, o RunOptions) error {
	switch o.Mode {
	case "docker":
		o.UI = nil
	case "native":
		if o.UI == nil {
			return errors.New("native mode needs the web UI files")
		}
	default:
		return fmt.Errorf("unknown mode %q (want docker or native)", o.Mode)
	}
	if o.StateDir == "" {
		return errors.New("no state directory given")
	}
	token, err := readToken(o.TokenFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.Port)))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ps := newProviders()
	inv := inventory.New(ps, filepath.Join(o.StateDir, "cache.json"))
	jm := jobs.NewManager(inv)
	srv := newServer(ctx, Config{
		Token:        token,
		AllowedHosts: allowedHosts(o.Port, o.UIPort),
		UIOrigins:    uiOrigins(o.UIPort),
		UI:           o.UI,
		Version:      o.Version,
	}, inv, jm)
	hs := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		// Requests (event streams included) end when ctx is done, so
		// Shutdown does not wait on open streams.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		background(ctx, inv, ps)
	}()

	served := make(chan error, 1)
	go func() { served <- hs.Serve(ln) }()
	logf("albert agent %s listening on 127.0.0.1:%d (%s mode, UI port %d)", o.Version, o.Port, o.Mode, o.UIPort)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-served:
	}
	cancel()

	sctx, scancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer scancel()
	if err := hs.Shutdown(sctx); err != nil {
		hs.Close()
	}
	jm.Close()
	srv.wait()
	wg.Wait()
	logf("albert agent stopped")
	if errors.Is(runErr, http.ErrServerClosed) {
		runErr = nil
	}
	return runErr
}

// background does the initial scan and update check, then keeps the
// inventory fresh until ctx is done.
func background(ctx context.Context, inv *inventory.Inventory, ps []provider.Provider) {
	inv.ScanAll(ctx)
	if ctx.Err() != nil {
		return
	}

	// WatchPaths may run commands, so ask only after the first scan.
	paths := map[string][]string{}
	for _, p := range ps {
		if p.Available() {
			paths[p.Name()] = p.WatchPaths()
		}
	}
	var rescans coalescer
	defer rescans.wait()
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := watch.Watch(ctx, paths, watchDebounce, func(source string) {
			// Called on the watch loop: hand off and return at once.
			rescans.run(source, func() { _ = inv.Scan(ctx, source) })
		})
		if err != nil {
			logf("watching for changes is off: %v", err)
		}
	}()

	inv.CheckOutdated(ctx, "")
	tick := time.NewTicker(outdatedEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			inv.CheckOutdated(ctx, "")
		}
	}
}

// readToken reads the token file. Surrounding whitespace is ignored; an
// empty token is an error.
func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	return tok, nil
}

// allowedHosts is every Host the agent answers to: localhost and 127.0.0.1
// on the UI port (proxied by the container in docker mode) and on its own
// port.
func allowedHosts(port, uiPort int) []string {
	var hs []string
	for _, p := range []int{uiPort, port} {
		for _, h := range []string{"localhost", "127.0.0.1"} {
			hp := net.JoinHostPort(h, strconv.Itoa(p))
			if !slices.Contains(hs, hp) {
				hs = append(hs, hp)
			}
		}
	}
	return hs
}

// uiOrigins is the Origin a browser sends from the UI.
func uiOrigins(uiPort int) []string {
	p := strconv.Itoa(uiPort)
	return []string{"http://localhost:" + p, "http://127.0.0.1:" + p}
}
