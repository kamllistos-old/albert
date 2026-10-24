package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kamllistos/albert/internal/dock"
	"github.com/kamllistos/albert/internal/state"
)

// lcDocker is a fake docker control that records what it was asked to do.
type lcDocker struct {
	readyErr, ensureErr, startErr error
	hasImage                      bool

	ensured []string
	started []string // "image ui agent"
	stops   int
}

func (d *lcDocker) Ready(context.Context) error { return d.readyErr }
func (d *lcDocker) HasImage(_ context.Context, image string) bool {
	return d.hasImage
}
func (d *lcDocker) EnsureImage(_ context.Context, image string, _ fs.FS) error {
	d.ensured = append(d.ensured, image)
	return d.ensureErr
}
func (d *lcDocker) Start(_ context.Context, image string, uiPort, agentPort int) error {
	d.started = append(d.started, image+" "+strconv.Itoa(uiPort)+" "+strconv.Itoa(agentPort))
	return d.startErr
}
func (d *lcDocker) Stop(context.Context) error { d.stops++; return errors.New("docker is not running") }

// lcWorld is the fake outside world of a lifecycle: processes, the agent's
// health endpoint and the browser.
type lcWorld struct {
	mu        sync.Mutex
	nextPID   int
	spawned   [][]string
	killed    []int
	alive     map[int]bool   // pid -> agent answers health
	cmdlines  map[int]string // pid -> ps command line
	healthy   bool           // health answers regardless of spawn (pre-existing agent)
	neverUp   bool           // spawned agents never answer health
	codes     int
	codeCalls []string // "port token"
	opened    []string
	uiDown    bool     // the Docker window never answers through Caddy
	uiCalls   []string // "port token"
	jobs      int      // queued or running jobs the agent reports
	jobsErr   error
	jobsCalls int
}

type lcHarness struct {
	l   *lifecycle
	d   *lcDocker
	w   *lcWorld
	out *bytes.Buffer
}

func newLifecycle(t *testing.T, stdin string, tty bool) *lcHarness {
	t.Helper()
	t.Setenv("ALBERT_HOME", t.TempDir())
	oldTimeout, oldInterval, oldUI := healthTimeout, healthInterval, uiReadyTimeout
	healthTimeout, healthInterval, uiReadyTimeout = 2*time.Second, time.Millisecond, 2*time.Second
	t.Cleanup(func() { healthTimeout, healthInterval, uiReadyTimeout = oldTimeout, oldInterval, oldUI })

	h := &lcHarness{
		d:   &lcDocker{},
		w:   &lcWorld{nextPID: 5000, alive: map[int]bool{}, cmdlines: map[int]string{}},
		out: &bytes.Buffer{},
	}
	w := h.w
	h.l = &lifecycle{
		dir:     state.Dir(),
		version: "dev",
		docker:  h.d,
		spawn: func(args []string, logPath string) (int, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.nextPID++
			pid := w.nextPID
			w.spawned = append(w.spawned, args)
			w.alive[pid] = !w.neverUp
			w.cmdlines[pid] = "/usr/local/bin/albert " + strings.Join(args, " ")
			return pid, nil
		},
		kill: func(pid int) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.killed = append(w.killed, pid)
			delete(w.alive, pid)
			delete(w.cmdlines, pid)
			return nil
		},
		procCmd: func(pid int) string {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.cmdlines[pid]
		},
		health: func(port int, token string) bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.healthy {
				return true
			}
			for _, up := range w.alive {
				if up {
					return true
				}
			}
			return false
		},
		uiHealth: func(port int, token string) bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.uiCalls = append(w.uiCalls, strconv.Itoa(port)+" "+token)
			return !w.uiDown
		},
		activeJobs: func(port int, token string) (int, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.jobsCalls++
			return w.jobs, w.jobsErr
		},
		mintCode: func(port int, token string) (string, error) {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.codes++
			w.codeCalls = append(w.codeCalls, strconv.Itoa(port)+" "+token)
			return "code" + strconv.Itoa(w.codes), nil
		},
		openURL: func(u string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.opened = append(w.opened, u)
			return nil
		},
		out: h.out,
		in:  strings.NewReader(stdin),
		tty: tty,
	}
	return h
}

