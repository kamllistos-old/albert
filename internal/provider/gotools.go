package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

type goProvider struct {
	r   runner.Runner
	reg *registry.Client
}

// goBin is an installed Go binary and the module it was built from.
type goBin struct {
	tool   Tool
	module string
}

// NewGo returns the provider for binaries installed with `go install`.
func NewGo(r runner.Runner, reg *registry.Client) Provider { return &goProvider{r: r, reg: reg} }

func (p *goProvider) Name() string { return "go" }

func (p *goProvider) Available() bool {
	_, err := p.r.LookPath("go")
	return err == nil
}

// binDir is GOBIN, else the bin directory of the first GOPATH entry.
func (p *goProvider) binDir(ctx context.Context) (string, error) {
	out, err := runOutput(ctx, p.r, "go", "env", "GOBIN")
	if err != nil {
		return "", err
	}
	if dir := strings.TrimSpace(string(out)); dir != "" {
		return dir, nil
	}
	out, err = runOutput(ctx, p.r, "go", "env", "GOPATH")
	if err != nil {
		return "", err
	}
	gopath := filepath.SplitList(strings.TrimSpace(string(out)))
	if len(gopath) == 0 || gopath[0] == "" {
		return "", errors.New("go: GOPATH is empty")
	}
	return filepath.Join(gopath[0], "bin"), nil
}

// scan inspects every file in the bin dir with `go version -m`. Files that are
// not Go binaries are skipped; a missing bin dir is an empty result.
func (p *goProvider) scan(ctx context.Context) ([]goBin, error) {
	dir, err := p.binDir(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("go: %w", err)
	}
	var bins []goBin
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		file := filepath.Join(dir, e.Name())
		res, err := p.r.Run(ctx, "go", "version", "-m", file)
		if err != nil {
			return nil, fmt.Errorf("go: %w", err) // includes ctx expiry: no truncated lists
		}
		if res.ExitCode != 0 {
			continue // not a Go binary
		}
		if b, ok := parseGoVersionM(string(res.Stdout)); ok {
			b.tool.Name, b.tool.Source, b.tool.Path = e.Name(), p.Name(), file
			bins = append(bins, b)
		}
	}
	return bins, nil
}

// parseGoVersionM reads the `path` and first `mod` lines of `go version -m`.
func parseGoVersionM(s string) (goBin, bool) {
	var b goBin
	var hasPath, hasMod bool
	for _, l := range strings.Split(s, "\n") {
		f := strings.Split(strings.TrimRight(l, "\r"), "\t")
		if len(f) < 3 || f[0] != "" {
			continue
		}
		switch {
		case f[1] == "path" && !hasPath:
			b.tool.ID, hasPath = f[2], true
		case f[1] == "mod" && !hasMod:
			b.module, hasMod = f[2], true
			if len(f) > 3 {
				b.tool.Version = f[3]
			}
		}
	}
	return b, hasPath
}

func (p *goProvider) List(ctx context.Context) ([]Tool, error) {
	bins, err := p.scan(ctx)
	if err != nil {
		return nil, err
	}
	tools := make([]Tool, len(bins))
	for i, b := range bins {
		tools[i] = b.tool
	}
	return tools, nil
}

func (p *goProvider) Outdated(ctx context.Context) ([]Update, error) {
	bins, err := p.scan(ctx)
	if err != nil {
		return nil, err
	}
	return p.outdated(ctx, bins)
}

func (p *goProvider) outdated(ctx context.Context, bins []goBin) ([]Update, error) {
	if p.reg == nil {
		return nil, errors.New("go: no registry client")
	}
	name := func(b goBin) string { return b.tool.Name }
	return LookupAll(ctx, "go", bins, lookupLimit, name, func(ctx context.Context, b goBin) (Update, bool, error) {
		if b.module == "" || b.tool.Version == "" || b.tool.Version == "(devel)" {
			return Update{}, false, nil
		}
		latest, err := p.reg.GoLatest(ctx, b.module)
		if errors.Is(err, registry.ErrNotFound) {
			return Update{}, false, nil
		}
		if err != nil {
			return Update{}, false, err
		}
		if latest == "" || latest == b.tool.Version {
			return Update{}, false, nil
		}
		return Update{Name: b.tool.Name, Latest: latest}, true, nil
	})
}

func goInstallArgs(target string) []string { return []string{"install", target + "@latest"} }

func (p *goProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if err := checkNames("go", names); err != nil {
		return err
	}
	bins, err := p.scan(ctx)
	if err != nil {
		return err
	}
	var lookupErr error
	if len(names) == 0 {
		ups, err := p.outdated(ctx, bins)
		lookupErr, err = lookupFailure(err)
		if err != nil {
			return err
		}
		for _, u := range ups {
			names = append(names, u.Name)
		}
	}
	byName := make(map[string]string, 2*len(bins))
	for _, b := range bins {
		byName[b.tool.Name] = b.tool.ID
		byName[b.tool.ID] = b.tool.ID
	}
	targets := make([]string, 0, len(names))
	for _, n := range names {
		id, ok := byName[n]
		if !ok {
			return fmt.Errorf("go: %q is not installed", n)
		}
		targets = append(targets, id)
	}
	// Package paths come from binary metadata, so check them too.
	if err := checkNames("go", targets); err != nil {
		return err
	}
	return errors.Join(streamEach(ctx, p.r, out, "go", targets, goInstallArgs), lookupErr)
}

func (p *goProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if err := checkNames("go", []string{name}); err != nil {
		return err
	}
	return runStream(ctx, p.r, out, "go", goInstallArgs(name)...)
}

func (p *goProvider) Remove(context.Context, string, io.Writer) error { return ErrUnsupported }

func (p *goProvider) Search(context.Context, string) ([]Result, error) {
	return nil, ErrUnsupported
}

func (p *goProvider) WatchPaths() []string {
	ctx, cancel := context.WithTimeout(context.Background(), pathTimeout)
	defer cancel()
	dir, err := p.binDir(ctx)
	if err != nil {
		return nil
	}
	return []string{dir}
}
