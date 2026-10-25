package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kamllistos/albert"
	"github.com/kamllistos/albert/internal/agent"
	"github.com/kamllistos/albert/internal/dock"
	"github.com/kamllistos/albert/internal/jobs"
	"github.com/kamllistos/albert/internal/runner"
	"github.com/kamllistos/albert/internal/state"
)

const (
	defaultAgentPort = 7878
	defaultUIPort    = 7879
	logName          = "albert.log"
)

// Package vars so tests run fast.
var (
	healthTimeout  = 10 * time.Second       // how long up waits for a new agent
	healthInterval = 200 * time.Millisecond // how often it asks
	uiReadyTimeout = 10 * time.Second       // how long up waits for the Docker window
	killWait       = 3 * time.Second        // SIGTERM grace before SIGKILL
)

// dockerControl is what Albert needs from Docker.
type dockerControl interface {
	Ready(ctx context.Context) error
	HasImage(ctx context.Context, image string) bool
	EnsureImage(ctx context.Context, image string, files fs.FS) error
	Start(ctx context.Context, image string, uiPort, agentPort int) error
	Stop(ctx context.Context) error
	// RemoveImages deletes Albert's window images (this version's, the dev
	// build and every registry tag).
	RemoveImages(ctx context.Context, version string) error
}

// lifecycle starts, stops and reports on the background agent (and, in
// docker mode, the GUI container). Every outside effect is a field so tests
// can fake it.
type lifecycle struct {
	dir     string
	version string
	ctx     context.Context // nil means context.Background()
	docker  dockerControl
	spawn   func(args []string, logPath string) (pid int, err error) // start a detached "albert <args>"
	kill    func(pid int) error                                      // SIGTERM, wait, SIGKILL
	procCmd func(pid int) string                                     // pid's command line, "" if gone
	health  func(port int, token string) bool                        // agent answers /api/health
	// uiHealth reports whether the agent answers /api/health through the
	// Docker window (Caddy) on the UI port.
	uiHealth func(port int, token string) bool
	// activeJobs counts the agent's queued and running jobs.
	activeJobs func(port int, token string) (int, error)
	// mintCode asks the agent for a one-time browser sign-in code.
	mintCode func(port int, token string) (string, error)
	openURL  func(url string) error
	out      io.Writer
	in       io.Reader
	tty      bool

	// What uninstall needs from the machine.
	executable func() (string, error)  // this program's path
	homeDir    func() (string, error)  // the user's home folder
	removeFile func(path string) error // never recursive

	br *bufio.Reader // l.in, buffered once so successive questions share it
}

func (l *lifecycle) context() context.Context {
	if l.ctx == nil {
		return context.Background()
	}
	return l.ctx
}

func uiURL(port int) string { return "http://localhost:" + strconv.Itoa(port) }

// isRunning reports the saved state and whether its agent answers health
// checks with the saved token.
func isRunning(l *lifecycle) (state.State, bool) {
	st, ok, err := state.Load(l.dir)
	if err != nil || !ok {
		return st, false
	}
	tok, err := state.ReadToken(l.dir)
	if err != nil || tok == "" {
		return st, false
	}
	return st, l.health(st.AgentPort, tok)
}

