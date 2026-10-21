package provider

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kamllistos/albert/internal/runner"
)

const (
	infoKey        = "brew info --json=v2 --installed"
	outdatedKey    = "brew outdated --formula --json=v2"
	outdatedCaskKy = "brew outdated --cask --greedy-auto-updates --json=v2"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func brewFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		"brew --prefix": {Stdout: []byte("/opt/homebrew\n")},
		infoKey:         {Stdout: fixture(t, "brew_info.json"), Stderr: []byte("==> progress noise\n")},
		outdatedKey:     {Stdout: fixture(t, "brew_outdated.json")},
		outdatedCaskKy:  {Stdout: fixture(t, "brew_outdated_cask.json")},
	}}
}

func TestBrewList(t *testing.T) {
	tools, err := NewBrew(brewFake(t)).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2: %+v", len(tools), tools)
	}
	argo, ada := tools[0], tools[1]
	if argo.Name != "argocd" || argo.Version != "3.5.6" || argo.Source != "brew" {
		t.Errorf("argocd = %+v", argo)
	}
	if argo.Description != "GitOps Continuous Delivery for Kubernetes" || argo.Homepage != "https://argoproj.github.io/cd/" {
		t.Errorf("argocd metadata = %+v", argo)
	}
	if argo.Path != "/opt/homebrew/Cellar/argocd" {
		t.Errorf("argocd path = %q", argo.Path)
	}
	if argo.Dependency {
		t.Error("argocd was installed on request, Dependency = true")
	}
	if ada.Name != "ada-url" || !ada.Dependency || ada.Path != "/opt/homebrew/Cellar/ada-url" {
		t.Errorf("ada-url = %+v", ada)
	}
}

func TestCaskList(t *testing.T) {
	tools, err := NewCask(brewFake(t)).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools, want 1: %+v", len(tools), tools)
	}
	c := tools[0]
	if c.Name != "claude-code" || c.Source != "cask" {
		t.Errorf("cask = %+v", c)
	}
	if c.Version != "2.1.274" {
		t.Errorf("Version = %q, want installed 2.1.274 (not latest 2.1.304)", c.Version)
	}
	if c.Path != "/opt/homebrew/Caskroom/claude-code" {
		t.Errorf("Path = %q", c.Path)
	}
	if c.Homepage == "" || c.Description == "" {
		t.Errorf("missing metadata: %+v", c)
	}
}

func TestBrewOutdated(t *testing.T) {
	ups, err := NewBrew(brewFake(t)).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Name != "ada-url" || ups[0].Latest != "4.0.0_1" {
		t.Errorf("ups = %+v", ups)
	}
}

func TestCaskOutdated(t *testing.T) {
	ups, err := NewCask(brewFake(t)).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Name != "claude-code" || ups[0].Latest != "2.1.304" {
		t.Errorf("ups = %+v", ups)
	}
}

func TestBrewUpgradeAll(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"brew upgrade --formula": {Stdout: []byte("upgrading\n")},
	}}
	var out bytes.Buffer
	if err := NewBrew(f).Upgrade(context.Background(), nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 || f.Calls[0] != "brew upgrade --formula" {
		t.Errorf("calls = %q", f.Calls)
	}
	if out.String() != "upgrading\n" {
		t.Errorf("out = %q", out.String())
	}
}

func TestBrewUpgradeNames(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"brew upgrade --formula jq wget": {},
		"brew upgrade --cask firefox":    {},
	}}
	ctx := context.Background()
	if err := NewBrew(f).Upgrade(ctx, []string{"jq", "wget"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := NewCask(f).Upgrade(ctx, []string{"firefox"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"brew upgrade --formula jq wget", "brew upgrade --cask firefox"}
	if strings.Join(f.Calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", f.Calls, want)
	}
}

func TestBrewUpgradeFailure(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"brew upgrade --formula": {ExitCode: 1, Stderr: []byte("Error: boom\n")},
	}}
	err := NewBrew(f).Upgrade(context.Background(), nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "exit code 1") {
		t.Errorf("err = %v, want exit-code error", err)
	}
}

func TestBrewRejectsBadNames(t *testing.T) {
	f := &runner.Fake{}
	p := NewBrew(f)
	ctx := context.Background()
	if err := p.Upgrade(ctx, []string{"--force"}, &bytes.Buffer{}); err == nil {
		t.Error("Upgrade accepted a flag-like name")
	}
	if err := p.Install(ctx, "x;rm", &bytes.Buffer{}); err == nil {
		t.Error("Install accepted an invalid name")
	}
	if _, err := p.Search(ctx, "--cask"); err == nil {
		t.Error("Search accepted a flag-like query")
	}
	if len(f.Calls) != 0 {
		t.Errorf("commands ran despite invalid input: %q", f.Calls)
	}
}

