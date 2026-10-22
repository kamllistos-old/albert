package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

const (
	uvListKey      = "uv tool list --show-paths"
	pipxListKey    = "pipx list --json"
	pipListKey     = "pip3 list --format=json"
	pipListUserKey = "pip3 list --user --format=json"
	pipOutdatedKey = "pip3 list --outdated --format=json"
)

// pyRegistry serves PyPI JSON for the given name -> latest version map and
// 404s for any other package.
func pyRegistry(t *testing.T, latest map[string]string) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/pypi/"), "/json")
		v, ok := latest[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"info": map[string]any{
			"version":   v,
			"summary":   name + " summary",
			"home_page": "https://example.com/" + name,
		}})
	}))
	t.Cleanup(srv.Close)
	return &registry.Client{HTTP: srv.Client(), PyPIURL: srv.URL}
}

func uvFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		uvListKey:               {Stdout: fixture(t, "uv_list.txt")},
		"uv tool dir":           {Stdout: []byte("/Users/x/.local/share/uv/tools\n")},
		"uv tool upgrade --all": {Stdout: []byte("ok\n")},
	}}
}

func pipxFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		pipxListKey: {Stdout: fixture(t, "pipx_list.json")},
		"pipx environment --value PIPX_LOCAL_VENVS": {Stdout: []byte("/Users/x/.local/pipx/venvs\n")},
		"pipx upgrade-all":                          {},
		"pipx upgrade black":                        {},
	}}
}

func pipFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		pipListKey:     {Stdout: fixture(t, "pip_list.json"), Stderr: []byte("WARNING: noise\n")},
		pipListUserKey: {Stdout: fixture(t, "pip_list_user.json")},
		pipOutdatedKey: {Stdout: fixture(t, "pip_outdated.json")},
	}}
}

func TestUVList(t *testing.T) {
	tools, err := NewUV(uvFake(t), nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2 (binary lines skipped): %+v", len(tools), tools)
	}
	c := tools[0]
	if c.Name != "crewai" || c.Version != "1.14.2" || c.Source != "uv" {
		t.Errorf("crewai = %+v", c)
	}
	if c.Path != "/Users/camtoando/.local/share/uv/tools/crewai" {
		t.Errorf("Path = %q", c.Path)
	}
	if tools[1].Name != "manim" || tools[1].Version != "0.21.0" {
		t.Errorf("manim = %+v", tools[1])
	}
}

func TestUVListError(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		uvListKey: {ExitCode: 2, Stderr: []byte("boom\nmore\n")},
	}}
	_, err := NewUV(f, nil).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want first stderr line", err)
	}
}

func TestUVOutdatedOnlyWhenDifferent(t *testing.T) {
	reg := pyRegistry(t, map[string]string{"crewai": "1.15.0", "manim": "0.21.0"})
	ups, err := NewUV(uvFake(t), reg).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 {
		t.Fatalf("got %d updates, want 1: %+v", len(ups), ups)
	}
	u := ups[0]
	if u.Name != "crewai" || u.Latest != "1.15.0" || u.Description != "crewai summary" || u.Homepage != "https://example.com/crewai" {
		t.Errorf("update = %+v", u)
	}
}

func TestUVOutdatedSkipsLookupFailures(t *testing.T) {
	// manim is unknown to the registry (404): skipped, not an error.
	reg := pyRegistry(t, map[string]string{"crewai": "1.15.0"})
	ups, err := NewUV(uvFake(t), reg).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Name != "crewai" {
		t.Fatalf("updates = %+v", ups)
	}
}

