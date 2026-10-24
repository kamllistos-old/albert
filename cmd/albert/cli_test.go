package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/kamllistos/albert/internal/provider"
)

// fakeProvider is an in-memory provider.Provider. It records Upgrade and
// Install calls and never touches the machine.
type fakeProvider struct {
	name      string
	missing   bool
	tools     []provider.Tool
	updates   []provider.Update
	results   []provider.Result
	listErr   error
	upgradeEr error

	mu        sync.Mutex
	upgrades  [][]string
	installed []string
}

func (f *fakeProvider) Name() string    { return f.name }
func (f *fakeProvider) Available() bool { return !f.missing }
func (f *fakeProvider) List(context.Context) ([]provider.Tool, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]provider.Tool(nil), f.tools...), nil
}
func (f *fakeProvider) Outdated(context.Context) ([]provider.Update, error) { return f.updates, nil }
func (f *fakeProvider) Upgrade(_ context.Context, names []string, out io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgrades = append(f.upgrades, names)
	io.WriteString(out, "upgrading "+f.name+"\n")
	return f.upgradeEr
}
func (f *fakeProvider) Install(_ context.Context, name string, _ io.Writer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, name)
	return nil
}
func (f *fakeProvider) Search(_ context.Context, q string) ([]provider.Result, error) {
	return f.results, nil
}
func (f *fakeProvider) WatchPaths() []string { return nil }

type fakeDocker struct{ err error }

func (d fakeDocker) Ready(context.Context) error { return d.err }

type harness struct {
	a           *app
	out, errOut *bytes.Buffer
}

func newApp(stdin string, tty bool, ps ...provider.Provider) *harness {
	h := &harness{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	h.a = &app{
		providers: ps,
		stdout:    h.out,
		stderr:    h.errOut,
		stdin:     strings.NewReader(stdin),
		isTTY:     tty,
		docker:    fakeDocker{},
	}
	return h
}

func (h *harness) run(args ...string) int { return run(h.a, args) }

func TestListHidesDeps(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{
		{Name: "wget", Version: "1.0"},
		{Name: "libidn2", Version: "2.3", Dependency: true},
	}}
	h := newApp("", false, brew)
	if code := h.run("list"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.errOut)
	}
	out := h.out.String()
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "STATUS") {
		t.Errorf("missing table header:\n%s", out)
	}
	if !strings.Contains(out, "wget") || strings.Contains(out, "libidn2") {
		t.Errorf("dependencies should be hidden:\n%s", out)
	}
	if !strings.Contains(out, "1 tool from 1 source.") {
		t.Errorf("missing footer:\n%s", out)
	}

	h = newApp("", false, brew)
	h.run("list", "--deps")
	if !strings.Contains(h.out.String(), "libidn2") {
		t.Errorf("--deps should show dependencies:\n%s", h.out)
	}
}

func TestListJSON(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1.0"}}}
	h := newApp("", false, brew)
	if code := h.run("list", "--json"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.errOut)
	}
	var got []provider.Tool
	if err := json.Unmarshal(h.out.Bytes(), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, h.out)
	}
	if len(got) != 1 || got[0].Name != "wget" || got[0].Source != "brew" {
		t.Errorf("got %+v", got)
	}
}

func TestListJSONEmptyIsArray(t *testing.T) {
	h := newApp("", false, &fakeProvider{name: "brew"})
	h.run("list", "--json")
	if strings.TrimSpace(h.out.String()) != "[]" {
		t.Errorf("got %q", h.out)
	}
}

func TestListShowsProviderError(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1.0"}}}
	pnpm := &fakeProvider{name: "pnpm", listErr: errors.New("pnpm: not on PATH")}
	h := newApp("", false, brew, pnpm)
	if code := h.run("list"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "! pnpm: not on PATH\n") {
		t.Errorf("missing provider error line (once, without a doubled name):\n%s", h.out)
	}
}

