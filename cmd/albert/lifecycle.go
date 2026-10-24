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
	killWait       = 3 * time.Second        // SIGTERM grace before SIGKILL
)

// dockerControl is what Albert needs from Docker.
type dockerControl interface {
	Ready(ctx context.Context) error
	HasImage(ctx context.Context, image string) bool
	EnsureImage(ctx context.Context, image string, files fs.FS) error
	Start(ctx context.Context, image string, uiPort, agentPort int) error
	Stop(ctx context.Context) error
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
	// mintCode asks the agent for a one-time browser sign-in code.
	mintCode func(port int, token string) (string, error)
	openURL  func(url string) error
	out      io.Writer
	in       io.Reader
	tty      bool
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

	agentPort, err := state.FreePort(defaultAgentPort)
	if err != nil {
		fmt.Fprintf(l.out, "Albert couldn't find a free port: %v\n", err)
		return exitFail
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
			return exitFail
		}
	}
	tok, err := state.WriteToken(l.dir)
	if err != nil {
		fmt.Fprintf(l.out, "Albert couldn't save its settings in %s: %v\n", l.dir, err)
		return exitFail
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
		return exitFail
	}
	// From here on any failure must stop the agent we just started.
	fail := func() int {
		_ = l.kill(pid)
		_ = state.Clear(l.dir)
		return exitFail
	}
	if !l.waitHealthy(ctx, agentPort, tok) {
		fmt.Fprintf(l.out, "Albert couldn't start. Details: %s\n", logPath)
		return fail()
	}

	if mode == "docker" {
		image := dock.Image(l.version)
		if !l.docker.HasImage(ctx, image) {
			fmt.Fprintln(l.out, "Preparing Albert's window (first time only)…")
		}
		err := l.docker.EnsureImage(ctx, image, albert.Files)
		if err == nil {
			err = l.docker.Start(ctx, image, uiPort, agentPort)
		}
		if err != nil {
			fmt.Fprintf(l.out, "Albert couldn't open its window in Docker: %v\n", err)
			fmt.Fprintln(l.out, "Try again, or open Albert without Docker: albert up --native")
			return fail()
		}
	}

	st := state.State{PID: pid, AgentPort: agentPort, UIPort: uiPort, Mode: mode, Started: time.Now()}
	if err := state.Save(l.dir, st); err != nil {
		fmt.Fprintf(l.out, "Albert couldn't save its settings in %s: %v\n", l.dir, err)
		if mode == "docker" {
			_ = l.docker.Stop(ctx)
		}
		return fail()
	}
	fmt.Fprintf(l.out, "Albert is running at %s\n", uiURL(uiPort))
	fmt.Fprintln(l.out, "Stop it any time with: albert down")
	if !noOpen {
		l.open(st, tok)
	}
	return exitOK
}

// waitEnter waits for a line on l.in (or end of input). It reports false if
// ctx ends first.
func (l *lifecycle) waitEnter(ctx context.Context) bool {
	done := make(chan struct{})
	go func() {
		_, _ = bufio.NewReader(l.in).ReadString('\n')
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// waitHealthy polls the agent's health until it answers, healthTimeout
// passes or ctx ends.
func (l *lifecycle) waitHealthy(ctx context.Context, port int, tok string) bool {
	deadline := time.Now().Add(healthTimeout)
	for {
		if l.health(port, tok) {
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
// code is never printed.
func (l *lifecycle) open(st state.State, tok string) {
	u := uiURL(st.UIPort)
	target := u
	if code, err := l.mintCode(st.AgentPort, tok); err == nil {
		target = u + "/login?code=" + url.QueryEscape(code)
	}
	if err := l.openURL(target); err != nil {
		fmt.Fprintf(l.out, "Open %s in your browser.\n", u)
	}
}

func (l *lifecycle) down() int {
	st, exists, err := state.Load(l.dir)
	if err != nil {
		exists = true // a damaged state file still means something may be running
	}
	if !exists {
		_ = l.docker.Stop(l.context()) // a container left behind by a lost state file
		fmt.Fprintln(l.out, "Albert isn't running.")
		return exitOK
	}
	_, healthy := isRunning(l)
	if err := l.cleanup(st, healthy); err != nil {
		fmt.Fprintf(l.out, "Albert couldn't stop (process %d): %v\n", st.PID, err)
		return exitFail
	}
	fmt.Fprintln(l.out, "Albert stopped.")
	return exitOK
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
		dir:      state.Dir(),
		version:  version,
		ctx:      a.ctx,
		docker:   a.docker,
		spawn:    spawnDetached,
		kill:     killProcess,
		procCmd:  processCommand,
		health:   httpHealth,
		mintCode: httpLoginCode,
		openURL:  func(u string) error { return exec.Command("open", u).Run() },
		out:      a.stdout,
		in:       a.stdin,
		tty:      a.isTTY,
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
	if len(args) > 0 {
		return a.usagef("albert down takes no arguments.")
	}
	return a.lifecycle().down()
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
