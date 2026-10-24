package provider

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/kamllistos/albert/internal/runner"
)

func masFake(t *testing.T) *runner.Fake {
	t.Helper()
	return &runner.Fake{Responses: map[string]runner.Result{
		"mas list":                         {Stdout: fixture(t, "mas_list.txt")},
		"mas outdated":                     {Stdout: fixture(t, "mas_outdated.txt")},
		"mas search xcode":                 {Stdout: fixture(t, "mas_search.txt")},
		"mas upgrade":                      {},
		"mas upgrade 1333542190":           {},
		"mas upgrade 497799835 1333542190": {},
		"mas install 497799835":            {},
	}}
}

func TestMasListNameWithSpaces(t *testing.T) {
	tools, err := NewMas(masFake(t)).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2: %+v", len(tools), tools)
	}
	x := tools[0]
	if x.Name != "Xcode" || x.ID != "497799835" || x.Version != "16.0" || x.Source != "mas" {
		t.Errorf("tools[0] = %+v", x)
	}
	p := tools[1]
	if p.Name != "1Password 7" || p.ID != "1333542190" || p.Version != "7.9.11" {
		t.Errorf("tools[1] = %+v", p)
	}
}

func TestMasOutdated(t *testing.T) {
	ups, err := NewMas(masFake(t)).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Update{{Name: "Xcode", Latest: "16.1"}}
	if !reflect.DeepEqual(ups, want) {
		t.Errorf("Outdated = %+v, want %+v", ups, want)
	}
}

func TestMasUpgradeMapsNamesToIDs(t *testing.T) {
	f := masFake(t)
	var out bytes.Buffer
	if err := NewMas(f).Upgrade(context.Background(), []string{"Xcode", "1333542190"}, &out); err != nil {
		t.Fatal(err)
	}
	want := []string{"mas list", "mas upgrade 497799835 1333542190"}
	if !reflect.DeepEqual(f.Calls, want) {
		t.Errorf("Calls = %q, want %q", f.Calls, want)
	}
}

func TestMasUpgradeAll(t *testing.T) {
	f := masFake(t)
	if err := NewMas(f).Upgrade(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"mas upgrade"}) {
		t.Errorf("Calls = %q", f.Calls)
	}
}

func TestMasUpgradeUnknownName(t *testing.T) {
	f := masFake(t)
	err := NewMas(f).Upgrade(context.Background(), []string{"Nope"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("err = %v, want unknown-app error", err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "mas upgrade") {
			t.Errorf("ran %q for an unknown app", c)
		}
	}
}

func TestMasInstallRequiresID(t *testing.T) {
	f := masFake(t)
	p := NewMas(f)
	for _, bad := range []string{"Xcode", "", "-1", "123 456", "12a"} {
		err := p.Install(context.Background(), bad, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "use the App Store ID from search") {
			t.Errorf("Install(%q) err = %v", bad, err)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("ran %q for invalid installs", f.Calls)
	}
	if err := p.Install(context.Background(), "497799835", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"mas install 497799835"}) {
		t.Errorf("Calls = %q", f.Calls)
	}
}

func TestMasSearch(t *testing.T) {
	rs, err := NewMas(masFake(t)).Search(context.Background(), "xcode")
	if err != nil {
		t.Fatal(err)
	}
	want := []Result{
		{Name: "Xcode", Source: "mas", Version: "16.0", ID: "497799835"},
		{Name: "The Unarchiver", Source: "mas", Version: "4.3.8", ID: "425424353"},
	}
	if !reflect.DeepEqual(rs, want) {
		t.Errorf("Search = %+v, want %+v", rs, want)
	}
}

func TestMasSearchRejectsFlagLikeQuery(t *testing.T) {
	f := masFake(t)
	for _, q := range []string{"--help", " -x", "  "} {
		rs, err := NewMas(f).Search(context.Background(), q)
		if err != nil || len(rs) != 0 {
			t.Errorf("Search(%q) = %v, %v", q, rs, err)
		}
	}
	if len(f.Calls) != 0 {
		t.Errorf("ran %q", f.Calls)
	}
}

func TestMasBasics(t *testing.T) {
	f := &runner.Fake{Missing: map[string]bool{"mas": true}}
	p := NewMas(f)
	if p.Name() != "mas" || p.Available() {
		t.Error("Name/Available wrong")
	}
	if got := p.WatchPaths(); !reflect.DeepEqual(got, []string{"/Applications"}) {
		t.Errorf("WatchPaths = %q", got)
	}
}