func (l *lifecycle) up(native, noOpen bool) int {
	ctx := l.context()
	if st, ok := isRunning(l); ok {
		fmt.Fprintf(l.out, "Albert is already running at %s\n", uiURL(st.UIPort))
		if !noOpen {
			tok, _ := state.ReadToken(l.dir)
			l.open(st, tok)
		}
		return exitOK
	} else if _, exists, _ := state.Load(l.dir); exists {
		if err := l.cleanup(st, false); err != nil {
			fmt.Fprintf(l.out, "Albert couldn't stop its old copy (process %d): %v\n", st.PID, err)
			return exitFail
		}
	}

	mode := "docker"
	if native {
		mode = "native"
	} else if l.docker.Ready(ctx) != nil {
		mode = "native"
		if l.tty {
			fmt.Fprintln(l.out, "Docker isn't running. Open Docker Desktop, or press Enter to open Albert without it.")
			if !l.waitEnter(ctx) {
				return exitFail
			}
		} else {
			fmt.Fprintln(l.out, "Docker isn't running — opening Albert without it.")
		}
	}

	st, ok := l.startAgent(ctx, mode)
	if !ok {
		return exitFail
	}

	if mode == "docker" {
		image := dock.Image(l.version)
		if !l.docker.HasImage(ctx, image) {
			fmt.Fprintln(l.out, "Preparing Albert's window (first time only)…")
		}
		err := l.docker.EnsureImage(ctx, image, albert.Files)
		if err == nil {
			err = l.docker.Start(ctx, image, st.UIPort, st.AgentPort)
		}
		if err != nil {
			fmt.Fprintf(l.out, "Albert couldn't open its window in Docker: %v\n", err)
			fmt.Fprintln(l.out, "Try again, or open Albert without Docker: albert up --native")
			l.stopAgent(st.PID)
			return exitFail
		}
		// The container started, but that doesn't mean the window works
		// (for example, it can't reach the agent). Check through Caddy
		// before opening the browser, and fall back to the native window.
		if !l.waitUI(ctx, st.UIPort, st.token) {
			if ctx.Err() != nil {
				_ = l.docker.Stop(context.Background())
				l.stopAgent(st.PID)
				return exitFail
			}
			_ = l.docker.Stop(ctx)
			l.stopAgent(st.PID)
			fmt.Fprintln(l.out, "Albert's window couldn't start in Docker. Opening it without Docker instead.")
			if st, ok = l.startAgent(ctx, "native"); !ok {
				return exitFail
			}
		}
	}

	fmt.Fprintf(l.out, "Albert is running at %s\n", uiURL(st.UIPort))
	fmt.Fprintln(l.out, "Stop it any time with: albert down")
	if !noOpen {
		l.open(st.State, st.token)
	}
	return exitOK
}

// started is a freshly started agent and its token.
type started struct {
	state.State
	token string
}

// startAgent picks ports, writes a new token, starts a detached agent in mode,
// saves the state and waits for the agent to answer. On failure it explains
// why, stops the agent and clears the state, and reports false.
func (l *lifecycle) startAgent(ctx context.Context, mode string) (started, bool) {
	agentPort, err := state.FreePort(defaultAgentPort)
	if err != nil {
		fmt.Fprintf(l.out, "Albert couldn't find a free port: %v\n", err)
		return started{}, false
	}
	uiPort := agentPort
	if mode == "docker" {
		// Nothing is listening yet, so FreePort could hand out the agent's
		// port again.
		pref := defaultUIPort
		if pref == agentPort {
			pref++
		}
		if uiPort, err = state.FreePort(pref); err == nil && uiPort == agentPort {
			uiPort, err = state.FreePort(agentPort + 1)
		}
		if err != nil {
			fmt.Fprintf(l.out, "Albert couldn't find a free port: %v\n", err)
			return started{}, false
		}
	}
	tok, err := state.WriteToken(l.dir)
	if err != nil {
		fmt.Fprintf(l.out, "Albert couldn't save its settings in %s: %v\n", l.dir, err)
		return started{}, false
	}

	logPath := filepath.Join(l.dir, logName)
	pid, err := l.spawn([]string{"agent",
		"--port", strconv.Itoa(agentPort),
		"--ui-port", strconv.Itoa(uiPort),
		"--mode", mode,
	}, logPath)
	if err != nil {
		_ = state.Clear(l.dir)
		fmt.Fprintf(l.out, "Albert couldn't start: %v\n", err)
		return started{}, false
	}
	// From here on any failure must stop the agent we just started.
	// Save state at once, so "albert down" can find the agent even if this
	// process dies (Terminal closed, SIGKILL) before it is ready.
	st := state.State{PID: pid, AgentPort: agentPort, UIPort: uiPort, Mode: mode, Started: time.Now()}
	if err := state.Save(l.dir, st); err != nil {
		fmt.Fprintf(l.out, "Albert couldn't save its settings in %s: %v\n", l.dir, err)
		l.stopAgent(pid)
		return started{}, false
	}
	if !l.waitHealthy(ctx, agentPort, tok) {
		fmt.Fprintf(l.out, "Albert couldn't start. Details: %s\n", logPath)
		l.stopAgent(pid)
		return started{}, false
	}
	return started{State: st, token: tok}, true
}

