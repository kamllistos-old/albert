package provider

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

var cargoLine = regexp.MustCompile(`^(\S+) v([^\s:]+)(?: \(.+\))?:$`)

type cargoProvider struct {
	r   runner.Runner
	reg *registry.Client
}

// NewCargo returns the cargo-installed crates provider.
func NewCargo(r runner.Runner, reg *registry.Client) Provider {
	return &cargoProvider{r: r, reg: reg}
}

func (p *cargoProvider) Name() string { return "cargo" }

func (p *cargoProvider) Available() bool {
	_, err := p.r.LookPath("cargo")
	return err == nil
}

func (p *cargoProvider) List(ctx context.Context) ([]Tool, error) {
	out, err := runOutput(ctx, p.r, "cargo", "install", "--list")
	if err != nil {
		return nil, err
	}
	var tools []Tool
	for _, l := range strings.Split(string(out), "\n") {
		// Indented lines are the binaries a crate provides.
		if m := cargoLine.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil {
			tools = append(tools, Tool{Name: m[1], Source: p.Name(), Version: m[2]})
		}
	}
	return tools, nil
}

func (p *cargoProvider) Outdated(ctx context.Context) ([]Update, error) {
	tools, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	if p.reg == nil {
		return nil, errors.New("cargo: no registry client")
	}
	return LookupAll(ctx, tools, lookupLimit, func(ctx context.Context, t Tool) (Update, bool) {
		pkg, err := p.reg.Crate(ctx, t.Name)
		if err != nil || pkg.Version == "" || pkg.Version == t.Version {
			return Update{}, false
		}
		return Update{Name: t.Name, Latest: pkg.Version, Description: pkg.Description, Homepage: pkg.Homepage}, true
	}), nil
}

func cargoInstallArgs(name string) []string { return []string{"install", name} }

func (p *cargoProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if err := checkNames("cargo", names); err != nil {
		return err
	}
	if len(names) == 0 {
		ups, err := p.Outdated(ctx)
		if err != nil {
			return err
		}
		for _, u := range ups {
			names = append(names, u.Name)
		}
		if err := checkNames("cargo", names); err != nil {
			return err
		}
	}
	return streamEach(ctx, p.r, out, "cargo", names, cargoInstallArgs)
}

func (p *cargoProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if err := checkNames("cargo", []string{name}); err != nil {
		return err
	}
	return runStream(ctx, p.r, out, "cargo", cargoInstallArgs(name)...)
}

func (p *cargoProvider) Search(context.Context, string) ([]Result, error) {
	return nil, ErrUnsupported
}

func (p *cargoProvider) WatchPaths() []string {
	if home := os.Getenv("CARGO_HOME"); home != "" {
		return []string{filepath.Join(home, "bin")}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".cargo", "bin")}
}