func TestBrewInstall(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"brew install --formula jq":   {},
		"brew install --cask firefox": {},
	}}
	ctx := context.Background()
	if err := NewBrew(f).Install(ctx, "jq", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := NewCask(f).Install(ctx, "firefox", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 {
		t.Errorf("calls = %q", f.Calls)
	}
}

func TestBrewSearchSkipsHeaders(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"brew search --formula jq": {Stdout: []byte("==> Formulae\njq\njqp\n\n==> Casks\njq-app\n")},
	}}
	got, err := NewBrew(f).Search(context.Background(), "jq")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Name != "jq" || got[1].Name != "jqp" || got[2].Name != "jq-app" {
		t.Fatalf("got %+v", got)
	}
	if got[0].Source != "brew" {
		t.Errorf("Source = %q", got[0].Source)
	}
}

func TestBrewSearchNoMatch(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"brew search --cask zzz": {ExitCode: 1, Stderr: []byte("Error: No formulae or casks found for \"zzz\".\n")},
	}}
	got, err := NewCask(f).Search(context.Background(), "zzz")
	if err != nil || len(got) != 0 {
		t.Errorf("got %+v, err %v; want no results and no error", got, err)
	}
}

func TestBrewListError(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"brew --prefix": {Stdout: []byte("/opt/homebrew\n")},
		infoKey:         {ExitCode: 1, Stderr: []byte("Error: boom\n")},
	}}
	for _, p := range []Provider{NewBrew(f), NewCask(f)} {
		if _, err := p.List(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s: err = %v, want it to contain boom", p.Name(), err)
		}
	}
}

func TestBrewAvailableAndWatchPaths(t *testing.T) {
	f := brewFake(t)
	b, c := NewBrew(f), NewCask(f)
	if b.Name() != "brew" || c.Name() != "cask" {
		t.Errorf("names = %q, %q", b.Name(), c.Name())
	}
	if !b.Available() || !c.Available() {
		t.Error("Available() = false with brew on PATH")
	}
	if got := b.WatchPaths(); len(got) != 1 || got[0] != "/opt/homebrew/Cellar" {
		t.Errorf("brew WatchPaths = %q", got)
	}
	if got := c.WatchPaths(); len(got) != 1 || got[0] != "/opt/homebrew/Caskroom" {
		t.Errorf("cask WatchPaths = %q", got)
	}
	prefixCalls := 0
	for _, call := range f.Calls {
		if call == "brew --prefix" {
			prefixCalls++
		}
	}
	if prefixCalls != 2 { // one per provider, each cached
		t.Errorf("brew --prefix ran %d times, want 2 (once per provider)", prefixCalls)
	}
}

func TestBrewMissing(t *testing.T) {
	f := &runner.Fake{Missing: map[string]bool{"brew": true}}
	p := NewBrew(f)
	if p.Available() {
		t.Error("Available() = true with brew missing")
	}
	if got := p.WatchPaths(); len(got) != 0 {
		t.Errorf("WatchPaths = %q, want none when the prefix is unknown", got)
	}
}

func TestDefaultsIncludesBrewAndCask(t *testing.T) {
	ps := Defaults(&runner.Fake{}, nil)
	if len(ps) < 2 || ps[0].Name() != "brew" || ps[1].Name() != "cask" {
		t.Errorf("Defaults order = %v", names(ps))
	}
}

func names(ps []Provider) []string {
	var n []string
	for _, p := range ps {
		n = append(n, p.Name())
	}
	return n
}

// flakyPrefix fails the first `brew --prefix` and succeeds afterwards.
type flakyPrefix struct {
	*runner.Fake
	prefixCalls int
}

func (f *flakyPrefix) Run(ctx context.Context, name string, args ...string) (runner.Result, error) {
	if name == "brew" && strings.Join(args, " ") == "--prefix" {
		f.prefixCalls++
		if f.prefixCalls == 1 {
			return runner.Result{ExitCode: 1, Stderr: []byte("Error: transient\n")}, nil
		}
		return runner.Result{Stdout: []byte("/opt/homebrew\n")}, nil
	}
	return f.Fake.Run(ctx, name, args...)
}

func TestBrewPrefixRetriedAfterFailure(t *testing.T) {
	f := &flakyPrefix{Fake: brewFake(t)}
	p := NewBrew(f)
	if got := p.WatchPaths(); len(got) != 0 {
		t.Fatalf("WatchPaths = %q while brew --prefix fails, want none", got)
	}
	got := p.WatchPaths()
	if len(got) != 1 || got[0] != "/opt/homebrew/Cellar" {
		t.Errorf("WatchPaths after recovery = %q", got)
	}
	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tools[0].Path != "/opt/homebrew/Cellar/argocd" {
		t.Errorf("Path after recovery = %q", tools[0].Path)
	}
	if f.prefixCalls != 2 {
		t.Errorf("brew --prefix ran %d times, want 2 (retry once, then cached)", f.prefixCalls)
	}
}