// flagValue returns the value after name in args.
func flagValue(t *testing.T, args []string, name string) string {
	t.Helper()
	i := slices.Index(args, name)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("flag %s missing from %v", name, args)
	}
	return args[i+1]
}

func loadState(t *testing.T, dir string) (state.State, bool) {
	t.Helper()
	st, ok, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st, ok
}

func noFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s should not exist (err %v)", path, err)
	}
}

// seed writes a state file and token as a previous "albert up" would have.
func seed(t *testing.T, dir string, st state.State) string {
	t.Helper()
	if err := state.Save(dir, st); err != nil {
		t.Fatal(err)
	}
	tok, err := state.WriteToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestUpDockerMode(t *testing.T) {
	h := newLifecycle(t, "", false)
	if code := h.l.up(false, false); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if len(h.w.spawned) != 1 {
		t.Fatalf("spawned %v, want one agent", h.w.spawned)
	}
	args := h.w.spawned[0]
	if args[0] != "agent" || flagValue(t, args, "--mode") != "docker" {
		t.Fatalf("spawn args %v", args)
	}
	agentPort, _ := strconv.Atoi(flagValue(t, args, "--port"))
	uiPort, _ := strconv.Atoi(flagValue(t, args, "--ui-port"))
	if agentPort == 0 || uiPort == 0 || agentPort == uiPort {
		t.Fatalf("ports agent=%d ui=%d", agentPort, uiPort)
	}
	img := dock.Image("dev")
	if want := []string{img}; !slices.Equal(h.d.ensured, want) {
		t.Fatalf("EnsureImage %v, want %v", h.d.ensured, want)
	}
	if want := []string{img + " " + strconv.Itoa(uiPort) + " " + strconv.Itoa(agentPort)}; !slices.Equal(h.d.started, want) {
		t.Fatalf("Start %v, want %v", h.d.started, want)
	}
	st, ok := loadState(t, h.l.dir)
	if !ok || st.Mode != "docker" || st.PID != 5001 || st.AgentPort != agentPort || st.UIPort != uiPort || st.Started.IsZero() {
		t.Fatalf("state %+v (exists %v)", st, ok)
	}
	tok, err := state.ReadToken(h.l.dir)
	if err != nil || len(tok) != 64 {
		t.Fatalf("token %q, err %v", tok, err)
	}
	if want := []string{strconv.Itoa(agentPort) + " " + tok}; !slices.Equal(h.w.codeCalls, want) {
		t.Fatalf("login code minted with %v, want %v", h.w.codeCalls, want)
	}
	ui := "http://localhost:" + strconv.Itoa(uiPort)
	if want := []string{ui + "/#code=code1"}; !slices.Equal(h.w.opened, want) {
		t.Fatalf("opened %v, want %v", h.w.opened, want)
	}
	out := h.out.String()
	for _, s := range []string{"Preparing Albert's window (first time only)…", "Albert is running at " + ui + "\n", "Stop it any time with: albert down"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "code1") || strings.Contains(out, tok) {
		t.Errorf("output leaks the login code or token:\n%s", out)
	}
}

func TestUpDockerImagePresentSkipsPreparingMessage(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.d.hasImage = true
	if code := h.l.up(false, true); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if strings.Contains(h.out.String(), "Preparing") {
		t.Fatalf("printed Preparing for a present image:\n%s", h.out)
	}
}

func TestUpNoOpen(t *testing.T) {
	h := newLifecycle(t, "", false)
	if code := h.l.up(true, true); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if len(h.w.opened) != 0 || len(h.w.codeCalls) != 0 {
		t.Fatalf("--no-open still opened %v / minted %v", h.w.opened, h.w.codeCalls)
	}
	if !strings.Contains(h.out.String(), "Albert is running at http://localhost:") {
		t.Fatalf("missing URL:\n%s", h.out)
	}
}

func TestUpNativeFlag(t *testing.T) {
	h := newLifecycle(t, "", false)
	if code := h.l.up(true, false); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	args := h.w.spawned[0]
	if flagValue(t, args, "--mode") != "native" || flagValue(t, args, "--port") != flagValue(t, args, "--ui-port") {
		t.Fatalf("spawn args %v", args)
	}
	if len(h.d.ensured)+len(h.d.started) != 0 {
		t.Fatalf("native mode used docker: ensure %v start %v", h.d.ensured, h.d.started)
	}
	st, _ := loadState(t, h.l.dir)
	if st.Mode != "native" || st.UIPort != st.AgentPort {
		t.Fatalf("state %+v", st)
	}
	if want := "http://localhost:" + strconv.Itoa(st.UIPort) + "/#code=code1"; !slices.Equal(h.w.opened, []string{want}) {
		t.Fatalf("opened %v, want %v", h.w.opened, want)
	}
}

func TestUpFallsBackNativeNonTTY(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.d.readyErr = errors.New("docker is not running")
	if code := h.l.up(false, true); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if !strings.Contains(h.out.String(), "Docker isn't running — opening Albert without it.") {
		t.Fatalf("missing fallback message:\n%s", h.out)
	}
	if flagValue(t, h.w.spawned[0], "--mode") != "native" {
		t.Fatalf("spawn args %v", h.w.spawned[0])
	}
	if len(h.d.started) != 0 {
		t.Fatalf("Start called in native fallback: %v", h.d.started)
	}
	if st, _ := loadState(t, h.l.dir); st.Mode != "native" {
		t.Fatalf("state mode %q", st.Mode)
	}
}

func TestUpDockerDownTTYWaitsForEnter(t *testing.T) {
	h := newLifecycle(t, "\n", true)
	h.d.readyErr = errors.New("docker is not running")
	if code := h.l.up(false, true); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if !strings.Contains(h.out.String(), "Docker isn't running. Open Docker Desktop, or press Enter to open Albert without it.") {
		t.Fatalf("missing Docker-down prompt:\n%s", h.out)
	}
	if flagValue(t, h.w.spawned[0], "--mode") != "native" {
		t.Fatalf("spawn args %v", h.w.spawned[0])
	}
}

func TestUpDockerDownTTYCancelled(t *testing.T) {
	h := newLifecycle(t, "", true)
	r, w, _ := os.Pipe() // never written: the user does not press Enter
	t.Cleanup(func() { r.Close(); w.Close() })
	h.l.in = r
	h.d.readyErr = errors.New("docker is not running")
	ctx, cancel := context.WithCancel(context.Background())
	h.l.ctx = ctx
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if code := h.l.up(false, true); code == 0 {
		t.Fatal("up succeeded after Ctrl-C at the Docker prompt")
	}
	if len(h.w.spawned) != 0 {
		t.Fatalf("spawned after cancel: %v", h.w.spawned)
	}
}

func TestUpAlreadyRunning(t *testing.T) {
	h := newLifecycle(t, "", false)
	tok := seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7901, Mode: "docker", Started: time.Now()})
	h.w.healthy = true
	if code := h.l.up(false, false); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if len(h.w.spawned) != 0 || len(h.d.started) != 0 {
		t.Fatalf("already running but spawned %v / started %v", h.w.spawned, h.d.started)
	}
	if !strings.Contains(h.out.String(), "Albert is already running at http://localhost:7901\n") {
		t.Fatalf("output:\n%s", h.out)
	}
	if want := []string{"7900 " + tok}; !slices.Equal(h.w.codeCalls, want) {
		t.Fatalf("minted %v, want %v", h.w.codeCalls, want)
	}
	if want := []string{"http://localhost:7901/#code=code1"}; !slices.Equal(h.w.opened, want) {
		t.Fatalf("opened %v, want %v", h.w.opened, want)
	}
}