// stopAgent stops an agent this up started and clears the state files.
func (l *lifecycle) stopAgent(pid int) {
	_ = l.kill(pid)
	_ = state.Clear(l.dir)
}

// waitUI polls the agent's health through the Docker window until it
// answers, uiReadyTimeout passes or ctx ends.
func (l *lifecycle) waitUI(ctx context.Context, port int, tok string) bool {
	return poll(ctx, uiReadyTimeout, func() bool { return l.uiHealth(port, tok) })
}

// waitEnter waits for a line on l.in (or end of input). It reports false if
// ctx ends first.
func (l *lifecycle) waitEnter(ctx context.Context) bool {
	_, ok := l.readLine(ctx)
	return ok
}

// readLine reads one line from l.in (or up to end of input), without its
// line ending. It reports false if ctx ends first.
func (l *lifecycle) readLine(ctx context.Context) (string, bool) {
	if l.br == nil {
		l.br = bufio.NewReader(l.in)
	}
	br := l.br
	done := make(chan string, 1)
	go func() {
		line, _ := br.ReadString('\n')
		done <- strings.TrimRight(line, "\r\n")
	}()
	select {
	case line := <-done:
		return line, true
	case <-ctx.Done():
		return "", false
	}
}

// waitHealthy polls the agent's health until it answers, healthTimeout
// passes or ctx ends.
func (l *lifecycle) waitHealthy(ctx context.Context, port int, tok string) bool {
	return poll(ctx, healthTimeout, func() bool { return l.health(port, tok) })
}

// poll calls ok every healthInterval until it reports true (true), timeout
// passes or ctx ends (false).
func poll(ctx context.Context, timeout time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if ok() {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(healthInterval):
		}
	}
}

// open signs the browser in with a fresh one-time code and opens the UI. The
// code goes in the URL fragment, which the browser never sends to a server
// or in a Referer; the page trades it at POST /api/session. The code is
// never printed.
func (l *lifecycle) open(st state.State, tok string) {
	u := uiURL(st.UIPort)
	target := u
	if code, err := l.mintCode(st.AgentPort, tok); err == nil {
		target = u + "/#code=" + url.QueryEscape(code)
	}
	if err := l.openURL(target); err != nil {
		fmt.Fprintf(l.out, "Open %s in your browser.\n", u)
	}
}

// stopResult is how stopAlbert ended.
type stopResult int

const (
	stopNotRunning stopResult = iota // nothing was running
	stopDone                         // Albert was running and is now stopped
	stopDeclined                     // an update is running and Albert was left alone
	stopFailed                       // Albert couldn't be stopped
)

// stopAlbert stops Albert and says why when it doesn't. Unless force is set,
// it first asks the agent whether an update or install is still going:
// stopping would cut a package manager off mid-install. In a terminal it
// asks; otherwise it refuses. It prints no success message: down and
// uninstall each say their own.
func (l *lifecycle) stopAlbert(force bool) stopResult {
	st, exists, err := state.Load(l.dir)
	if err != nil {
		exists = true // a damaged state file still means something may be running
	}
	if !exists {
		_ = l.docker.Stop(l.context()) // a container left behind by a lost state file
		return stopNotRunning
	}
	_, healthy := isRunning(l)
	if healthy && !force && l.jobsRunning(st) {
		if !l.tty {
			fmt.Fprintln(l.out, `An update is still running. Wait for it to finish, or run "albert down --force".`)
			return stopDeclined
		}
		fmt.Fprint(l.out, "An update is still running. Stop Albert anyway? [y/N] ")
		answer, ok := l.readLine(l.context())
		if a := strings.ToLower(strings.TrimSpace(answer)); !ok || (a != "y" && a != "yes") {
			return stopDeclined
		}
	}
	if err := l.cleanup(st, healthy); err != nil {
		fmt.Fprintf(l.out, "Albert couldn't stop (process %d): %v\n", st.PID, err)
		return stopFailed
	}
	return stopDone
}