func TestListSkipsNotInstalledProviders(t *testing.T) {
	h := newApp("", false,
		&fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1"}}},
		&fakeProvider{name: "bun", missing: true})
	h.run("list")
	if strings.Contains(h.out.String(), "bun") {
		t.Errorf("a missing provider is not an error:\n%s", h.out)
	}
}

func TestListOutdatedStatuses(t *testing.T) {
	brew := &fakeProvider{name: "brew",
		tools:   []provider.Tool{{Name: "wget", Version: "1.0"}, {Name: "jq", Version: "1.6"}},
		updates: []provider.Update{{Name: "wget", Latest: "2.0"}}}
	h := newApp("", false, brew)
	h.run("list", "--outdated")
	out := h.out.String()
	if !strings.Contains(out, "wget") || !strings.Contains(out, "update available") || !strings.Contains(out, "2.0") {
		t.Errorf("wget should be listed as outdated:\n%s", out)
	}
	if strings.Contains(out, "jq") {
		t.Errorf("--outdated should hide up-to-date tools:\n%s", out)
	}
	if !strings.Contains(h.errOut.String(), "Checking for updates…") {
		t.Errorf("stderr: %q", h.errOut)
	}
}

func TestListStatusReadOnly(t *testing.T) {
	brew := &fakeProvider{name: "brew",
		tools:   []provider.Tool{{Name: "jq", Version: "1.6"}, {Name: "sys", Version: "1", ReadOnly: true}},
		updates: nil}
	h := newApp("", false, brew)
	h.run("list", "--source", "brew")
	out := h.out.String()
	if !strings.Contains(out, "read-only") {
		t.Errorf("want read-only status:\n%s", out)
	}
}

func TestListUncheckedDoesNotClaimUpToDate(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "jq", Version: "1.6"}}}
	h := newApp("", false, brew)
	h.run("list")
	if strings.Contains(h.out.String(), "up to date") {
		t.Errorf("list without --outdated has not checked, so must not say up to date:\n%s", h.out)
	}
}

func TestListUnknownSource(t *testing.T) {
	h := newApp("", false, &fakeProvider{name: "brew"})
	if code := h.run("list", "--source", "nope"); code != 2 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), `"nope"`) {
		t.Errorf("stderr %q", h.errOut)
	}
}

func TestOutdatedNone(t *testing.T) {
	h := newApp("", false, &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1.0"}}})
	if code := h.run("outdated"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), "Everything is up to date.") {
		t.Errorf("stdout %q", h.out)
	}
	if !strings.Contains(h.errOut.String(), "Checking for updates…") {
		t.Errorf("stderr %q", h.errOut)
	}
}

func TestOutdatedListsTools(t *testing.T) {
	brew := &fakeProvider{name: "brew",
		tools:   []provider.Tool{{Name: "wget", Version: "1.0"}, {Name: "jq", Version: "1.6"}},
		updates: []provider.Update{{Name: "wget", Latest: "2.0"}}}
	h := newApp("", false, brew)
	h.run("outdated")
	out := h.out.String()
	if !strings.Contains(out, "wget") || strings.Contains(out, "jq") {
		t.Errorf("stdout:\n%s", out)
	}
}

func TestUpdateAllSkipsPip(t *testing.T) {
	brew := &fakeProvider{name: "brew",
		tools:   []provider.Tool{{Name: "wget", Version: "1.0"}},
		updates: []provider.Update{{Name: "wget", Latest: "2.0"}}}
	pip := &fakeProvider{name: "pip",
		tools:   []provider.Tool{{Name: "requests", Version: "1.0"}},
		updates: []provider.Update{{Name: "requests", Latest: "2.0"}}}
	h := newApp("", false, brew, pip)
	if code := h.run("update", "--all"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.errOut)
	}
	if len(brew.upgrades) != 1 || len(brew.upgrades[0]) != 0 {
		t.Errorf("brew upgrades: %v", brew.upgrades)
	}
	if len(pip.upgrades) != 0 {
		t.Errorf("pip must be skipped without --include-pip: %v", pip.upgrades)
	}

	brew.upgrades, pip.upgrades = nil, nil
	h = newApp("", false, brew, pip)
	if code := h.run("update", "--all", "--include-pip"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(pip.upgrades) != 1 {
		t.Errorf("pip should be upgraded with --include-pip: %v", pip.upgrades)
	}
}

