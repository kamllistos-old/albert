package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kamllistos/albert/internal/runner"
)

const prefixTimeout = 5 * time.Second

// brewInfo is the part of `brew info --json=v2 --installed` that Albert reads.
// Formula installed is an array of objects; cask installed is a version string.
type brewInfo struct {
	Formulae []struct {
		Name      string `json:"name"`
		Desc      string `json:"desc"`
		Homepage  string `json:"homepage"`
		Installed []struct {
			Version   string `json:"version"`
			OnRequest bool   `json:"installed_on_request"`
		} `json:"installed"`
	} `json:"formulae"`
	Casks []struct {
		Token     string `json:"token"`
		Desc      string `json:"desc"`
		Homepage  string `json:"homepage"`
		Installed string `json:"installed"`
	} `json:"casks"`
}

// brewOutdated is `brew outdated --json=v2`.
type brewOutdated struct {
	Formulae []brewOutdatedEntry `json:"formulae"`
	Casks    []brewOutdatedEntry `json:"casks"`
}

type brewOutdatedEntry struct {
	Name           string `json:"name"`
	CurrentVersion string `json:"current_version"`
}

// brewProvider implements both Homebrew sources; cask selects which one.
type brewProvider struct {
	r    runner.Runner
	cask bool

	mu     sync.Mutex
	prefix string // cached only once `brew --prefix` has succeeded
}

// NewBrew returns the Homebrew formula provider.
func NewBrew(r runner.Runner) Provider { return &brewProvider{r: r} }

// NewCask returns the Homebrew cask provider.
func NewCask(r runner.Runner) Provider { return &brewProvider{r: r, cask: true} }

func (p *brewProvider) Name() string {
	if p.cask {
		return "cask"
	}
	return "brew"
}

// kind is the flag that restricts a brew command to this provider's packages.
func (p *brewProvider) kind() string {
	if p.cask {
		return "--cask"
	}
	return "--formula"
}

// installDir is the directory under the prefix where installs live.
func (p *brewProvider) installDir() string {
	if p.cask {
		return "Caskroom"
	}
	return "Cellar"
}

// brewPrefix returns the Homebrew prefix. A successful lookup is cached for
// good; a failed one is not, so a transient failure (timeout, brew briefly
// unavailable) is retried on the next call. It is empty while brew cannot be
// run.
func (p *brewProvider) brewPrefix() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prefix != "" {
		return p.prefix
	}
	ctx, cancel := context.WithTimeout(context.Background(), prefixTimeout)
	defer cancel()
	res, err := p.r.Run(ctx, "brew", "--prefix")
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	p.prefix = strings.TrimSpace(string(res.Stdout))
	return p.prefix
}

func (p *brewProvider) toolPath(name string) string {
	if prefix := p.brewPrefix(); prefix != "" {
		return filepath.Join(prefix, p.installDir(), name)
	}
	return ""
}

func (p *brewProvider) Available() bool {
	_, err := p.r.LookPath("brew")
	return err == nil
}

// run runs brew and returns stdout, or an error built from stderr on failure.
func (p *brewProvider) run(ctx context.Context, args ...string) ([]byte, error) {
	res, err := p.r.Run(ctx, "brew", args...)
	if err != nil {
		return nil, fmt.Errorf("brew: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, cmdErr("brew", res)
	}
	return res.Stdout, nil
}

func (p *brewProvider) List(ctx context.Context) ([]Tool, error) {
	out, err := p.run(ctx, "info", "--json=v2", "--installed")
	if err != nil {
		return nil, err
	}
	var info brewInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("brew: unreadable output: %w", err)
	}
	var tools []Tool
	if p.cask {
		for _, c := range info.Casks {
			tools = append(tools, Tool{
				Name:        c.Token,
				Source:      p.Name(),
				Version:     c.Installed,
				Description: c.Desc,
				Homepage:    c.Homepage,
				Path:        p.toolPath(c.Token),
			})
		}
		return tools, nil
	}
	for _, f := range info.Formulae {
		if len(f.Installed) == 0 {
			continue
		}
		tools = append(tools, Tool{
			Name:        f.Name,
			Source:      p.Name(),
			Version:     f.Installed[0].Version,
			Description: f.Desc,
			Homepage:    f.Homepage,
			Path:        p.toolPath(f.Name),
			Dependency:  !f.Installed[0].OnRequest,
		})
	}
	return tools, nil
}

func (p *brewProvider) Outdated(ctx context.Context) ([]Update, error) {
	args := []string{"outdated", p.kind(), "--json=v2"}
	if p.cask {
		// Casks that update themselves are hidden from outdated unless greedy.
		args = []string{"outdated", "--cask", "--greedy-auto-updates", "--json=v2"}
	}
	out, err := p.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var o brewOutdated
	if err := json.Unmarshal(out, &o); err != nil {
		return nil, fmt.Errorf("brew: unreadable output: %w", err)
	}
	entries := o.Formulae
	if p.cask {
		entries = o.Casks
	}
	var ups []Update
	for _, e := range entries {
		ups = append(ups, Update{Name: e.Name, Latest: e.CurrentVersion})
	}
	return ups, nil
}

// stream runs a brew command that prints progress to out.
func (p *brewProvider) stream(ctx context.Context, out io.Writer, args ...string) error {
	code, err := p.r.Stream(ctx, out, "brew", args...)
	if err != nil {
		return fmt.Errorf("brew: %w", err)
	}
	if code != 0 {
		return cmdErr("brew", runner.Result{ExitCode: code})
	}
	return nil
}

func (p *brewProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	for _, n := range names {
		if !ValidName(n) {
			return fmt.Errorf("brew: invalid name %q", n)
		}
	}
	return p.stream(ctx, out, append([]string{"upgrade", p.kind()}, names...)...)
}

func (p *brewProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if !ValidName(name) {
		return fmt.Errorf("brew: invalid name %q", name)
	}
	return p.stream(ctx, out, "install", p.kind(), name)
}

func (p *brewProvider) Search(ctx context.Context, q string) ([]Result, error) {
	if strings.HasPrefix(q, "-") {
		return nil, fmt.Errorf("brew: invalid search %q", q)
	}
	res, err := p.r.Run(ctx, "brew", "search", p.kind(), q)
	if err != nil {
		return nil, fmt.Errorf("brew: %w", err)
	}
	if res.ExitCode != 0 {
		// brew exits 1 when nothing matches; that is an empty result.
		if len(res.Stdout) == 0 && strings.Contains(string(res.Stderr), "No formulae or casks found") {
			return nil, nil
		}
		return nil, cmdErr("brew", res)
	}
	var hits []Result
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "==>") {
			continue
		}
		hits = append(hits, Result{Name: l, Source: p.Name()})
	}
	return hits, nil
}

func (p *brewProvider) WatchPaths() []string {
	prefix := p.brewPrefix()
	if prefix == "" {
		return nil
	}
	return []string{filepath.Join(prefix, p.installDir())}
}
