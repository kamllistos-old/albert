package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

const cargoListKey = "cargo install --list"

// cargoRegistry serves crates.io JSON for the given name -> latest map and
// 404s for other crates.
func cargoRegistry(t *testing.T, latest map[string]string) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/api/v1/crates/")
		v, ok := latest[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"crate": map[string]any{
			"name": name, "max_stable_version": v, "description": name + " summary",
			"homepage": "https://example.com/" + name,
		}})
	}))
	t.Cleanup(srv.Close)
	return &registry.Client{HTTP: srv.Client(), CratesURL: srv.URL}
}

func cargoFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		cargoListKey:            {Stdout: fixture(t, "cargo_list.txt")},
		"cargo install bat":     {},
		"cargo install fd-find": {},
		"cargo install ripgrep": {},
	}}
}

func TestCargoList(t *testing.T) {
	tools, err := NewCargo(cargoFake(t), nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2 (indented binary lines skipped): %+v", len(tools), tools)
	}
	if tools[0].Name != "ripgrep" || tools[0].Version != "14.1.0" || tools[0].Source != "cargo" {
		t.Errorf("tools[0] = %+v", tools[0])
	}
	if tools[1].Name != "bat" || tools[1].Version != "0.24.0" {
		t.Errorf("tools[1] = %+v", tools[1])
	}
}

func TestCargoOutdated(t *testing.T) {
	reg := cargoRegistry(t, map[string]string{"ripgrep": "14.2.0", "bat": "0.24.0"})
	ups, err := NewCargo(cargoFake(t), reg).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Update{{Name: "ripgrep", Latest: "14.2.0", Description: "ripgrep summary", Homepage: "https://example.com/ripgrep"}}
	if !reflect.DeepEqual(ups, want) {
		t.Errorf("Outdated = %+v, want %+v", ups, want)
	}
}

func TestCargoOutdatedRegistryOutageIsAnError(t *testing.T) {
	ups, err := NewCargo(cargoFake(t), outageRegistry(t)).Outdated(context.Background())
	requireAllFailed(t, ups, err)
}

func TestCargoOutdatedNeedsRegistry(t *testing.T) {
	if _, err := NewCargo(cargoFake(t), nil).Outdated(context.Background()); err == nil {
		t.Error("want error without a registry client")
	}
}

func TestCargoUpgradeNamed(t *testing.T) {
	f := cargoFake(t)
	var out bytes.Buffer
	if err := NewCargo(f, nil).Upgrade(context.Background(), []string{"ripgrep", "fd-find"}, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{cargoListKey, "cargo install ripgrep", "cargo install fd-find"}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Errorf("Calls = %q, want %q", f.Calls, want)
	}
}

func TestCargoUpgradeAllUsesOutdated(t *testing.T) {
	f := cargoFake(t)
	reg := cargoRegistry(t, map[string]string{"ripgrep": "14.2.0", "bat": "0.24.0"})
	if err := NewCargo(f, reg).Upgrade(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{cargoListKey, "cargo install ripgrep"}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Errorf("Calls = %q, want %q", f.Calls, want)
	}
}

func TestCargoUpgradeAllChecksDerivedNames(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		cargoListKey: {Stdout: []byte("--force v1.0.0:\n    x\n")},
	}}
	reg := cargoRegistry(t, map[string]string{"--force": "2.0.0"})
	err := NewCargo(f, reg).Upgrade(context.Background(), nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("err = %v, want invalid name", err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "cargo install ") && c != cargoListKey {
			t.Errorf("ran %q", c)
		}
	}
}

func TestCargoUpgradeContinuesAfterFailure(t *testing.T) {
	f := cargoFake(t)
	f.Responses["cargo install ripgrep"] = runner.Result{ExitCode: 101, Stderr: []byte("error: boom\n")}
	err := NewCargo(f, nil).Upgrade(context.Background(), []string{"ripgrep", "fd-find"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "exit code 101") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(f.Calls, []string{cargoListKey, "cargo install ripgrep", "cargo install fd-find"}) {
		t.Errorf("Calls = %q, want fd-find still attempted", f.Calls)
	}
}

func TestCargoInstall(t *testing.T) {
	f := cargoFake(t)
	p := NewCargo(f, nil)
	if err := p.Install(context.Background(), "-bad", &bytes.Buffer{}); err == nil {
		t.Error("want error for flag-like name")
	}
	if len(f.Calls) != 0 {
		t.Errorf("ran %q", f.Calls)
	}
	if err := p.Install(context.Background(), "bat", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"cargo install bat"}) {
		t.Errorf("Calls = %q", f.Calls)
	}
}

func TestCargoSearchUnsupported(t *testing.T) {
	if _, err := NewCargo(cargoFake(t), nil).Search(context.Background(), "bat"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestCargoWatchPaths(t *testing.T) {
	p := NewCargo(&runner.Fake{}, nil)
	t.Setenv("CARGO_HOME", "/custom/cargo")
	if got := p.WatchPaths(); !reflect.DeepEqual(got, []string{"/custom/cargo/bin"}) {
		t.Errorf("WatchPaths = %q", got)
	}
	t.Setenv("CARGO_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := p.WatchPaths(); !reflect.DeepEqual(got, []string{filepath.Join(home, ".cargo", "bin")}) {
		t.Errorf("WatchPaths = %q", got)
	}
}

func TestCargoAvailable(t *testing.T) {
	f := &runner.Fake{Missing: map[string]bool{"cargo": true}}
	if p := NewCargo(f, nil); p.Name() != "cargo" || p.Available() {
		t.Error("Name/Available wrong")
	}
}

func TestCargoSourceInstalledCratesAreNotRegistryCrates(t *testing.T) {
	// crates.io has a different "bat" (0.99.0) than the one built from /Users/x/src/bat.
	reg := cargoRegistry(t, map[string]string{"ripgrep": "14.2.0", "bat": "0.99.0"})
	f := cargoFake(t)
	p := NewCargo(f, reg)

	ups, err := p.Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range ups {
		if u.Name == "bat" {
			t.Errorf("Outdated reports source-installed bat: %+v", u)
		}
	}
	if len(ups) != 1 || ups[0].Name != "ripgrep" {
		t.Errorf("Outdated = %+v, want only ripgrep", ups)
	}

	f.Calls = nil
	if err := p.Upgrade(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{cargoListKey, "cargo install ripgrep"}; !reflect.DeepEqual(f.Calls, want) {
		t.Errorf("upgrade-all Calls = %q, want %q", f.Calls, want)
	}

	f.Calls = nil
	err = p.Upgrade(context.Background(), []string{"bat"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "installed from a local path or git") {
		t.Fatalf("explicit Upgrade(bat) err = %v", err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "cargo install ") && c != cargoListKey {
			t.Errorf("ran %q", c)
		}
	}
	// Source-installed crates are still listed.
	tools, _ := p.List(context.Background())
	if len(tools) != 2 {
		t.Errorf("List = %+v, want both crates", tools)
	}
}
