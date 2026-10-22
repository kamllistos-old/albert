package provider

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kamllistos/albert/internal/runner"
)

const (
	gemSystemDir = "/Library/Ruby/Gems/2.6.0"
	gemUserDir   = "/Users/x/.gem/ruby/3.3.0"
)

func gemFake(t *testing.T, gemdir string) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		"gem env gemdir":                      {Stdout: []byte(gemdir + "\n")},
		"gem list --local":                    {Stdout: fixture(t, "gem_list.txt")},
		"gem outdated":                        {Stdout: fixture(t, "gem_outdated.txt")},
		"gem search rake":                     {Stdout: []byte("rake (13.2.1)\nrake-compiler (1.2.7 ruby)\nrake-multi (1.0, 0.9)\n")},
		"gem update":                          {},
		"gem update nokogiri minitest":        {},
		"gem install --user-install nokogiri": {},
	}}
}

func TestGemSkipsDefault(t *testing.T) {
	tools, err := NewGem(gemFake(t, gemUserDir)).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tl := range tools {
		got = append(got, tl.Name+"@"+tl.Version)
		if tl.ReadOnly {
			t.Errorf("%s is read-only in a user gem dir", tl.Name)
		}
		if tl.Source != "gem" {
			t.Errorf("Source = %q", tl.Source)
		}
	}
	want := []string{"CFPropertyList@2.3.6", "minitest@5.11.3", "nokogiri@1.13.8"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v (default gem bigdecimal skipped)", got, want)
	}
	if tools[0].Path != gemUserDir+"/gems/CFPropertyList-2.3.6" {
		t.Errorf("Path = %q", tools[0].Path)
	}
}

func TestGemListFirstVersionOfMany(t *testing.T) {
	f := gemFake(t, gemUserDir)
	f.Responses["gem list --local"] = runner.Result{Stdout: []byte("rake (13.0.6, 12.3.3)\nmixed (1.0, default: 0.9)\nplat (2.0 java)\n")}
	tools, err := NewGem(f).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tl := range tools {
		got = append(got, tl.Name+"@"+tl.Version)
	}
	if want := []string{"rake@13.0.6", "mixed@1.0", "plat@2.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
}

func TestGemSystemRubyReadOnly(t *testing.T) {
	f := gemFake(t, gemSystemDir)
	p := NewGem(f)
	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 3 {
		t.Fatalf("got %d tools", len(tools))
	}
	for _, tl := range tools {
		if !tl.ReadOnly {
			t.Errorf("%s should be read-only on system Ruby", tl.Name)
		}
	}
	for _, names := range [][]string{nil, {"nokogiri"}} {
		if err := p.Upgrade(context.Background(), names, &bytes.Buffer{}); !errors.Is(err, ErrReadOnly) {
			t.Errorf("Upgrade(%v) err = %v, want ErrReadOnly", names, err)
		}
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "gem update") {
			t.Errorf("ran %q on a read-only gem dir", c)
		}
	}
	// /System is read-only too.
	f.Responses["gem env gemdir"] = runner.Result{Stdout: []byte("/System/Library/Ruby/Gems\n")}
	if err := p.Upgrade(context.Background(), nil, &bytes.Buffer{}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Upgrade under /System err = %v", err)
	}
}

func TestGemOutdated(t *testing.T) {
	ups, err := NewGem(gemFake(t, gemUserDir)).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Update{
		{Name: "CFPropertyList", Latest: "4.0.0"},
		{Name: "bigdecimal", Latest: "4.1.3"},
		{Name: "minitest", Latest: "6.0.6"},
		{Name: "nokogiri", Latest: "1.19.4"},
	}
	if !reflect.DeepEqual(ups, want) {
		t.Errorf("Outdated = %+v, want %+v", ups, want)
	}
}

func TestGemUpgrade(t *testing.T) {
	f := gemFake(t, gemUserDir)
	p := NewGem(f)
	if err := p.Upgrade(context.Background(), []string{"-bad"}, &bytes.Buffer{}); err == nil {
		t.Error("want error for flag-like name")
	}
	if err := p.Upgrade(context.Background(), []string{"nokogiri", "minitest"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Upgrade(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var updates []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "gem update") {
			updates = append(updates, c)
		}
	}
	if want := []string{"gem update nokogiri minitest", "gem update"}; !reflect.DeepEqual(updates, want) {
		t.Errorf("updates = %q, want %q", updates, want)
	}
}

func TestGemInstall(t *testing.T) {
	f := gemFake(t, gemUserDir)
	p := NewGem(f)
	if err := p.Install(context.Background(), "-x", &bytes.Buffer{}); err == nil {
		t.Error("want error for flag-like name")
	}
	if err := p.Install(context.Background(), "nokogiri", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"gem install --user-install nokogiri"}) {
		t.Errorf("Calls = %q", f.Calls)
	}
}

