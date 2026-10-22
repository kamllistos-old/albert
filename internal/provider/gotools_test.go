package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

// goBinDir returns a temp bin dir holding an empty file per name.
func goBinDir(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// goFake answers `go env GOBIN` with dir and `go version -m` for the named
// file with the given output.
func goFake(dir string, versionM map[string]string) *runner.Fake {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"go env GOBIN": {Stdout: []byte(dir + "\n")},
	}}
	for file, out := range versionM {
		f.Responses["go version -m "+filepath.Join(dir, file)] = runner.Result{Stdout: []byte(out)}
	}
	return f
}

func goVersionM(t *testing.T) string {
	t.Helper()
	return string(fixture(t, "go_version_m.txt"))
}

// goProxy serves the module proxy @latest endpoint for module -> version.
func goProxy(t *testing.T, latest map[string]string) *registry.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mod := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/@latest")
		v, ok := latest[mod]
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"Version": v})
	}))
	t.Cleanup(srv.Close)
	return &registry.Client{HTTP: srv.Client(), GoProxyURL: srv.URL}
}

func TestGoListParsesModule(t *testing.T) {
	dir := goBinDir(t, "goimports")
	tools, err := NewGo(goFake(dir, map[string]string{"goimports": goVersionM(t)}), nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("got %d tools: %+v", len(tools), tools)
	}
	g := tools[0]
	if g.Name != "goimports" || g.ID != "golang.org/x/tools/cmd/goimports" || g.Version != "v0.20.0" ||
		g.Source != "go" || g.Path != filepath.Join(dir, "goimports") {
		t.Errorf("tool = %+v", g)
	}
}

func TestGoMissingBinDirEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	tools, err := NewGo(goFake(dir, nil), nil).List(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want none for a missing bin dir", err)
	}
	if len(tools) != 0 {
		t.Errorf("tools = %+v", tools)
	}
}

