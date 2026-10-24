// Package runner abstracts running external commands so providers can be
// tested without touching the real machine.
package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// waitDelay is how long a cancelled command gets to exit after SIGTERM before
// it is killed, and how long we then wait for its pipes to close, so a
// timeout is not held up by grandchildren that inherited stdout/stderr.
const waitDelay = 2 * time.Second

// Result is the outcome of a finished command.
type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
}

// Runner runs external commands.
type Runner interface {
	// Run runs the command and captures its output. err is non-nil only if the
	// command could not start or ctx expired; a non-zero exit is NOT an error.
	Run(ctx context.Context, name string, args ...string) (Result, error)
	// Stream writes combined stdout+stderr to out as it is produced and returns
	// the exit code.
	Stream(ctx context.Context, out io.Writer, name string, args ...string) (int, error)
	// LookPath reports where name is installed, or an error if it is not.
	LookPath(name string) (string, error)
}

// Exec runs real commands. Stdin nil means the command gets no stdin.
type Exec struct{ Stdin io.Reader }

func (e Exec) command(ctx context.Context, name string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	// On cancel (timeout, "albert down"), ask politely first: a package
	// manager stopped with SIGTERM can remove its lock files and roll back.
	// One that ignores it is killed after WaitDelay.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = waitDelay
	cmd.Stdin = e.Stdin
	return cmd
}

// Run implements Runner.
func (e Exec) Run(ctx context.Context, name string, args ...string) (Result, error) {
	var stdout, stderr bytes.Buffer
	cmd := e.command(ctx, name, args)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	code, err := finish(ctx, cmd.Run())
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: code}, err
}

// Stream implements Runner. stdout and stderr share one writer, and exec
// serialises writes to it when both are the same non-*os.File writer.
func (e Exec) Stream(ctx context.Context, out io.Writer, name string, args ...string) (int, error) {
	cmd := e.command(ctx, name, args)
	cmd.Stdout, cmd.Stderr = out, out
	return finish(ctx, cmd.Run())
}

// LookPath implements Runner.
func (Exec) LookPath(name string) (string, error) { return exec.LookPath(name) }

// finish turns the error from cmd.Run into (exit code, error): a non-zero exit
// is reported through the code only, while a context expiry or a failure to
// start is returned as an error.
func finish(ctx context.Context, err error) (int, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return -1, err
}