// down stops Albert; see stopAlbert for when it asks first.
func (l *lifecycle) down(force bool) int {
	switch l.stopAlbert(force) {
	case stopNotRunning:
		fmt.Fprintln(l.out, "Albert isn't running.")
	case stopDone:
		fmt.Fprintln(l.out, "Albert stopped.")
	default:
		return exitFail
	}
	return exitOK
}

// jobsRunning reports whether the agent described by st has queued or
// running jobs. If it can't tell, it reports false: down must still work.
func (l *lifecycle) jobsRunning(st state.State) bool {
	tok, err := state.ReadToken(l.dir)
	if err != nil {
		return false
	}
	n, err := l.activeJobs(st.AgentPort, tok)
	return err == nil && n > 0
}

// cleanup stops what st describes and clears the state files. The PID is
// only killed if it is still Albert's agent: it answered health checks, or
// its command line says so. A PID reused by another program is left alone.
func (l *lifecycle) cleanup(st state.State, healthy bool) error {
	if st.Mode == "docker" {
		_ = l.docker.Stop(l.context())
	}
	if st.PID > 1 && (healthy || isAgentCommand(l.procCmd(st.PID))) {
		if err := l.kill(st.PID); err != nil {
			return err
		}
	}
	return state.Clear(l.dir)
}

// isAgentCommand reports whether a ps command line is an "albert agent"
// started by up.
func isAgentCommand(cmd string) bool {
	return strings.Contains(cmd, "albert") && strings.Contains(cmd, " agent --port ")
}

func (l *lifecycle) status() int {
	st, ok := isRunning(l)
	if !ok {
		fmt.Fprintln(l.out, "Albert isn't running. Start it with: albert up")
		return exitFail
	}
	fmt.Fprintf(l.out, "Running at %s (mode: %s, since %s)\n", uiURL(st.UIPort), st.Mode, st.Started.Local().Format("15:04"))
	return exitOK
}

// --- commands ---

func (a *app) lifecycle() *lifecycle {
	return &lifecycle{
		dir:        state.Dir(),
		version:    version,
		ctx:        a.ctx,
		docker:     a.docker,
		spawn:      spawnDetached,
		kill:       killProcess,
		procCmd:    processCommand,
		health:     httpHealth,
		uiHealth:   httpUIHealth,
		activeJobs: httpActiveJobs,
		mintCode:   httpLoginCode,
		openURL:    func(u string) error { return exec.Command("open", u).Run() },
		out:        a.stdout,
		in:         a.stdin,
		tty:        a.isTTY,
		executable: os.Executable,
		homeDir:    os.UserHomeDir,
		removeFile: os.Remove,
	}
}

func cmdUp(a *app, args []string) int {
	flags := flag.NewFlagSet("up", flag.ContinueOnError)
	native := flags.Bool("native", false, "open Albert without Docker")
	noOpen := flags.Bool("no-open", false, "don't open the browser")
	pos, code := a.parseFlags(flags, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		return a.usagef("albert up takes no arguments. Options: --native, --no-open.")
	}
	return a.lifecycle().up(*native, *noOpen)
}

func cmdDown(a *app, args []string) int {
	flags := flag.NewFlagSet("down", flag.ContinueOnError)
	force := flags.Bool("force", false, "stop even if an update is still running")
	pos, code := a.parseFlags(flags, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		return a.usagef("albert down takes no arguments. Option: --force.")
	}
	return a.lifecycle().down(*force)
}

func cmdStatus(a *app, args []string) int {
	if len(args) > 0 {
		return a.usagef("albert status takes no arguments.")
	}
	return a.lifecycle().status()
}