func TestIsRunningStalePID(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native", Started: time.Now()})
	if _, ok := isRunning(h.l); ok {
		t.Fatal("isRunning true although health fails")
	}
	if code := h.l.up(true, true); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if len(h.w.spawned) != 1 {
		t.Fatalf("spawned %v, want a fresh agent", h.w.spawned)
	}
	if len(h.w.killed) != 0 {
		t.Fatalf("killed %v: the stale PID is not running", h.w.killed)
	}
	st, _ := loadState(t, h.l.dir)
	if st.PID != 5001 {
		t.Fatalf("state %+v, want the new agent's", st)
	}
}

func TestUpStaleHungAgentIsKilled(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	h.w.cmdlines[4242] = "/usr/local/bin/albert agent --port 7900 --ui-port 7900 --mode native"
	if code := h.l.up(true, true); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if !slices.Equal(h.w.killed, []int{4242}) {
		t.Fatalf("killed %v, want the hung agent 4242", h.w.killed)
	}
}

func TestUpHealthTimeoutCleansUp(t *testing.T) {
	h := newLifecycle(t, "", false)
	healthTimeout = 30 * time.Millisecond
	h.w.neverUp = true
	if code := h.l.up(true, false); code != 1 {
		t.Fatalf("up = %d, want 1; out:\n%s", code, h.out)
	}
	if !slices.Equal(h.w.killed, []int{5001}) {
		t.Fatalf("killed %v, want the agent 5001", h.w.killed)
	}
	noFile(t, filepath.Join(h.l.dir, "state.json"))
	noFile(t, filepath.Join(h.l.dir, "token"))
	if want := "Albert couldn't start. Details: " + filepath.Join(h.l.dir, "albert.log"); !strings.Contains(h.out.String(), want) {
		t.Fatalf("output missing %q:\n%s", want, h.out)
	}
	if len(h.w.opened) != 0 {
		t.Fatalf("opened %v after a failed start", h.w.opened)
	}
}