func TestUpdateAllNothingToDo(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1.0"}}}
	h := newApp("", false, brew)
	if code := h.run("update", "--all"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(brew.upgrades) != 0 || !strings.Contains(h.out.String(), "Everything is up to date.") {
		t.Errorf("upgrades %v, stdout %q", brew.upgrades, h.out)
	}
}

func TestUpdateAllFailureExitsOne(t *testing.T) {
	brew := &fakeProvider{name: "brew",
		tools:     []provider.Tool{{Name: "wget", Version: "1.0"}},
		updates:   []provider.Update{{Name: "wget", Latest: "2.0"}},
		upgradeEr: errors.New("brew: boom")}
	h := newApp("", false, brew)
	if code := h.run("update", "--all"); code != 1 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "boom") {
		t.Errorf("stderr %q", h.errOut)
	}
}

func TestUpdateOneTool(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1.0"}}}
	h := newApp("", false, brew)
	// flag after the tool name must still work
	if code := h.run("update", "wget", "--source", "brew"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.errOut)
	}
	if len(brew.upgrades) != 1 || len(brew.upgrades[0]) != 1 || brew.upgrades[0][0] != "wget" {
		t.Errorf("upgrades %v", brew.upgrades)
	}
	if !strings.Contains(h.out.String(), "upgrading brew") {
		t.Errorf("provider output should reach stdout: %q", h.out)
	}
}

func TestUpdateUnknownTool(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1.0"}}}
	h := newApp("", false, brew)
	if code := h.run("update", "nothing"); code != 1 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), `Couldn't find "nothing"`) || len(brew.upgrades) != 0 {
		t.Errorf("stderr %q upgrades %v", h.errOut, brew.upgrades)
	}
}

func TestUpdateReadOnlyIsPlainEnglish(t *testing.T) {
	brew := &fakeProvider{name: "brew",
		tools:     []provider.Tool{{Name: "wget", Version: "1.0"}},
		upgradeEr: provider.ErrReadOnly}
	h := newApp("", false, brew)
	if code := h.run("update", "wget"); code != 1 {
		t.Errorf("exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "wget") || !strings.Contains(h.errOut.String(), "read-only") {
		t.Errorf("stderr %q", h.errOut)
	}
}

func TestUpdateRejectsBadName(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "--force", Version: "1"}}}
	h := newApp("", false, brew)
	if code := h.run("update", "--", "--force"); code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if len(brew.upgrades) != 0 {
		t.Errorf("Upgrade must not be called: %v", brew.upgrades)
	}
	if !strings.Contains(h.errOut.String(), "--force") {
		t.Errorf("stderr %q", h.errOut)
	}
}

func TestUpdateUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"update"},
		{"update", "--all", "wget"},
		{"update", "a", "b"},
		{"update", "wget", "--include-pip"},
		{"update", "wget", "--source", "nope"},
		{"install"},
		{"install", "--", "--force"},
		{"search"},
		{"bogus"},
	} {
		brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1"}}}
		h := newApp("", false, brew)
		if code := h.run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2 (stderr %q)", args, code, h.errOut)
		}
		if len(brew.upgrades)+len(brew.installed) != 0 {
			t.Errorf("%v: no provider call expected", args)
		}
	}
}