// cmdAgent is the hidden "albert agent" command that up runs in the
// background. It serves until SIGTERM or SIGINT.
func cmdAgent(a *app, args []string) int {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	port := flags.Int("port", 0, "agent port")
	uiPort := flags.Int("ui-port", 0, "UI port (default: the agent port)")
	mode := flags.String("mode", "native", "docker or native")
	pos, code := a.parseFlags(flags, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 || *port <= 0 || *port > 65535 || *uiPort < 0 || *uiPort > 65535 {
		return a.usagef("usage: albert agent --port P [--ui-port U] [--mode docker|native]")
	}
	if *uiPort == 0 {
		*uiPort = *port
	}
	opts := agent.RunOptions{Port: *port, UIPort: *uiPort, Mode: *mode, Version: version}
	switch *mode {
	case "native":
		ui, err := fs.Sub(albert.Files, "web")
		if err != nil {
			return a.failf("albert agent: %v", err)
		}
		opts.UI = ui
	case "docker":
	default:
		return a.usagef("albert agent: --mode must be docker or native")
	}
	dir := state.Dir()
	opts.StateDir = dir
	opts.TokenFile = filepath.Join(dir, "token")

	ctx, stop := signal.NotifyContext(a.context(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := agent.Run(ctx, opts); err != nil {
		return a.failf("albert agent: %v", err)
	}
	return exitOK
}

// --- real outside world ---

// spawnDetached starts "<this binary> <args>" in its own session, with
// output appended to logPath, and lets it run on after we exit.
func spawnDetached(args []string, logPath string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(self, args...)
	cmd.Stdout, cmd.Stderr = logf, logf // stdin stays nil: /dev/null
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// killProcess sends SIGTERM, waits up to killWait for the process to exit,
// then sends SIGKILL. A process that is already gone is not an error.
func killProcess(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal process %d", pid)
	}
	gone := func(err error) bool { return errors.Is(err, syscall.ESRCH) }
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if gone(err) {
			return nil
		}
		return err
	}
	for deadline := time.Now().Add(killWait); time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		if gone(syscall.Kill(pid, 0)) {
			return nil
		}
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !gone(err) {
		return err
	}
	return nil
}

// processCommand returns pid's command line, or "" if there is no such
// process.
func processCommand(pid int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := runner.Exec{}.Run(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid))
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(string(res.Stdout))
}

var agentClient = &http.Client{Timeout: time.Second}

// agentRequest builds a request to the agent on 127.0.0.1:port that passes
// its Host and token checks.
func agentRequest(method string, port int, path, token string) (*http.Request, error) {
	host := "127.0.0.1:" + strconv.Itoa(port)
	req, err := http.NewRequest(method, "http://"+host+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

// httpHealth reports whether the agent on port answers /api/health with 200
// for token.
func httpHealth(port int, token string) bool {
	req, err := agentRequest("GET", port, "/api/health", token)
	if err != nil {
		return false
	}
	return healthOK(req)
}

// httpUIHealth reports whether the agent answers /api/health through the
// Docker window on port, addressed the way the browser will address it.
func httpUIHealth(port int, token string) bool {
	req, err := agentRequest("GET", port, "/api/health", token)
	if err != nil {
		return false
	}
	req.Host = "localhost:" + strconv.Itoa(port)
	return healthOK(req)
}

func healthOK(req *http.Request) bool {
	resp, err := agentClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode == http.StatusOK
}

// httpLoginCode asks the agent on port for a one-time browser sign-in code.
func httpLoginCode(port int, token string) (string, error) {
	req, err := agentRequest("POST", port, "/api/login-code", token)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Albert-Request", "1")
	resp, err := agentClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("login code: status %d", resp.StatusCode)
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body); err != nil {
		return "", err
	}
	if body.Code == "" {
		return "", errors.New("login code: empty")
	}
	return body.Code, nil
}

// httpActiveJobs counts the queued and running jobs of the agent on port.
func httpActiveJobs(port int, token string) (int, error) {
	req, err := agentRequest("GET", port, "/api/jobs", token)
	if err != nil {
		return 0, err
	}
	resp, err := agentClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("jobs: status %d", resp.StatusCode)
	}
	var body struct {
		Jobs []struct {
			Status string `json:"status"`
		} `json:"jobs"`
	}
	// Each job carries its full output, so the list can be large.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&body); err != nil {
		return 0, err
	}
	n := 0
	for _, j := range body.Jobs {
		if j.Status == jobs.StatusQueued || j.Status == jobs.StatusRunning {
			n++
		}
	}
	return n, nil
}