func TestUpDockerStartFailureCleansUp(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.d.startErr = errors.New("docker run failed: port is taken")
	if code := h.l.up(false, false); code != 1 {
		t.Fatalf("up = %d, want 1; out:\n%s", code, h.out)
	}
	if !slices.Equal(h.w.killed, []int{5001}) {
		t.Fatalf("killed %v, want the agent 5001", h.w.killed)
	}
	noFile(t, filepath.Join(h.l.dir, "state.json"))
	noFile(t, filepath.Join(h.l.dir, "token"))
	if !strings.Contains(h.out.String(), "port is taken") {
		t.Fatalf("output missing the error:\n%s", h.out)
	}
}

func TestUpDockerChecksWindowThroughCaddy(t *testing.T) {
	h := newLifecycle(t, "", false)
	if code := h.l.up(false, true); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	uiPort := flagValue(t, h.w.spawned[0], "--ui-port")
	tok, _ := state.ReadToken(h.l.dir)
	if len(h.w.uiCalls) == 0 || h.w.uiCalls[0] != uiPort+" "+tok {
		t.Fatalf("window checks %v, want the UI port %s with the token", h.w.uiCalls, uiPort)
	}
}

func TestUpDockerWindowNotReadyFallsBackToNative(t *testing.T) {
	h := newLifecycle(t, "", false)
	uiReadyTimeout = 30 * time.Millisecond
	h.w.uiDown = true
	if code := h.l.up(false, false); code != 0 {
		t.Fatalf("up = %d, want 0 (opened without Docker); out:\n%s", code, h.out)
	}
	if len(h.d.started) != 1 || h.d.stops == 0 {
		t.Fatalf("container started %v, stopped %d times; want it started then stopped", h.d.started, h.d.stops)
	}
	if !slices.Equal(h.w.killed, []int{5001}) {
		t.Fatalf("killed %v, want the docker-mode agent 5001", h.w.killed)
	}
	if len(h.w.spawned) != 2 || flagValue(t, h.w.spawned[1], "--mode") != "native" {
		t.Fatalf("spawned %v, want a docker agent then a native one", h.w.spawned)
	}
	native := h.w.spawned[1]
	port := flagValue(t, native, "--port")
	if ui := flagValue(t, native, "--ui-port"); ui != port {
		t.Fatalf("native agent ports %s/%s, want the same", port, ui)
	}
	st, ok := loadState(t, h.l.dir)
	if !ok || st.Mode != "native" || st.PID != 5002 || strconv.Itoa(st.AgentPort) != port {
		t.Fatalf("state %+v (exists %v), want the native agent", st, ok)
	}
	out := h.out.String()
	if !strings.Contains(out, "Albert's window couldn't start in Docker. Opening it without Docker instead.\n") {
		t.Fatalf("output missing the fallback message:\n%s", out)
	}
	ui := "http://localhost:" + port
	if !strings.Contains(out, "Albert is running at "+ui+"\n") {
		t.Fatalf("output missing the native address:\n%s", out)
	}
	if want := []string{ui + "/#code=code1"}; !slices.Equal(h.w.opened, want) {
		t.Fatalf("opened %v, want %v (and never the Docker window)", h.w.opened, want)
	}
}

