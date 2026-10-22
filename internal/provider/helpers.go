package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kamllistos/albert/internal/runner"
)

const (
	pathTimeout = 5 * time.Second
	// lookupLimit is how many registry lookups run at once per provider.
	lookupLimit = 8
)

// runOutput runs name with args and returns stdout, or an error built from
// stderr.
func runOutput(ctx context.Context, r runner.Runner, name string, args ...string) ([]byte, error) {
	res, err := r.Run(ctx, name, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if res.ExitCode != 0 {
		return nil, cmdErr(name, res)
	}
	return res.Stdout, nil
}

// runStream runs name with args, streaming progress to out.
func runStream(ctx context.Context, r runner.Runner, out io.Writer, name string, args ...string) error {
	code, err := r.Stream(ctx, out, name, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if code != 0 {
		return cmdErr(name, runner.Result{ExitCode: code})
	}
	return nil
}

// cmdPaths runs a command that prints a directory and returns it as a one-item
// slice, trimmed, or nil on any failure.
func cmdPaths(r runner.Runner, name string, args ...string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), pathTimeout)
	defer cancel()
	out, err := runOutput(ctx, r, name, args...)
	if err != nil {
		return nil
	}
	if p := strings.TrimSpace(string(out)); p != "" {
		return []string{p}
	}
	return nil
}

func checkNames(prefix string, names []string) error {
	for _, n := range names {
		if !ValidName(n) {
			return fmt.Errorf("%s: invalid name %q", prefix, n)
		}
	}
	return nil
}

// streamEach runs `name args(target)` once per target, streaming to out. A
// failure does not stop the remaining targets; all failures are returned
// joined. It stops early once ctx is done.
func streamEach(ctx context.Context, r runner.Runner, out io.Writer, name string, targets []string, args func(string) []string) error {
	var errs []error
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if err := runStream(ctx, r, out, name, args(t)...); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