func TestUVUpgradeAll(t *testing.T) {
	f := uvFake(t)
	var out strings.Builder
	if err := NewUV(f, nil).Upgrade(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls; len(got) != 1 || got[0] != "uv tool upgrade --all" {
		t.Errorf("calls = %q", got)
	}
}

func TestUVUpgradeNamesAndInstall(t *testing.T) {
	f := uvFake(t)
	f.Responses["uv tool upgrade crewai manim"] = runner.Result{}
	f.Responses["uv tool install cowsay"] = runner.Result{}
	p := NewUV(f, nil)
	if err := p.Upgrade(context.Background(), []string{"crewai", "manim"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Install(context.Background(), "cowsay", &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"uv tool upgrade crewai manim", "uv tool install cowsay"}
	if strings.Join(f.Calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", f.Calls, want)
	}
}

func TestPythonRejectsBadNames(t *testing.T) {
	for _, p := range []struct {
		name string
		p    Provider
	}{
		{"uv", NewUV(uvFake(t), nil)},
		{"pipx", NewPipx(pipxFake(t), nil)},
		{"pip", NewPip(pipFake(t))},
	} {
		if err := p.p.Upgrade(context.Background(), []string{"--all"}, &strings.Builder{}); err == nil {
			t.Errorf("%s: Upgrade accepted flag-like name", p.name)
		}
		if p.name == "pip" {
			continue // pip Install is unsupported regardless
		}
		if err := p.p.Install(context.Background(), "-e", &strings.Builder{}); err == nil {
			t.Errorf("%s: Install accepted flag-like name", p.name)
		}
	}
}

func TestUVSearch(t *testing.T) {
	reg := pyRegistry(t, map[string]string{"cowsay": "6.1"})
	p := NewUV(uvFake(t), reg)
	hits, err := p.Search(context.Background(), "cowsay")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Name != "cowsay" || hits[0].Version != "6.1" || hits[0].Source != "uv" || hits[0].Description != "cowsay summary" {
		t.Fatalf("hits = %+v", hits)
	}
	hits, err = p.Search(context.Background(), "nosuchpkg")
	if err != nil || len(hits) != 0 {
		t.Fatalf("no match: hits=%+v err=%v, want empty and nil", hits, err)
	}
}

func TestUVWatchPaths(t *testing.T) {
	got := NewUV(uvFake(t), nil).WatchPaths()
	if len(got) != 1 || got[0] != "/Users/x/.local/share/uv/tools" {
		t.Errorf("WatchPaths = %q", got)
	}
	if got := NewUV(&runner.Fake{}, nil).WatchPaths(); len(got) != 0 {
		t.Errorf("WatchPaths on failure = %q, want none", got)
	}
}

func TestPipxList(t *testing.T) {
	tools, err := NewPipx(pipxFake(t), nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "black" || tools[0].Version != "24.1.0" || tools[0].Source != "pipx" {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestPipxOutdated(t *testing.T) {
	reg := pyRegistry(t, map[string]string{"black": "24.2.0"})
	ups, err := NewPipx(pipxFake(t), reg).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Name != "black" || ups[0].Latest != "24.2.0" || ups[0].Homepage != "https://example.com/black" {
		t.Fatalf("updates = %+v", ups)
	}
}

func TestPipxUpgradeAll(t *testing.T) {
	f := pipxFake(t)
	if err := NewPipx(f, nil).Upgrade(context.Background(), nil, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 || f.Calls[0] != "pipx upgrade-all" {
		t.Errorf("calls = %q", f.Calls)
	}
}

func TestPipxUpgradeNamesOneAtATime(t *testing.T) {
	f := pipxFake(t)
	f.Responses["pipx upgrade httpie"] = runner.Result{ExitCode: 1, Stderr: []byte("no such venv\n")}
	err := NewPipx(f, nil).Upgrade(context.Background(), []string{"black", "httpie", "ruff"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "pipx: exit code 1") {
		t.Fatalf("err = %v", err)
	}
	want := "pipx upgrade black|pipx upgrade httpie"
	if got := strings.Join(f.Calls, "|"); got != want {
		t.Errorf("calls = %q, want %q (stop at first failure)", got, want)
	}
}

func TestPipxInstallAndWatch(t *testing.T) {
	f := pipxFake(t)
	f.Responses["pipx install black"] = runner.Result{}
	p := NewPipx(f, nil)
	if err := p.Install(context.Background(), "black", &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if got := p.WatchPaths(); len(got) != 1 || got[0] != "/Users/x/.local/pipx/venvs" {
		t.Errorf("WatchPaths = %q", got)
	}
}

func TestPipReadOnlyMarking(t *testing.T) {
	tools, err := NewPip(pipFake(t)).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3: %+v", len(tools), tools)
	}
	byName := map[string]Tool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
		if tl.Source != "pip" {
			t.Errorf("%s source = %q", tl.Name, tl.Source)
		}
	}
	if byName["numpy"].ReadOnly {
		t.Error("numpy is in the --user list, ReadOnly = true")
	}
	if !byName["pip"].ReadOnly || !byName["certifi"].ReadOnly {
		t.Errorf("system packages must be ReadOnly: %+v", byName)
	}
	if byName["numpy"].Version != "2.5.2" {
		t.Errorf("numpy version = %q", byName["numpy"].Version)
	}
}

func TestPipOutdated(t *testing.T) {
	ups, err := NewPip(pipFake(t)).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 3 || ups[1].Name != "numpy" || ups[1].Latest != "2.5.4" {
		t.Fatalf("updates = %+v", ups)
	}
}

func TestPipUpgradeRejectsReadOnly(t *testing.T) {
	f := pipFake(t)
	err := NewPip(f).Upgrade(context.Background(), []string{"numpy", "certifi"}, &strings.Builder{})
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("err = %v, want ErrReadOnly", err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "pip3 install") {
			t.Errorf("install ran despite read-only name: %q", f.Calls)
		}
	}
}

func TestPipUpgradeUserTools(t *testing.T) {
	f := pipFake(t)
	f.Responses["pip3 install --user --upgrade NumPy"] = runner.Result{}
	if err := NewPip(f).Upgrade(context.Background(), []string{"NumPy"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if last := f.Calls[len(f.Calls)-1]; last != "pip3 install --user --upgrade NumPy" {
		t.Errorf("last call = %q", last)
	}
}

func TestPipUpgradeAllUsesOnlyUserOutdated(t *testing.T) {
	f := pipFake(t)
	f.Responses["pip3 install --user --upgrade numpy"] = runner.Result{}
	if err := NewPip(f).Upgrade(context.Background(), nil, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if last := f.Calls[len(f.Calls)-1]; last != "pip3 install --user --upgrade numpy" {
		t.Errorf("last call = %q, want only the non-read-only outdated tool", last)
	}
}

func TestPipUpgradeAllNothingToDo(t *testing.T) {
	f := pipFake(t)
	f.Responses[pipListUserKey] = runner.Result{Stdout: []byte("[]")}
	if err := NewPip(f).Upgrade(context.Background(), nil, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "pip3 install") {
			t.Errorf("install ran with nothing to upgrade: %q", f.Calls)
		}
	}
}

func TestPipUpgradeUnknownName(t *testing.T) {
	f := pipFake(t)
	err := NewPip(f).Upgrade(context.Background(), []string{"ghost"}, &strings.Builder{})
	if err == nil || errors.Is(err, ErrReadOnly) {
		t.Fatalf("err = %v, want a not-installed error", err)
	}
}

func TestPipExternallyManagedMessage(t *testing.T) {
	f := pipFake(t)
	f.Responses["pip3 install --user --upgrade numpy"] = runner.Result{
		ExitCode: 1,
		Stderr:   []byte("error: externally-managed-environment\n\n× This environment is externally managed\n"),
	}
	err := NewPip(f).Upgrade(context.Background(), []string{"numpy"}, &strings.Builder{})
	want := "pip refused: this Python is managed by Homebrew. Use uv or pipx for tools."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestPipUpgradeFailure(t *testing.T) {
	f := pipFake(t)
	f.Responses["pip3 install --user --upgrade numpy"] = runner.Result{ExitCode: 1, Stderr: []byte("ERROR: no network\n")}
	err := NewPip(f).Upgrade(context.Background(), []string{"numpy"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "pip3: exit code 1") {
		t.Fatalf("err = %v", err)
	}
}

func TestPipInstallUnsupported(t *testing.T) {
	p := NewPip(pipFake(t))
	if err := p.Install(context.Background(), "requests", &strings.Builder{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Install err = %v, want ErrUnsupported", err)
	}
	if _, err := p.Search(context.Background(), "requests"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Search err = %v, want ErrUnsupported", err)
	}
}

func TestPipAvailableAndWatchPaths(t *testing.T) {
	f := pipFake(t)
	f.Responses["python3 -m site --user-site"] = runner.Result{Stdout: []byte("/Users/x/Library/Python/3.14/lib/python/site-packages\n")}
	p := NewPip(f)
	if !p.Available() || p.Name() != "pip" {
		t.Error("pip should be available and named pip")
	}
	if got := p.WatchPaths(); len(got) != 1 || got[0] != "/Users/x/Library/Python/3.14/lib/python/site-packages" {
		t.Errorf("WatchPaths = %q", got)
	}
	f.Missing = map[string]bool{"pip3": true}
	if p.Available() {
		t.Error("Available with pip3 missing")
	}
}

func TestPythonDefaults(t *testing.T) {
	got := strings.Join(names(Defaults(&runner.Fake{}, &registry.Client{})), ",")
	if !strings.HasPrefix(got, "brew,cask,uv,pipx,pip") {
		t.Errorf("Defaults order = %s", got)
	}
}