func TestUpDockerFallbackNativeFailureCleansUp(t *testing.T) {
	h := newLifecycle(t, "", false)
	uiReadyTimeout = 30 * time.Millisecond
	healthTimeout = 30 * time.Millisecond
	h.w.uiDown = true
	// The docker agent answers health; the native one never does.
	spawn := h.l.spawn
	h.l.spawn = func(args []string, logPath string) (int, error) {
		pid, err := spawn(args, logPath)
		if flagValue(t, args, "--mode") == "native" {
			h.w.mu.Lock()
			h.w.alive[pid] = false
			h.w.mu.Unlock()
		}
		return pid, err
	}
	if code := h.l.up(false, false); code != 1 {
		t.Fatalf("up = %d, want 1; out:\n%s", code, h.out)
	}
	if !slices.Equal(h.w.killed, []int{5001, 5002}) {
		t.Fatalf("killed %v, want both agents", h.w.killed)
	}
	noFile(t, filepath.Join(h.l.dir, "state.json"))
	noFile(t, filepath.Join(h.l.dir, "token"))
	if len(h.w.opened) != 0 {
		t.Fatalf("opened %v after a failed start", h.w.opened)
	}
}

func TestDownRefusesWhileUpdateRunningNonTTY(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	h.w.healthy = true
	h.w.jobs = 1
	if code := h.l.down(false); code != 1 {
		t.Fatalf("down = %d, want 1; out:\n%s", code, h.out)
	}
	if want := "An update is still running. Wait for it to finish, or run \"albert down --force\".\n"; h.out.String() != want {
		t.Fatalf("output %q, want %q", h.out, want)
	}
	if len(h.w.killed) != 0 {
		t.Fatalf("killed %v while an update was running", h.w.killed)
	}
	if _, ok := loadState(t, h.l.dir); !ok {
		t.Fatal("state cleared although Albert kept running")
	}
}

func TestDownTTYPromptDefaultsToNo(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", "no\n", ""} {
		h := newLifecycle(t, answer, true)
		seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
		h.w.healthy = true
		h.w.jobs = 2
		if code := h.l.down(false); code != 1 {
			t.Fatalf("answer %q: down = %d, want 1; out:\n%s", answer, code, h.out)
		}
		if !strings.Contains(h.out.String(), "An update is still running. Stop Albert anyway? [y/N]") {
			t.Fatalf("answer %q: missing prompt:\n%s", answer, h.out)
		}
		if len(h.w.killed) != 0 || strings.Contains(h.out.String(), "Albert stopped.") {
			t.Fatalf("answer %q: Albert was stopped:\n%s", answer, h.out)
		}
	}
}

