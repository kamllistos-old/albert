package agent

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/kamllistos/albert/internal/provider"
)

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func useProviders(t *testing.T, ps ...provider.Provider) {
	t.Helper()
	old := newProviders
	newProviders = func() []provider.Provider { return ps }
	t.Cleanup(func() { newProviders = old })
}

func writeToken(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func get(t *testing.T, port int, path, token string) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return http.DefaultClient.Do(req)
}

func TestRunServesAndShutsDown(t *testing.T) {
	brew := &fakeProvider{name: "brew",
		tools:   []provider.Tool{{Name: "jq", Version: "1.6"}},
		updates: []provider.Update{{Name: "jq", Latest: "1.7"}}}
	useProviders(t, brew)
	dir := t.TempDir()
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Port: port, UIPort: freePort(t), TokenFile: writeToken(t, dir, testToken+"\n"),
			StateDir: dir, Mode: "docker", Version: "9.9.9"})
	}()

	eventually(t, "agent to answer", func() bool {
		resp, err := get(t, port, "/api/health", testToken)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	resp, err := get(t, port, "/api/health", "wrong")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: status %d, want 401", resp.StatusCode)
	}

	// Startup scans and checks for updates in the background, and writes
	// the cache to the state dir.
	eventually(t, "initial scan and outdated check", func() bool {
		resp, err := get(t, port, "/api/tools", testToken)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return strings.Contains(string(body), `"latest":"1.7"`)
	})
	if _, err := os.Stat(filepath.Join(dir, "cache.json")); err != nil {
		t.Fatalf("cache not written: %v", err)
	}

	// An open event stream must not hold up shutdown.
	events, err := get(t, port, "/api/events", testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer events.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil after cancel", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if _, err := get(t, port, "/api/health", testToken); err == nil {
		t.Fatal("agent still answering after shutdown")
	}
}

func TestRunNativeServesUI(t *testing.T) {
	useProviders(t)
	dir := t.TempDir()
	port := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, RunOptions{Port: port, UIPort: port, TokenFile: writeToken(t, dir, testToken),
			StateDir: dir, Mode: "native", UI: fstest.MapFS{"index.html": {Data: []byte("hi")}}})
	}()
	defer func() {
		cancel()
		<-done
	}()
	eventually(t, "UI to be served", func() bool {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/", port))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK && slices.ContainsFunc(resp.Cookies(), func(c *http.Cookie) bool {
			return c.Name == "albert_token" && c.Value == testToken
		})
	})
}

func TestRunRejectsBadOptions(t *testing.T) {
	useProviders(t)
	dir := t.TempDir()
	good := writeToken(t, dir, testToken)
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte(" \n\t"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]RunOptions{
		"empty token":   {Port: freePort(t), TokenFile: empty, StateDir: dir, Mode: "docker"},
		"missing token": {Port: freePort(t), TokenFile: filepath.Join(dir, "nope"), StateDir: dir, Mode: "docker"},
		"bad mode":      {Port: freePort(t), TokenFile: good, StateDir: dir, Mode: "cloud"},
		"native no UI":  {Port: freePort(t), TokenFile: good, StateDir: dir, Mode: "native"},
		"no state dir":  {Port: freePort(t), TokenFile: good, Mode: "docker"},
	}
	for name, o := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := Run(ctx, o); err == nil {
				t.Fatal("Run = nil, want an error")
			}
		})
	}
}

func TestRunPortBusy(t *testing.T) {
	useProviders(t)
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Run(ctx, RunOptions{Port: port, UIPort: port + 1, TokenFile: writeToken(t, dir, testToken), StateDir: dir, Mode: "docker"}); err == nil {
		t.Fatal("Run = nil on a busy port, want an error")
	}
}

func TestAllowedHostsAndOrigins(t *testing.T) {
	if got, want := allowedHosts(7878, 7879), []string{"localhost:7879", "127.0.0.1:7879", "localhost:7878", "127.0.0.1:7878"}; !slices.Equal(got, want) {
		t.Errorf("allowedHosts = %v, want %v", got, want)
	}
	if got, want := allowedHosts(7878, 7878), []string{"localhost:7878", "127.0.0.1:7878"}; !slices.Equal(got, want) {
		t.Errorf("allowedHosts(native) = %v, want %v", got, want)
	}
	if got, want := uiOrigins(7879), []string{"http://localhost:7879", "http://127.0.0.1:7879"}; !slices.Equal(got, want) {
		t.Errorf("uiOrigins = %v, want %v", got, want)
	}
}