func TestResolveSourcePrompt(t *testing.T) {
	tools := []provider.Tool{{Name: "ruff", Source: "brew"}, {Name: "ruff", Source: "uv"}, {Name: "other", Source: "npm"}}
	var out bytes.Buffer
	got, err := resolveSource(tools, "ruff", "", strings.NewReader("2\n"), &out, true)
	if err != nil || got != "uv" {
		t.Fatalf("got %q, %v", got, err)
	}
	if !strings.Contains(out.String(), "1) brew") || !strings.Contains(out.String(), "2) uv") {
		t.Errorf("menu: %q", out.String())
	}
}

func TestResolveSourceBadChoice(t *testing.T) {
	tools := []provider.Tool{{Name: "ruff", Source: "brew"}, {Name: "ruff", Source: "uv"}}
	for _, in := range []string{"9\n", "x\n", "", "0\n"} {
		if got, err := resolveSource(tools, "ruff", "", strings.NewReader(in), io.Discard, true); err == nil {
			t.Errorf("input %q: got %q, want error", in, got)
		}
	}
}

func TestResolveSourceNonTTYError(t *testing.T) {
	tools := []provider.Tool{{Name: "ruff", Source: "brew"}, {Name: "ruff", Source: "uv"}}
	_, err := resolveSource(tools, "ruff", "", strings.NewReader(""), io.Discard, false)
	want := `"ruff" is installed from several sources (brew, uv). Add --source.`
	if err == nil || err.Error() != want {
		t.Errorf("got %v, want %q", err, want)
	}
}

func TestResolveSourceExplicitAndSingle(t *testing.T) {
	tools := []provider.Tool{{Name: "ruff", Source: "brew"}, {Name: "ruff", Source: "uv"}, {Name: "jq", Source: "brew"}}
	if got, err := resolveSource(tools, "ruff", "brew", nil, io.Discard, false); err != nil || got != "brew" {
		t.Errorf("explicit: %q, %v", got, err)
	}
	if _, err := resolveSource(tools, "jq", "uv", nil, io.Discard, false); err == nil {
		t.Error("jq is not installed from uv; want error")
	}
	if got, err := resolveSource(tools, "jq", "", nil, io.Discard, false); err != nil || got != "brew" {
		t.Errorf("single: %q, %v", got, err)
	}
	if _, err := resolveSource(tools, "missing", "", nil, io.Discard, true); err == nil {
		t.Error("want error for unknown tool")
	}
}

func TestInstallNotFoundMessage(t *testing.T) {
	brew := &fakeProvider{name: "brew"}
	h := newApp("", false, brew)
	if code := h.run("install", "nothing"); code != 1 {
		t.Errorf("exit %d", code)
	}
	want := `Couldn't find "nothing". Try "albert search nothing".`
	if !strings.Contains(h.errOut.String(), want) {
		t.Errorf("stderr %q, want %q", h.errOut, want)
	}
	if len(brew.installed) != 0 {
		t.Errorf("installed %v", brew.installed)
	}
}

func TestInstallSingleMatch(t *testing.T) {
	brew := &fakeProvider{name: "brew", results: []provider.Result{
		{Name: "wget", Source: "brew"}, {Name: "wget2", Source: "brew"}}}
	uv := &fakeProvider{name: "uv", results: []provider.Result{{Name: "cowsay", Source: "uv"}}}
	h := newApp("", false, brew, uv)
	if code := h.run("install", "wget"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.errOut)
	}
	if len(brew.installed) != 1 || brew.installed[0] != "wget" || len(uv.installed) != 0 {
		t.Errorf("brew %v uv %v", brew.installed, uv.installed)
	}
}