func TestDownTTYPromptYesStops(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n"} {
		h := newLifecycle(t, answer, true)
		seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
		h.w.healthy = true
		h.w.jobs = 1
		if code := h.l.down(false); code != 0 {
			t.Fatalf("answer %q: down = %d; out:\n%s", answer, code, h.out)
		}
		if !slices.Equal(h.w.killed, []int{4242}) || !strings.HasSuffix(h.out.String(), "Albert stopped.\n") {
			t.Fatalf("answer %q: killed %v, out:\n%s", answer, h.w.killed, h.out)
		}
	}
}

func TestDownForceSkipsJobCheck(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	h.w.healthy = true
	h.w.jobs = 1
	if code := h.l.down(true); code != 0 {
		t.Fatalf("down --force = %d; out:\n%s", code, h.out)
	}
	if h.w.jobsCalls != 0 {
		t.Fatalf("--force still asked for jobs %d times", h.w.jobsCalls)
	}
	if !slices.Equal(h.w.killed, []int{4242}) || h.out.String() != "Albert stopped.\n" {
		t.Fatalf("killed %v, out %q", h.w.killed, h.out)
	}
}

func TestDownChecksJobsWithToken(t *testing.T) {
	h := newLifecycle(t, "", false)
	tok := seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	h.w.healthy = true
	var got string
	h.l.activeJobs = func(port int, token string) (int, error) {
		got = strconv.Itoa(port) + " " + token
		return 0, nil
	}
	if code := h.l.down(false); code != 0 {
		t.Fatalf("down = %d; out:\n%s", code, h.out)
	}
	if got != "7900 "+tok {
		t.Fatalf("jobs asked with %q, want the agent port and token", got)
	}
	if h.out.String() != "Albert stopped.\n" {
		t.Fatalf("output %q", h.out)
	}
}

func TestDownKillsAndClears(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	h.w.healthy = true
	if code := h.l.down(false); code != 0 {
		t.Fatalf("down = %d, out:\n%s", code, h.out)
	}
	if !slices.Equal(h.w.killed, []int{4242}) {
		t.Fatalf("killed %v", h.w.killed)
	}
	if h.d.stops != 0 {
		t.Fatalf("native down stopped docker %d times", h.d.stops)
	}
	noFile(t, filepath.Join(h.l.dir, "state.json"))
	noFile(t, filepath.Join(h.l.dir, "token"))
	if h.out.String() != "Albert stopped.\n" {
		t.Fatalf("output %q", h.out)
	}
}

func TestDownNothingRunning(t *testing.T) {
	h := newLifecycle(t, "", false)
	if code := h.l.down(false); code != 0 {
		t.Fatalf("down = %d, out:\n%s", code, h.out)
	}
	if h.d.stops != 1 {
		t.Fatalf("stops = %d, want a best-effort container stop", h.d.stops)
	}
	if len(h.w.killed) != 0 {
		t.Fatalf("killed %v", h.w.killed)
	}
	if h.out.String() != "Albert isn't running.\n" {
		t.Fatalf("output %q", h.out)
	}
}

func TestDownAfterCrashStillStopsContainer(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7901, Mode: "docker"})
	if code := h.l.down(false); code != 0 {
		t.Fatalf("down = %d, out:\n%s", code, h.out)
	}
	if h.d.stops != 1 {
		t.Fatalf("stops = %d, want 1", h.d.stops)
	}
	if len(h.w.killed) != 0 {
		t.Fatalf("killed %v: the agent is gone", h.w.killed)
	}
	noFile(t, filepath.Join(h.l.dir, "state.json"))
	if h.out.String() != "Albert stopped.\n" {
		t.Fatalf("output %q", h.out)
	}
}

