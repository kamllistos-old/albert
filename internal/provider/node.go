package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

var bunLine = regexp.MustCompile(`── (@?[^@\s]+)@(\S+)`)

// nodeProvider holds what npm, pnpm and bun share: they differ only in the
// binary, the verb that adds a global package, and how they list and watch.
type nodeProvider struct {
	r     runner.Runner
	reg   *registry.Client
	name  string
	addAs string // "install" for npm, "add" for pnpm and bun
	list  func(ctx context.Context) ([]Tool, error)
	watch func() []string
}

// NewNPM returns the npm global-package provider.
func NewNPM(r runner.Runner, reg *registry.Client) Provider {
	p := &nodeProvider{r: r, reg: reg, name: "npm", addAs: "install"}
	p.list = p.listNPM
	p.watch = func() []string { return cmdPaths(r, "npm", "root", "-g") }
	return p
}

// NewPnpm returns the pnpm global-package provider.
func NewPnpm(r runner.Runner, reg *registry.Client) Provider {
	p := &nodeProvider{r: r, reg: reg, name: "pnpm", addAs: "add"}
	p.list = p.listPnpm
	p.watch = func() []string { return cmdPaths(r, "pnpm", "root", "-g") }
	return p
}

// NewBun returns the bun global-package provider.
func NewBun(r runner.Runner, reg *registry.Client) Provider {
	p := &nodeProvider{r: r, reg: reg, name: "bun", addAs: "add"}
	p.list = p.listBun
	p.watch = func() []string {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		return []string{filepath.Join(home, ".bun", "install", "global", "node_modules")}
	}
	return p
}

func (p *nodeProvider) Name() string { return p.name }

func (p *nodeProvider) Available() bool {
	_, err := p.r.LookPath(p.name)
	return err == nil
}

func (p *nodeProvider) List(ctx context.Context) ([]Tool, error) { return p.list(ctx) }

type nodeDeps map[string]struct {
	Version string `json:"version"`
}

func (p *nodeProvider) toolsFrom(deps nodeDeps, dir string) []Tool {
	tools := make([]Tool, 0, len(deps))
	for name, d := range deps {
		t := Tool{Name: name, Source: p.name, Version: d.Version}
		if dir != "" {
			t.Path = filepath.Join(dir, name)
		}
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools
}

func (p *nodeProvider) listNPM(ctx context.Context) ([]Tool, error) {
	// npm ls exits non-zero for problems such as missing peer dependencies
	// but still prints the package list, so only fail when there is no list.
	res, err := p.r.Run(ctx, "npm", "ls", "-g", "--json", "--depth=0")
	if err != nil {
		return nil, fmt.Errorf("npm: %w", err)
	}
	var doc struct {
		Dependencies nodeDeps `json:"dependencies"`
	}
	if jerr := json.Unmarshal(res.Stdout, &doc); jerr != nil {
		if res.ExitCode != 0 {
			return nil, cmdErr("npm", res)
		}
		return nil, fmt.Errorf("npm: unreadable output: %w", jerr)
	}
	var dir string
	if roots := cmdPaths(p.r, "npm", "root", "-g"); len(roots) > 0 {
		dir = roots[0]
	}
	return p.toolsFrom(doc.Dependencies, dir), nil
}

func (p *nodeProvider) listPnpm(ctx context.Context) ([]Tool, error) {
	out, err := runOutput(ctx, p.r, "pnpm", "ls", "-g", "--json")
	if err != nil {
		return nil, err
	}
	var docs []struct {
		Dependencies nodeDeps `json:"dependencies"`
	}
	if err := json.Unmarshal(out, &docs); err != nil {
		return nil, fmt.Errorf("pnpm: unreadable output: %w", err)
	}
	if len(docs) == 0 {
		return nil, nil
	}
	return p.toolsFrom(docs[0].Dependencies, ""), nil
}

func (p *nodeProvider) listBun(ctx context.Context) ([]Tool, error) {
	out, err := runOutput(ctx, p.r, "bun", "pm", "ls", "-g")
	if err != nil {
		return nil, err
	}
	var tools []Tool
	for _, l := range strings.Split(string(out), "\n") {
		if m := bunLine.FindStringSubmatch(l); m != nil {
			tools = append(tools, Tool{Name: m[1], Source: p.name, Version: m[2]})
		}
	}
	return tools, nil
}

func (p *nodeProvider) Outdated(ctx context.Context) ([]Update, error) {
	tools, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	if p.reg == nil {
		return nil, errors.New(p.name + ": no registry client")
	}
	return LookupAll(ctx, p.name, tools, lookupLimit, toolName, func(ctx context.Context, t Tool) (Update, bool, error) {
		pkg, err := p.reg.NPM(ctx, t.Name)
		return pkgUpdate(t, pkg, err)
	})
}

func (p *nodeProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if err := checkNames(p.name, names); err != nil {
		return err
	}
	var lookupErr error
	if len(names) == 0 {
		ups, err := p.Outdated(ctx)
		lookupErr, err = lookupFailure(err)
		if err != nil {
			return err
		}
		for _, u := range ups {
			names = append(names, u.Name)
		}
		if len(names) == 0 {
			return lookupErr
		}
	}
	args := []string{p.addAs, "-g"}
	for _, n := range names {
		args = append(args, n+"@latest")
	}
	return errors.Join(runStream(ctx, p.r, out, p.name, args...), lookupErr)
}

func (p *nodeProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if err := checkNames(p.name, []string{name}); err != nil {
		return err
	}
	return runStream(ctx, p.r, out, p.name, p.addAs, "-g", name)
}

func (p *nodeProvider) Remove(ctx context.Context, name string, out io.Writer) error {
	if err := checkNames(p.name, []string{name}); err != nil {
		return err
	}
	return runStream(ctx, p.r, out, p.name, "remove", "-g", name)
}

func (p *nodeProvider) Search(ctx context.Context, q string) ([]Result, error) {
	if p.reg == nil {
		return nil, errors.New(p.name + ": no registry client")
	}
	pkgs, err := p.reg.NPMSearch(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	rs := make([]Result, 0, len(pkgs))
	for _, k := range pkgs {
		rs = append(rs, Result{Name: k.Name, Source: p.name, Version: k.Version, Description: k.Description})
	}
	return rs, nil
}

func (p *nodeProvider) WatchPaths() []string { return p.watch() }
