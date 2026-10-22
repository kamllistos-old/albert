package dock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/kamllistos/albert/internal/runner"
)

func ok(out string) runner.Result { return runner.Result{Stdout: []byte(out)} }

func fail(msg string) runner.Result { return runner.Result{ExitCode: 1, Stderr: []byte(msg)} }

func hasCall(f *runner.Fake, key string) bool {
	for _, c := range f.Calls {
		if c == key {
			return true
		}
	}
	return false
}

func TestImage(t *testing.T) {
	if got := Image("dev"); got != "albert-ui:dev" {
		t.Fatalf("Image(dev) = %q", got)
	}
	if got := Image("1.2.3"); got != "ghcr.io/kamllistos/albert-ui:1.2.3" {
		t.Fatalf("Image(1.2.3) = %q", got)
	}
}

func TestReadyFailsWhenDockerMissing(t *testing.T) {
	f := &runner.Fake{Missing: map[string]bool{"docker": true}}
	if err := (Docker{R: f}).Ready(context.Background()); err == nil {
		t.Fatal("want error when docker is not installed")
	}
}

func TestReadyFailsWhenDaemonDown(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker info --format {{.ServerVersion}}": fail("Cannot connect to the Docker daemon"),
	}}
	if err := (Docker{R: f}).Ready(context.Background()); err == nil {
		t.Fatal("want error when the daemon is down")
	}
}

func TestReadyOK(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker info --format {{.ServerVersion}}": ok("27.0.1\n"),
	}}
	if err := (Docker{R: f}).Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureImageUsesExisting(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker image inspect ghcr.io/kamllistos/albert-ui:1.0.0": ok("[]"),
	}}
	if err := (Docker{R: f}).EnsureImage(context.Background(), "ghcr.io/kamllistos/albert-ui:1.0.0", fstest.MapFS{}); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 {
		t.Fatalf("calls = %v, want only the inspect", f.Calls)
	}
}

func TestEnsureImagePullsRegistryImage(t *testing.T) {
	img := "ghcr.io/kamllistos/albert-ui:1.0.0"
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker image inspect " + img: fail("No such image"),
		"docker pull " + img:          ok("pulled"),
	}}
	if err := (Docker{R: f}).EnsureImage(context.Background(), img, fstest.MapFS{}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "docker build") {
			t.Fatalf("unexpected build after successful pull: %v", f.Calls)
		}
	}
}

func TestEnsureImageDevBuildsLocally(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "ctx")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker image inspect albert-ui:dev":                                    fail("No such image"),
		"docker build -t albert-ui:dev -f " + tmp + "/docker/Dockerfile " + tmp: ok("built"),
	}}
	d := Docker{R: f}
	d.mkTemp = func() (string, error) { return tmp, nil }
	if err := d.EnsureImage(context.Background(), "albert-ui:dev", buildFS()); err != nil {
		t.Fatal(err)
	}
	if hasCall(f, "docker pull albert-ui:dev") {
		t.Fatal("a local image must not be pulled")
	}
	if !hasCall(f, "docker build -t albert-ui:dev -f "+tmp+"/docker/Dockerfile "+tmp) {
		t.Fatalf("calls = %v, want a docker build with an absolute -f <tmp>/docker/Dockerfile", f.Calls)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("temp build dir was not cleaned up: %v", err)
	}
}

func TestEnsureImageFallsBackToBuildWhenPullFails(t *testing.T) {
	img := "ghcr.io/kamllistos/albert-ui:1.0.0"
	tmp := filepath.Join(t.TempDir(), "ctx")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker image inspect " + img: fail("No such image"),
		"docker pull " + img:          fail("denied"),
		"docker build -t " + img + " -f " + tmp + "/docker/Dockerfile " + tmp: ok("built"),
	}}
	d := Docker{R: f}
	d.mkTemp = func() (string, error) { return tmp, nil }
	if err := d.EnsureImage(context.Background(), img, buildFS()); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureImageBuildFailureReportsOutput(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "ctx")
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker image inspect albert-ui:dev":                                    fail("No such image"),
		"docker build -t albert-ui:dev -f " + tmp + "/docker/Dockerfile " + tmp: fail("boom on step 2"),
	}}
	d := Docker{R: f}
	d.mkTemp = func() (string, error) { return tmp, nil }
	err := d.EnsureImage(context.Background(), "albert-ui:dev", buildFS())
	if err == nil || !strings.Contains(err.Error(), "boom on step 2") {
		t.Fatalf("err = %v, want it to include the build output", err)
	}
}

func TestCopyBuildContext(t *testing.T) {
	dst := t.TempDir()
	if err := copyBuildContext(buildFS(), dst); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"web/index.html":    "<html>",
		"web/assets/app.js": "js",
		"docker/Dockerfile": "FROM x",
		"docker/Caddyfile":  ":80",
	} {
		b, err := os.ReadFile(filepath.Join(dst, path))
		if err != nil || string(b) != want {
			t.Fatalf("%s = %q, %v; want %q", path, b, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "other.txt")); !os.IsNotExist(err) {
		t.Fatal("files outside web/ and docker/ must not be copied")
	}
}

func buildFS() fstest.MapFS {
	return fstest.MapFS{
		"web/index.html":    {Data: []byte("<html>")},
		"web/assets/app.js": {Data: []byte("js")},
		"docker/Dockerfile": {Data: []byte("FROM x")},
		"docker/Caddyfile":  {Data: []byte(":80")},
		"other.txt":         {Data: []byte("no")},
	}
}

func TestStartArgs(t *testing.T) {
	run := "docker run -d --rm --name albert-ui -p 127.0.0.1:7879:80 -e ALBERT_AGENT=host.docker.internal:7878 albert-ui:dev"
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker rm -f albert-ui": fail("No such container: albert-ui"),
		run:                      ok("abc123\n"),
	}}
	if err := (Docker{R: f}).Start(context.Background(), "albert-ui:dev", 7879, 7878); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 2 || f.Calls[0] != "docker rm -f albert-ui" || f.Calls[1] != run {
		t.Fatalf("calls = %v", f.Calls)
	}
}

func TestStartReportsRunFailure(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker rm -f albert-ui": ok(""),
		"docker run -d --rm --name albert-ui -p 127.0.0.1:7879:80 -e ALBERT_AGENT=host.docker.internal:7878 albert-ui:dev": fail("port is already allocated"),
	}}
	err := (Docker{R: f}).Start(context.Background(), "albert-ui:dev", 7879, 7878)
	if err == nil || !strings.Contains(err.Error(), "port is already allocated") {
		t.Fatalf("err = %v", err)
	}
}

func TestStopIgnoresMissing(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker stop albert-ui": fail("Error response from daemon: No such container: albert-ui"),
	}}
	if err := (Docker{R: f}).Stop(context.Background()); err != nil {
		t.Fatalf("Stop on a missing container = %v, want nil", err)
	}
}

func TestStopReportsOtherFailures(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{
		"docker stop albert-ui": fail("permission denied"),
	}}
	if err := (Docker{R: f}).Stop(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestStopOK(t *testing.T) {
	f := &runner.Fake{Responses: map[string]runner.Result{"docker stop albert-ui": ok("albert-ui")}}
	if err := (Docker{R: f}).Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
