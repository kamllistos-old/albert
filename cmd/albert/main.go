// Command albert lists and updates the tools installed by your package
// managers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/kamllistos/albert/internal/dock"
	"github.com/kamllistos/albert/internal/provider"
	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

var version = "dev"

// Exit codes.
const (
	exitOK    = 0
	exitFail  = 1 // the operation failed
	exitUsage = 2 // the command was used wrongly
)

// app is everything a command needs from the outside world; tests build one
// with fake providers and in-memory IO.
type app struct {
	providers      []provider.Provider
	stdout, stderr io.Writer
	stdin          io.Reader
	isTTY          bool
	docker         dockerControl
	ctx            context.Context // nil means context.Background()
}

func (a *app) context() context.Context {
	if a.ctx == nil {
		return context.Background()
	}
	return a.ctx
}

// command runs one sub-command and returns the process exit code.
type command func(a *app, args []string) int

// commands is the one place sub-commands are registered.
var commands = map[string]command{
	"up":       cmdUp,
	"down":     cmdDown,
	"status":   cmdStatus,
	"agent":    cmdAgent, // hidden: run by "albert up" in the background
	"list":     cmdList,
	"outdated": cmdOutdated,
	"update":   cmdUpdate,
	"install":  cmdInstall,
	"search":   cmdSearch,
	"doctor":   cmdDoctor,
	"version":  cmdVersion,
}

const helpText = `Albert shows the tools installed on this Mac and keeps them up to date.

Usage: albert <command> [options]

Commands:
  up         Open Albert in your browser     [--native] [--no-open]
  down       Stop Albert
  status     Show whether Albert is running
  list       Show installed tools            [--source S] [--outdated] [--deps] [--json]
  outdated   Show tools with a newer version [--source S]
  update     Update one tool or all of them  <tool> | --all  [--source S] [--include-pip]
  install    Install a tool                  <tool> [--source S]
  search     Find tools to install           <query>
  doctor     Check which package managers Albert can use
  version    Show Albert's version
  help       Show this help
`

func run(a *app, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(a.stdout, helpText)
		return exitOK
	}
	name := args[0]
	switch name {
	case "help", "-h", "--help":
		fmt.Fprint(a.stdout, helpText)
		return exitOK
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(a.stderr, "Unknown command %q. Run \"albert help\" to see what Albert can do.\n", name)
		return exitUsage
	}
	return cmd(a, args[1:])
}

func cmdVersion(a *app, _ []string) int {
	fmt.Fprintf(a.stdout, "albert %s\n", version)
	return exitOK
}

// parseFlags parses args into fs, allowing flags before and after positional
// arguments. Everything after a "--" is positional. The second result is -1
// when parsing succeeded; otherwise it is the exit code to return.
func (a *app) parseFlags(fs *flag.FlagSet, args []string) ([]string, int) {
	fs.SetOutput(a.stderr)
	rest, tail := args, []string(nil)
	for i, s := range args {
		if s == "--" {
			rest, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, exitOK
			}
			return nil, exitUsage
		}
		rest = fs.Args()
		if len(rest) > 0 {
			pos, rest = append(pos, rest[0]), rest[1:]
		}
	}
	return append(pos, tail...), -1
}

// usagef prints a usage problem and returns the usage exit code.
func (a *app) usagef(format string, args ...any) int {
	fmt.Fprintf(a.stderr, format+"\n", args...)
	return exitUsage
}

// failf prints a failure and returns the failure exit code.
func (a *app) failf(format string, args ...any) int {
	fmt.Fprintf(a.stderr, format+"\n", args...)
	return exitFail
}

// find returns the provider with the given name.
func (a *app) find(name string) (provider.Provider, bool) {
	for _, p := range a.providers {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}

// checkSource returns a usage error message if source is set but unknown.
func (a *app) checkSource(source string) string {
	if source == "" {
		return ""
	}
	if _, ok := a.find(source); ok {
		return ""
	}
	names := make([]string, len(a.providers))
	for i, p := range a.providers {
		names[i] = p.Name()
	}
	return fmt.Sprintf("Unknown source %q. Sources are: %s.", source, strings.Join(names, ", "))
}

// isTerminal reports whether f is an interactive terminal.
func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// cliSignals cancel the command's context instead of killing the process,
// so a command can clean up: Ctrl-C, closing the Terminal window (SIGHUP)
// and a plain kill (SIGTERM).
var cliSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), cliSignals...)
	a := &app{
		providers: provider.Defaults(runner.Exec{Stdin: os.Stdin}, registry.New()),
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		stdin:     os.Stdin,
		isTTY:     isTerminal(os.Stdin),
		docker:    dock.Docker{R: runner.Exec{}},
		ctx:       ctx,
	}
	code := run(a, os.Args[1:])
	stop()
	os.Exit(code)
}