func TestGoBinDirFallsBackToGOPATH(t *testing.T) {
	gopath := t.TempDir()
	bin := filepath.Join(gopath, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "goimports"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{Responses: map[string]runner.Result{
		"go env GOBIN":  {Stdout: []byte("\n")},
		"go env GOPATH": {Stdout: []byte(gopath + string(os.PathListSeparator) + "/other\n")},
		"go version -m " + filepath.Join(bin, "goimports"): {Stdout: []byte(goVersionM(t))},
	}}
	p := NewGo(f, nil)
	tools, err := p.List(context.Background())
	if err != nil || len(tools) != 1 {
		t.Fatalf("List = %+v, %v", tools, err)
	}
	if got := p.WatchPaths(); !reflect.DeepEqual(got, []string{bin}) {
		t.Errorf("WatchPaths = %q, want %q", got, bin)
	}
}

func TestGoSkipsNonGoFilesAndDirs(t *testing.T) {
	dir := goBinDir(t, "goimports", "script.sh")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := goFake(dir, map[string]string{"goimports": goVersionM(t)})
	f.Responses["go version -m "+filepath.Join(dir, "script.sh")] = runner.Result{ExitCode: 1, Stderr: []byte("script.sh: not a Go executable\n")}
	tools, err := NewGo(f, nil).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "goimports" {
		t.Errorf("tools = %+v", tools)
	}
	for _, c := range f.Calls {
		if strings.HasSuffix(c, "subdir") {
			t.Errorf("inspected a directory: %q", c)
		}
	}
}

func TestGoOutdated(t *testing.T) {
	dir := goBinDir(t, "goimports")
	reg := goProxy(t, map[string]string{"golang.org/x/tools": "v0.21.0"})
	ups, err := NewGo(goFake(dir, map[string]string{"goimports": goVersionM(t)}), reg).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Update{{Name: "goimports", Latest: "v0.21.0"}}
	if !reflect.DeepEqual(ups, want) {
		t.Errorf("Outdated = %+v, want %+v", ups, want)
	}
}

func TestGoSkipsDevel(t *testing.T) {
	dir := goBinDir(t, "mine")
	devel := "/x/mine: go1.26.5\n\tpath\texample.com/mine\n\tmod\texample.com/mine\t(devel)\t\n"
	// A proxy that would report an update proves (devel) is skipped, not just unknown.
	reg := goProxy(t, map[string]string{"example.com/mine": "v1.0.0"})
	ups, err := NewGo(goFake(dir, map[string]string{"mine": devel}), reg).Outdated(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 0 {
		t.Errorf("Outdated = %+v, want none for (devel)", ups)
	}
}

func TestGoOutdatedNeedsRegistry(t *testing.T) {
	dir := goBinDir(t, "goimports")
	if _, err := NewGo(goFake(dir, map[string]string{"goimports": goVersionM(t)}), nil).Outdated(context.Background()); err == nil {
		t.Error("want error without a registry client")
	}
}

func TestGoUpgradeNamedUsesID(t *testing.T) {
	dir := goBinDir(t, "goimports")
	f := goFake(dir, map[string]string{"goimports": goVersionM(t)})
	f.Responses["go install golang.org/x/tools/cmd/goimports@latest"] = runner.Result{}
	if err := NewGo(f, nil).Upgrade(context.Background(), []string{"goimports"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	last := f.Calls[len(f.Calls)-1]
	if last != "go install golang.org/x/tools/cmd/goimports@latest" {
		t.Errorf("last call = %q", last)
	}
}

func TestGoUpgradeAllUsesOutdated(t *testing.T) {
	dir := goBinDir(t, "goimports", "other")
	other := "/x/other: go1.26.5\n\tpath\texample.com/other/cmd/other\n\tmod\texample.com/other\tv1.0.0\th1:x=\n"
	f := goFake(dir, map[string]string{"goimports": goVersionM(t), "other": other})
	f.Responses["go install golang.org/x/tools/cmd/goimports@latest"] = runner.Result{}
	reg := goProxy(t, map[string]string{"golang.org/x/tools": "v0.21.0", "example.com/other": "v1.0.0"})
	if err := NewGo(f, reg).Upgrade(context.Background(), nil, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var installs []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "go install ") {
			installs = append(installs, c)
		}
	}
	if !reflect.DeepEqual(installs, []string{"go install golang.org/x/tools/cmd/goimports@latest"}) {
		t.Errorf("installs = %q", installs)
	}
}

func TestGoUpgradeChecksNames(t *testing.T) {
	dir := goBinDir(t, "evil")
	f := goFake(dir, map[string]string{"evil": "/x/evil: go1.26.5\n\tpath\t-toolexec=evil\n\tmod\texample.com/evil\tv1.0.0\th1:x=\n"})
	reg := goProxy(t, map[string]string{"example.com/evil": "v2.0.0"})
	p := NewGo(f, reg)
	for _, names := range [][]string{nil, {"evil"}, {"-bad"}} {
		err := p.Upgrade(context.Background(), names, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "invalid name") {
			t.Errorf("Upgrade(%v) err = %v, want invalid name", names, err)
		}
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "go install ") {
			t.Errorf("ran %q", c)
		}
	}
}

func TestGoUpgradeUnknownName(t *testing.T) {
	dir := goBinDir(t, "goimports")
	f := goFake(dir, map[string]string{"goimports": goVersionM(t)})
	err := NewGo(f, nil).Upgrade(context.Background(), []string{"nope"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("err = %v", err)
	}
}

func TestGoInstall(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"go install golang.org/x/tools/cmd/goimports@latest": {},
	}}
	p := NewGo(f, nil)
	if err := p.Install(context.Background(), "--bad", &bytes.Buffer{}); err == nil {
		t.Error("want error for flag-like name")
	}
	if err := p.Install(context.Background(), "golang.org/x/tools/cmd/goimports", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Calls, []string{"go install golang.org/x/tools/cmd/goimports@latest"}) {
		t.Errorf("Calls = %q", f.Calls)
	}
}

func TestGoSearchUnsupported(t *testing.T) {
	if _, err := NewGo(&runner.Fake{}, nil).Search(context.Background(), "x"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestGoWatchPathsAndAvailable(t *testing.T) {
	dir := t.TempDir()
	p := NewGo(goFake(dir, nil), nil)
	if got := p.WatchPaths(); !reflect.DeepEqual(got, []string{dir}) {
		t.Errorf("WatchPaths = %q", got)
	}
	f := &runner.Fake{Missing: map[string]bool{"go": true}}
	if q := NewGo(f, nil); q.Name() != "go" || q.Available() {
		t.Error("Name/Available wrong")
	}
}

// goErrRunner fails every `go version -m` with err, or with ctx.Err() when err
// is nil, standing in for a runner whose command timed out.
type goErrRunner struct {
	*runner.Fake
	err error
}

func (g goErrRunner) Run(ctx context.Context, name string, args ...string) (runner.Result, error) {
	if name == "go" && len(args) > 0 && args[0] == "version" {
		if g.err != nil {
			return runner.Result{}, g.err
		}
		if err := ctx.Err(); err != nil {
			return runner.Result{}, err
		}
	}
	return g.Fake.Run(ctx, name, args...)
}

func TestGoListReturnsRunnerAndContextErrors(t *testing.T) {
	dir := goBinDir(t, "a", "goimports")
	fake := goFake(dir, map[string]string{"goimports": goVersionM(t), "a": goVersionM(t)})

	boom := errors.New("boom")
	if _, err := NewGo(goErrRunner{Fake: fake, err: boom}, nil).List(context.Background()); !errors.Is(err, boom) {
		t.Errorf("runner error: err = %v, want boom", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tools, err := NewGo(goErrRunner{Fake: fake}, nil).List(ctx)
	if !errors.Is(err, context.Canceled) || tools != nil {
		t.Errorf("canceled ctx: tools = %+v, err = %v, want nil and context.Canceled", tools, err)
	}
}