func TestGemSearch(t *testing.T) {
	f := gemFake(t, gemUserDir)
	rs, err := NewGem(f).Search(context.Background(), "rake")
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{
		{Name: "rake", Source: "gem", Version: "13.2.1"},
		{Name: "rake-compiler", Source: "gem", Version: "1.2.7"},
		{Name: "rake-multi", Source: "gem", Version: "1.0"},
	}
	if !reflect.DeepEqual(rs, want) {
		t.Errorf("Search = %+v, want %+v", rs, want)
	}
	if rs, err := NewGem(f).Search(context.Background(), "--help"); err != nil || len(rs) != 0 {
		t.Errorf("flag-like query = %v, %v", rs, err)
	}
}

func TestGemWatchPathsAndAvailable(t *testing.T) {
	p := NewGem(gemFake(t, gemUserDir))
	if got := p.WatchPaths(); !reflect.DeepEqual(got, []string{gemUserDir + "/gems"}) {
		t.Errorf("WatchPaths = %q", got)
	}
	f := &runner.Fake{Missing: map[string]bool{"gem": true}}
	if q := NewGem(f); q.Name() != "gem" || q.Available() {
		t.Error("Name/Available wrong")
	}
	if got := NewGem(&runner.Fake{}).WatchPaths(); len(got) != 0 {
		t.Errorf("WatchPaths with failing gem env = %q", got)
	}
}

func TestDefaultsHasTwelve(t *testing.T) {
	got := names(Defaults(&runner.Fake{}, nil))
	want := []string{"brew", "cask", "uv", "pipx", "pip", "npm", "pnpm", "bun", "mas", "cargo", "go", "gem"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Defaults = %v, want %v", got, want)
	}
}

const gemUserDirKey = "ruby -e print Gem.user_dir"

func TestGemUserDirGemsWritableOnSystemRuby(t *testing.T) {
	user := t.TempDir()
	if err := os.MkdirAll(filepath.Join(user, "gems", "nokogiri-1.13.8"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := gemFake(t, gemSystemDir)
	f.Responses[gemUserDirKey] = runner.Result{Stdout: []byte(user + "\n")}
	f.Responses["gem update --user-install nokogiri"] = runner.Result{}
	p := NewGem(f)

	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		wantRO := tl.Name != "nokogiri"
		if tl.ReadOnly != wantRO {
			t.Errorf("%s ReadOnly = %v, want %v", tl.Name, tl.ReadOnly, wantRO)
		}
		if tl.Name == "nokogiri" && tl.Path != filepath.Join(user, "gems", "nokogiri-1.13.8") {
			t.Errorf("nokogiri Path = %q", tl.Path)
		}
	}

	f.Calls = nil
	if err := p.Upgrade(context.Background(), []string{"nokogiri"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if last := f.Calls[len(f.Calls)-1]; last != "gem update --user-install nokogiri" {
		t.Errorf("last call = %q", last)
	}
	// Upgrade-all only touches the writable gem.
	f.Calls = nil
	if err := p.Upgrade(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if last := f.Calls[len(f.Calls)-1]; last != "gem update --user-install nokogiri" {
		t.Errorf("upgrade-all last call = %q", last)
	}
	// A system gem is still refused, and nothing runs.
	f.Calls = nil
	if err := p.Upgrade(context.Background(), []string{"nokogiri", "minitest"}, &bytes.Buffer{}); !errors.Is(err, ErrReadOnly) {
		t.Errorf("err = %v, want ErrReadOnly", err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "gem update") {
			t.Errorf("ran %q", c)
		}
	}
	want := []string{gemSystemDir + "/gems", filepath.Join(user, "gems")}
	if got := p.WatchPaths(); !reflect.DeepEqual(got, want) {
		t.Errorf("WatchPaths = %q, want %q", got, want)
	}
}

func TestGemGemdirFailureFailsClosed(t *testing.T) {
	f := gemFake(t, gemUserDir)
	f.Responses["gem env gemdir"] = runner.Result{ExitCode: 1, Stderr: []byte("boom\n")}
	p := NewGem(f)
	tools, err := p.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if !tl.ReadOnly {
			t.Errorf("%s should be read-only when gemdir is unknown", tl.Name)
		}
	}
	for _, names := range [][]string{nil, {"nokogiri"}} {
		if err := p.Upgrade(context.Background(), names, &bytes.Buffer{}); !errors.Is(err, ErrReadOnly) {
			t.Errorf("Upgrade(%v) err = %v, want ErrReadOnly", names, err)
		}
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "gem update") {
			t.Errorf("ran %q", c)
		}
	}
}
