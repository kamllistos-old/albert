package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

const (
	npmListKey  = "npm ls -g --json --depth=0"
	npmRootKey  = "npm root -g"
	pnpmListKey = "pnpm ls -g --json"
	bunListKey  = "bun pm ls -g"
)

// nodeRegistry serves npm registry JSON for the given name -> latest version
// map, 404s for other packages, and answers every search with two hits.
func nodeRegistry(t *testing.T, latest map[string]string) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/-/v1/search" {
			w.Write([]byte(`{"objects":[
				{"package":{"name":"eslint","version":"9.1.0","description":"linter"}},
				{"package":{"name":"jshint","version":"2.13.6","description":"hinter"}}]}`))
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/latest")
		v, ok := latest[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"name":        name,
			"version":     v,
			"description": name + " summary",
			"homepage":    "https://example.com/" + name,
		})
	}))
	t.Cleanup(srv.Close)
	return &registry.Client{HTTP: srv.Client(), NPMURL: srv.URL}
}

func npmFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		npmListKey: {Stdout: fixture(t, "npm_ls.json")},
		npmRootKey: {Stdout: []byte("/opt/homebrew/lib/node_modules\n")},
		"npm install -g azure-functions-core-tools@latest": {},
	}}
}

func pnpmFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		pnpmListKey:    {Stdout: fixture(t, "pnpm_ls.json")},
		"pnpm root -g": {Stdout: []byte("/Users/x/Library/pnpm/global/5/node_modules\n")},
	}}
}

func bunFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		bunListKey: {Stdout: fixture(t, "bun_ls.txt")},
	}}
}

func TestNPMList(t *testing.T) {
	tools, err := NewNPM(npmFake(t), nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3: %+v", len(tools), tools)
	}
	// Sorted by name.
	a := tools[0]
	if a.Name != "azure-functions-core-tools" || a.Version != "4.12.1" || a.Source != "npm" {
		t.Errorf("azure = %+v", a)
	}
	if a.Path != "/opt/homebrew/lib/node_modules/azure-functions-core-tools" {
		t.Errorf("Path = %q", a.Path)
	}
	if tools[1].Name != "npm" || tools[2].Name != "typescript" || tools[2].Version != "7.0.2" {
		t.Errorf("tools = %+v", tools)
	}
}

func TestNPMListToleratesNonZeroExitWithJSON(t *testing.T) {
	// npm ls exits 1 for e.g. missing peer dependencies but still prints JSON.
	f := npmFake(t)
	f.Responses[npmListKey] = runner.Result{ExitCode: 1, Stdout: fixture(t, "npm_ls.json"), Stderr: []byte("npm error missing peer\n")}
	tools, err := NewNPM(f, nil).List(context.Background())
	if err != nil || len(tools) != 3 {
		t.Fatalf("tools=%d err=%v", len(tools), err)
	}
}

func TestNPMListErrorWithoutJSON(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		npmListKey: {ExitCode: 1, Stderr: []byte("npm error boom\nmore\n")},
	}}
	_, err := NewNPM(f, nil).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want first stderr line", err)
	}
}

func TestNPMListWithoutRootStillLists(t *testing.T) {
	f := npmFake(t)
	delete(f.Responses, npmRootKey)
	tools, err := NewNPM(f, nil).List(context.Background())
	if err != nil || len(tools) != 3 {
		t.Fatalf("tools=%d err=%v", len(tools), err)
	}
	if tools[0].Path != "" {
		t.Errorf("Path = %q, want empty", tools[0].Path)
	}
}

func TestNPMOutdatedFillsDescription(t *testing.T) {
	reg := nodeRegistry(t, map[string]string{
		"azure-functions-core-tools": "4.13.0",
		"npm":                        "11.19.0", // same: not outdated
		// typescript unknown (404): skipped
	})
	ups, err := NewNPM(npmFake(t), reg).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 {
		t.Fatalf("got %d updates, want 1: %+v", len(ups), ups)
	}
	u := ups[0]
	if u.Name != "azure-functions-core-tools" || u.Latest != "4.13.0" ||
		u.Description != "azure-functions-core-tools summary" || u.Homepage != "https://example.com/azure-functions-core-tools" {
		t.Errorf("update = %+v", u)
	}
}

func TestNPMOutdatedNeedsRegistry(t *testing.T) {
	if _, err := NewNPM(npmFake(t), nil).Outdated(context.Background()); err == nil {
		t.Fatal("want error without a registry client")
	}
}

func TestNPMOutdatedRegistryOutageIsAnError(t *testing.T) {
	ups, err := NewNPM(npmFake(t), outageRegistry(t)).Outdated(context.Background())
	requireAllFailed(t, ups, err)
}