func TestDownStalePIDOfAnotherProcessIsNotKilled(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	h.w.cmdlines[4242] = "/usr/bin/vim notes.txt" // the PID was reused
	if code := h.l.down(false); code != 0 {
		t.Fatalf("down = %d, out:\n%s", code, h.out)
	}
	if len(h.w.killed) != 0 {
		t.Fatalf("killed %v: PID 4242 is not Albert", h.w.killed)
	}
	noFile(t, filepath.Join(h.l.dir, "state.json"))
}

func TestDownKillsUnhealthyAlbertAgent(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	h.w.cmdlines[4242] = "/Users/me/bin/albert agent --port 7900 --ui-port 7900 --mode native"
	if code := h.l.down(false); code != 0 {
		t.Fatalf("down = %d, out:\n%s", code, h.out)
	}
	if !slices.Equal(h.w.killed, []int{4242}) {
		t.Fatalf("killed %v, want 4242", h.w.killed)
	}
}

func TestDownKillFailureKeepsState(t *testing.T) {
	h := newLifecycle(t, "", false)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	h.w.healthy = true
	h.l.kill = func(int) error { return errors.New("operation not permitted") }
	if code := h.l.down(false); code != 1 {
		t.Fatalf("down = %d, want 1; out:\n%s", code, h.out)
	}
	if _, ok := loadState(t, h.l.dir); !ok {
		t.Fatal("state cleared although the agent is still running")
	}
}

func TestStatus(t *testing.T) {
	h := newLifecycle(t, "", false)
	if code := h.l.status(); code != 1 {
		t.Fatalf("status = %d, want 1 when not running", code)
	}
	if h.out.String() != "Albert isn't running. Start it with: albert up\n" {
		t.Fatalf("output %q", h.out)
	}

	h.out.Reset()
	started := time.Date(2026, 10, 23, 15, 4, 0, 0, time.Local)
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7901, Mode: "docker", Started: started})
	h.w.healthy = true
	if code := h.l.status(); code != 0 {
		t.Fatalf("status = %d, want 0", code)
	}
	if want := "Running at http://localhost:7901 (mode: docker, since 15:04)\n"; h.out.String() != want {
		t.Fatalf("output %q, want %q", h.out, want)
	}
}