func TestInstallSeveralPrompts(t *testing.T) {
	brew := &fakeProvider{name: "brew", results: []provider.Result{{Name: "ruff", Source: "brew"}}}
	uv := &fakeProvider{name: "uv", results: []provider.Result{{Name: "ruff", Source: "uv"}}}
	h := newApp("2\n", true, brew, uv)
	if code := h.run("install", "ruff"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.errOut)
	}
	if len(uv.installed) != 1 || len(brew.installed) != 0 {
		t.Errorf("brew %v uv %v", brew.installed, uv.installed)
	}

	brew, uv = &fakeProvider{name: "brew", results: brew.results}, &fakeProvider{name: "uv", results: uv.results}
	h = newApp("", false, brew, uv)
	if code := h.run("install", "ruff"); code != 1 {
		t.Errorf("non-tty: exit %d", code)
	}
	if !strings.Contains(h.errOut.String(), "several sources (brew, uv). Add --source.") {
		t.Errorf("stderr %q", h.errOut)
	}
}

func TestInstallWithSource(t *testing.T) {
	brew := &fakeProvider{name: "brew"}
	h := newApp("", false, brew)
	if code := h.run("install", "wget", "--source", "brew"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.errOut)
	}
	if len(brew.installed) != 1 || brew.installed[0] != "wget" {
		t.Errorf("installed %v", brew.installed)
	}
}

func TestInstallUsesIDWhenSourceGivesOne(t *testing.T) {
	mas := &fakeProvider{name: "mas", results: []provider.Result{{Name: "Xcode", Source: "mas", ID: "497799835"}}}
	h := newApp("", false, mas)
	if code := h.run("install", "Xcode"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, h.errOut)
	}
	if len(mas.installed) != 1 || mas.installed[0] != "497799835" {
		t.Errorf("installed %v", mas.installed)
	}
}

func TestSearch(t *testing.T) {
	brew := &fakeProvider{name: "brew", results: []provider.Result{
		{Name: "wget", Source: "brew", Version: "1.0", Description: "Internet file retriever"}}}
	h := newApp("", false, brew)
	if code := h.run("search", "wget"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out := h.out.String()
	for _, w := range []string{"NAME", "SOURCE", "VERSION", "DESCRIPTION", "wget", "Internet file retriever"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
}

func TestSearchNoResults(t *testing.T) {
	h := newApp("", false, &fakeProvider{name: "brew"})
	if code := h.run("search", "zzz"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(h.out.String(), `Nothing found for "zzz".`) {
		t.Errorf("stdout %q", h.out)
	}
}

func TestDoctor(t *testing.T) {
	brew := &fakeProvider{name: "brew", tools: []provider.Tool{{Name: "wget", Version: "1"}}}
	bun := &fakeProvider{name: "bun", missing: true}
	pnpm := &fakeProvider{name: "pnpm", listErr: errors.New("pnpm: not on PATH")}
	h := newApp("", false, brew, bun, pnpm)
	h.a.docker = fakeDocker{err: errors.New("docker is not running")}
	if code := h.run("doctor"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	out := h.out.String()
	for _, w := range []string{"✓ brew\n", "– bun (not installed)\n", "✗ pnpm: not on PATH\n", "✗ Docker: not running\n"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}

	h = newApp("", false, brew)
	h.run("doctor")
	if !strings.Contains(h.out.String(), "✓ Docker\n") {
		t.Errorf("stdout:\n%s", h.out)
	}
}

func TestHelpListsUpAndDownFirst(t *testing.T) {
	for _, args := range [][]string{{}, {"help"}, {"--help"}} {
		h := newApp("", false)
		if code := h.run(args...); code != 0 {
			t.Errorf("%v: exit %d", args, code)
		}
		out := h.out.String()
		up, down, list := strings.Index(out, "  up"), strings.Index(out, "  down"), strings.Index(out, "  list")
		if up < 0 || down < up || list < down {
			t.Errorf("%v: want up, down, then list:\n%s", args, out)
		}
	}
}

func TestVersion(t *testing.T) {
	h := newApp("", false)
	if code := h.run("version"); code != 0 || !strings.HasPrefix(h.out.String(), "albert ") {
		t.Errorf("exit %d out %q", code, h.out)
	}
}