func TestNPMUpgradeAllRegistryOutageFailsWithoutInstalling(t *testing.T) {
	f := npmFake(t)
	err := NewNPM(f, outageRegistry(t)).Upgrade(context.Background(), nil, &strings.Builder{})
	var le *LookupError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want the lookup failure reported", err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "npm install") {
			t.Errorf("unexpected %q", c)
		}
	}
}

func TestNPMUpgradeAllPartialFailureUpgradesFoundThenReports(t *testing.T) {
	// azure-functions-core-tools has an update; typescript's lookup fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "typescript"):
			http.Error(w, "down", http.StatusServiceUnavailable)
		case strings.Contains(r.URL.Path, "azure-functions-core-tools"):
			w.Write([]byte(`{"name":"azure-functions-core-tools","version":"4.13.0"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f := npmFake(t)
	err := NewNPM(f, &registry.Client{HTTP: srv.Client(), NPMURL: srv.URL}).Upgrade(context.Background(), nil, &strings.Builder{})
	if got := f.Calls[len(f.Calls)-1]; got != "npm install -g azure-functions-core-tools@latest" {
		t.Errorf("last call = %q, want the found update installed", got)
	}
	var le *LookupError
	if !errors.As(err, &le) || strings.Join(le.Failed, ",") != "typescript" {
		t.Errorf("err = %v, want the typescript lookup failure reported", err)
	}
}

func TestNPMUpgradeAllUsesOutdatedNames(t *testing.T) {
	f := npmFake(t)
	reg := nodeRegistry(t, map[string]string{"azure-functions-core-tools": "4.13.0", "npm": "11.19.0"})
	if err := NewNPM(f, reg).Upgrade(context.Background(), nil, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	want := "npm install -g azure-functions-core-tools@latest"
	if got := f.Calls[len(f.Calls)-1]; got != want {
		t.Errorf("last call = %q, want %q", got, want)
	}
}

func TestNPMUpgradeAllNothingOutdatedRunsNothing(t *testing.T) {
	f := npmFake(t)
	reg := nodeRegistry(t, nil)
	if err := NewNPM(f, reg).Upgrade(context.Background(), nil, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "npm install") {
			t.Errorf("unexpected %q", c)
		}
	}
}

func TestNPMUpgradeNamed(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"npm install -g typescript@latest @vercel/ncc@latest": {Stdout: []byte("done\n")},
	}}
	var out strings.Builder
	if err := NewNPM(f, nil).Upgrade(context.Background(), []string{"typescript", "@vercel/ncc"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "done\n" {
		t.Errorf("out = %q", out.String())
	}
}

func TestNodeRejectsBadNames(t *testing.T) {
	f := &runner.Fake{}
	for _, p := range []Provider{NewNPM(f, nil), NewPnpm(f, nil), NewBun(f, nil)} {
		if err := p.Upgrade(context.Background(), []string{"--evil"}, &strings.Builder{}); err == nil {
			t.Errorf("%s: Upgrade accepted flag-like name", p.Name())
		}
		if err := p.Install(context.Background(), "-g", &strings.Builder{}); err == nil {
			t.Errorf("%s: Install accepted flag-like name", p.Name())
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("ran %v", f.Calls)
	}
}

func TestNodeInstall(t *testing.T) {
	cases := []struct {
		p    func(runner.Runner) Provider
		want string
	}{
		{func(r runner.Runner) Provider { return NewNPM(r, nil) }, "npm install -g cowsay"},
		{func(r runner.Runner) Provider { return NewPnpm(r, nil) }, "pnpm add -g cowsay"},
		{func(r runner.Runner) Provider { return NewBun(r, nil) }, "bun add -g cowsay"},
	}
	for _, c := range cases {
		f := &runner.Fake{Responses: map[string]runner.Result{c.want: {}}}
		if err := c.p(f).Install(context.Background(), "cowsay", &strings.Builder{}); err != nil {
			t.Errorf("%s: %v", c.want, err)
		}
		if !reflect.DeepEqual(f.Calls, []string{c.want}) {
			t.Errorf("calls = %v, want %q", f.Calls, c.want)
		}
	}
}

func TestNPMWatchPaths(t *testing.T) {
	got := NewNPM(npmFake(t), nil).WatchPaths()
	if !reflect.DeepEqual(got, []string{"/opt/homebrew/lib/node_modules"}) {
		t.Errorf("WatchPaths = %v", got)
	}
	if got := NewNPM(&runner.Fake{}, nil).WatchPaths(); len(got) != 0 {
		t.Errorf("WatchPaths on failure = %v, want empty", got)
	}
}

func TestPnpmList(t *testing.T) {
	tools, err := NewPnpm(pnpmFake(t), nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools: %+v", len(tools), tools)
	}
	if t0 := tools[0]; t0.Name != "typescript" || t0.Version != "5.6.2" || t0.Source != "pnpm" {
		t.Errorf("tool = %+v", t0)
	}
}

func TestPnpmListEmpty(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{pnpmListKey: {Stdout: []byte("[]\n")}}}
	tools, err := NewPnpm(f, nil).List(context.Background())
	if err != nil || len(tools) != 0 {
		t.Fatalf("tools=%v err=%v", tools, err)
	}
}

func TestPnpmListErrorSurfacesReason(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		pnpmListKey: {ExitCode: 1, Stderr: []byte("[ERROR] The configured global bin directory \"/Users/x/Library/pnpm/bin\" is not in PATH\nmore\n")},
	}}
	_, err := NewPnpm(f, nil).List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "is not in PATH") {
		t.Fatalf("err = %v, want reason from stderr", err)
	}
	if strings.Contains(err.Error(), "more") {
		t.Errorf("err = %v, want only the first stderr line", err)
	}
}

func TestPnpmUpgradeAndWatch(t *testing.T) {
	f := pnpmFake(t)
	f.Responses["pnpm add -g typescript@latest"] = runner.Result{}
	p := NewPnpm(f, nil)
	if err := p.Upgrade(context.Background(), []string{"typescript"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls[len(f.Calls)-1]; got != "pnpm add -g typescript@latest" {
		t.Errorf("last call = %q", got)
	}
	if got := p.WatchPaths(); !reflect.DeepEqual(got, []string{"/Users/x/Library/pnpm/global/5/node_modules"}) {
		t.Errorf("WatchPaths = %v", got)
	}
	broken := &runner.Fake{Responses: map[string]runner.Result{"pnpm root -g": {ExitCode: 1, Stderr: []byte("[ERROR] nope\n")}}}
	if got := NewPnpm(broken, nil).WatchPaths(); len(got) != 0 {
		t.Errorf("WatchPaths on error = %v, want empty", got)
	}
}

func TestPnpmOutdated(t *testing.T) {
	reg := nodeRegistry(t, map[string]string{"typescript": "5.7.0"})
	ups, err := NewPnpm(pnpmFake(t), reg).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Name != "typescript" || ups[0].Latest != "5.7.0" {
		t.Errorf("ups = %+v", ups)
	}
}

func TestBunListScopedName(t *testing.T) {
	tools, err := NewBun(bunFake(t), nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools: %+v", len(tools), tools)
	}
	if tools[0].Name != "@anthropic-ai/sdk" || tools[0].Version != "0.30.1" || tools[0].Source != "bun" {
		t.Errorf("scoped = %+v", tools[0])
	}
	if tools[1].Name != "cowsay" || tools[1].Version != "1.6.0" {
		t.Errorf("cowsay = %+v", tools[1])
	}
}

func TestBunUpgradeAllUsesOutdatedNames(t *testing.T) {
	f := bunFake(t)
	f.Responses["bun add -g @anthropic-ai/sdk@latest"] = runner.Result{}
	reg := nodeRegistry(t, map[string]string{"@anthropic-ai/sdk": "0.31.0", "cowsay": "1.6.0"})
	if err := NewBun(f, reg).Upgrade(context.Background(), nil, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if got := f.Calls[len(f.Calls)-1]; got != "bun add -g @anthropic-ai/sdk@latest" {
		t.Errorf("last call = %q", got)
	}
}

func TestBunWatchPaths(t *testing.T) {
	t.Setenv("HOME", "/Users/x")
	got := NewBun(&runner.Fake{}, nil).WatchPaths()
	if !reflect.DeepEqual(got, []string{"/Users/x/.bun/install/global/node_modules"}) {
		t.Errorf("WatchPaths = %v", got)
	}
}

func TestNPMSearch(t *testing.T) {
	reg := nodeRegistry(t, nil)
	rs, err := NewNPM(&runner.Fake{}, reg).Search(context.Background(), "lint")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d results: %+v", len(rs), rs)
	}
	want := Result{Name: "eslint", Source: "npm", Version: "9.1.0", Description: "linter"}
	if rs[0] != want {
		t.Errorf("rs[0] = %+v, want %+v", rs[0], want)
	}
	// Each provider reports itself as the source.
	rs, err = NewBun(&runner.Fake{}, reg).Search(context.Background(), "lint")
	if err != nil || rs[0].Source != "bun" {
		t.Errorf("bun search = %+v err=%v", rs, err)
	}
}

func TestNPMSearchNeedsRegistry(t *testing.T) {
	if _, err := NewNPM(&runner.Fake{}, nil).Search(context.Background(), "lint"); err == nil {
		t.Fatal("want error without a registry client")
	}
}

func TestNodeAvailable(t *testing.T) {
	f := &runner.Fake{Missing: map[string]bool{"pnpm": true}}
	if !NewNPM(f, nil).Available() || NewPnpm(f, nil).Available() || !NewBun(f, nil).Available() {
		t.Error("Available should follow LookPath for npm, pnpm and bun")
	}
}