func TestOwnsAgentCommandLine(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/usr/local/bin/albert agent --port 7878 --ui-port 7879 --mode docker":     true,
		"/Users/me/My Tools/albert agent --port 7878 --ui-port 7878 --mode native": true,
		"":                         false,
		"/usr/bin/vim agent.go":    false,
		"/usr/local/bin/albert up": false,
	} {
		if got := isAgentCommand(cmd); got != want {
			t.Errorf("isAgentCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}

func TestHTTPHealthAndLoginCode(t *testing.T) {
	const tok = "secret-token"
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		if r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/health":
			w.Write([]byte(`{"ok":true}`))
		case r.Method == "POST" && r.URL.Path == "/api/login-code" && r.Header.Get("X-Albert-Request") == "1":
			w.Write([]byte(`{"code":"abc123"}`))
		default:
			http.Error(w, `{"error":"bad"}`, http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	if !httpHealth(port, tok) {
		t.Fatal("httpHealth false against a healthy agent")
	}
	if httpHealth(port, "wrong") {
		t.Fatal("httpHealth true with a wrong token")
	}
	code, err := httpLoginCode(port, tok)
	if err != nil || code != "abc123" {
		t.Fatalf("httpLoginCode = %q, %v", code, err)
	}
	if _, err := httpLoginCode(port, "wrong"); err == nil {
		t.Fatal("httpLoginCode succeeded with a wrong token")
	}
	want := "127.0.0.1:" + u.Port()
	for _, h := range hosts {
		if h != want {
			t.Fatalf("Host %q, want %q", h, want)
		}
	}
}

func TestHTTPWindowHealthGoesThroughUIPort(t *testing.T) {
	const tok = "secret-token"
	var host string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		if r.URL.Path != "/api/health" || r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	if !httpUIHealth(port, tok) {
		t.Fatal("httpUIHealth false against a working window")
	}
	if host != "localhost:"+u.Port() {
		t.Fatalf("Host %q, want localhost:%s (the UI's own address)", host, u.Port())
	}
	if httpUIHealth(port, "wrong") {
		t.Fatal("httpUIHealth true with a wrong token")
	}
}

func TestHTTPActiveJobs(t *testing.T) {
	const tok = "secret-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/jobs" || r.Header.Get("Authorization") != "Bearer "+tok {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"jobs":[{"status":"running"},{"status":"success"},{"status":"queued"},{"status":"failed"}]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())
	n, err := httpActiveJobs(port, tok)
	if err != nil || n != 2 {
		t.Fatalf("httpActiveJobs = %d, %v; want 2 (queued + running)", n, err)
	}
	if _, err := httpActiveJobs(port, "wrong"); err == nil {
		t.Fatal("httpActiveJobs succeeded with a wrong token")
	}
}

func TestKillProcess(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	if err := killProcess(cmd.Process.Pid); err != nil {
		t.Fatalf("killProcess: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process still running after killProcess")
	}
	if err := killProcess(cmd.Process.Pid); err != nil {
		t.Fatalf("killProcess on a gone process: %v", err)
	}
	for _, pid := range []int{0, 1, -1} {
		if err := killProcess(pid); err == nil {
			t.Fatalf("killProcess(%d) accepted", pid)
		}
	}
}

func TestAgentCommandRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--port", "7878", "--mode", "elsewhere"},
		{"--mode", "native"},
		{"--port", "7878", "extra"},
	} {
		h := newApp("", false)
		if code := cmdAgent(h.a, args); code != exitUsage {
			t.Errorf("agent %v = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestUpSavesStateRightAfterSpawn(t *testing.T) {
	h := newLifecycle(t, "", false)
	var seen []state.State
	healthy := h.l.health
	h.l.health = func(port int, token string) bool {
		if len(h.w.spawned) > 0 { // only the waits after spawn
			st, ok := loadState(t, h.l.dir)
			if !ok {
				t.Error("no state file while waiting for the new agent")
			}
			seen = append(seen, st)
		}
		return healthy(port, token)
	}
	if code := h.l.up(false, true); code != 0 {
		t.Fatalf("up = %d, out:\n%s", code, h.out)
	}
	if len(seen) == 0 {
		t.Fatal("health never checked after spawn")
	}
	agentPort, _ := strconv.Atoi(flagValue(t, h.w.spawned[0], "--port"))
	uiPort, _ := strconv.Atoi(flagValue(t, h.w.spawned[0], "--ui-port"))
	if st := seen[0]; st.PID != 5001 || st.AgentPort != agentPort || st.UIPort != uiPort || st.Mode != "docker" {
		t.Fatalf("state before health OK = %+v", st)
	}
}

func TestUpCancelledWhileWaitingKillsAgent(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.w.neverUp = true
	ctx, cancel := context.WithCancel(context.Background())
	h.l.ctx = ctx
	go func() { time.Sleep(20 * time.Millisecond); cancel() }() // Terminal closed / Ctrl-C
	if code := h.l.up(true, false); code != 1 {
		t.Fatalf("up = %d, want 1; out:\n%s", code, h.out)
	}
	if !slices.Equal(h.w.killed, []int{5001}) {
		t.Fatalf("killed %v, want the agent 5001", h.w.killed)
	}
	noFile(t, filepath.Join(h.l.dir, "state.json"))
	noFile(t, filepath.Join(h.l.dir, "token"))
}

func TestCLISignalsCancelContext(t *testing.T) {
	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		if !slices.Contains(cliSignals, sig) {
			t.Errorf("cliSignals %v missing %v", cliSignals, sig)
		}
	}
}
