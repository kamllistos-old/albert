package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/kamllistos/albert/internal/registry"
	"github.com/kamllistos/albert/internal/runner"
)

const pipManagedMsg = "pip refused: this Python is managed by Homebrew. Use uv or pipx for tools."

var uvLine = regexp.MustCompile(`^(\S+) v(\S+) \((.+)\)$`)

// pypiOutdated looks every tool up on PyPI and reports those whose latest
// release differs from the installed version. Failed lookups come back as a
// *LookupError alongside the updates that were found.
func pypiOutdated(ctx context.Context, reg *registry.Client, source string, tools []Tool) ([]Update, error) {
	if reg == nil {
		return nil, errors.New(source + ": no registry client")
	}
	return LookupAll(ctx, source, tools, lookupLimit, toolName, func(ctx context.Context, t Tool) (Update, bool, error) {
		pkg, err := reg.PyPI(ctx, t.Name)
		return pkgUpdate(t, pkg, err)
	})
}

// pypiSearch returns the package named exactly q, if PyPI has it.
func pypiSearch(ctx context.Context, reg *registry.Client, source, q string) ([]Result, error) {
	if reg == nil {
		return nil, errors.New(source + ": no registry client")
	}
	if !ValidName(q) {
		return nil, nil // cannot be a package name
	}
	pkg, err := reg.PyPI(ctx, q)
	if err != nil {
		if errors.Is(err, registry.ErrNotFound) {
			return nil, nil // no such package
		}
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return []Result{{Name: pkg.Name, Source: source, Version: pkg.Version, Description: pkg.Description}}, nil
}

// ---- uv ----

type uvProvider struct {
	r   runner.Runner
	reg *registry.Client
}

// NewUV returns the uv tool provider.
func NewUV(r runner.Runner, reg *registry.Client) Provider { return &uvProvider{r: r, reg: reg} }

func (p *uvProvider) Name() string { return "uv" }

func (p *uvProvider) Available() bool {
	_, err := p.r.LookPath("uv")
	return err == nil
}

func (p *uvProvider) List(ctx context.Context) ([]Tool, error) {
	out, err := runOutput(ctx, p.r, "uv", "tool", "list", "--show-paths")
	if err != nil {
		return nil, err
	}
	var tools []Tool
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "- ") { // a binary the tool provides
			continue
		}
		m := uvLine.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		tools = append(tools, Tool{Name: m[1], Source: p.Name(), Version: m[2], Path: m[3]})
	}
	return tools, nil
}

func (p *uvProvider) Outdated(ctx context.Context) ([]Update, error) {
	tools, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	return pypiOutdated(ctx, p.reg, p.Name(), tools)
}

func (p *uvProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if err := checkNames("uv", names); err != nil {
		return err
	}
	if len(names) == 0 {
		return runStream(ctx, p.r, out, "uv", "tool", "upgrade", "--all")
	}
	return runStream(ctx, p.r, out, "uv", append([]string{"tool", "upgrade"}, names...)...)
}

func (p *uvProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if err := checkNames("uv", []string{name}); err != nil {
		return err
	}
	return runStream(ctx, p.r, out, "uv", "tool", "install", name)
}

func (p *uvProvider) Search(ctx context.Context, q string) ([]Result, error) {
	return pypiSearch(ctx, p.reg, p.Name(), q)
}

func (p *uvProvider) WatchPaths() []string { return cmdPaths(p.r, "uv", "tool", "dir") }

// ---- pipx ----

type pipxProvider struct {
	r   runner.Runner
	reg *registry.Client
}

// NewPipx returns the pipx provider.
func NewPipx(r runner.Runner, reg *registry.Client) Provider { return &pipxProvider{r: r, reg: reg} }

func (p *pipxProvider) Name() string { return "pipx" }

func (p *pipxProvider) Available() bool {
	_, err := p.r.LookPath("pipx")
	return err == nil
}

func (p *pipxProvider) List(ctx context.Context) ([]Tool, error) {
	out, err := runOutput(ctx, p.r, "pipx", "list", "--json")
	if err != nil {
		return nil, err
	}
	var doc struct {
		Venvs map[string]struct {
			Metadata struct {
				MainPackage struct {
					Package        string `json:"package"`
					PackageVersion string `json:"package_version"`
				} `json:"main_package"`
			} `json:"metadata"`
		} `json:"venvs"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("pipx: unreadable output: %w", err)
	}
	var tools []Tool
	for key, v := range doc.Venvs {
		mp := v.Metadata.MainPackage
		name := mp.Package
		if name == "" {
			name = key
		}
		tools = append(tools, Tool{Name: name, Source: p.Name(), Version: mp.PackageVersion})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

func (p *pipxProvider) Outdated(ctx context.Context) ([]Update, error) {
	tools, err := p.List(ctx)
	if err != nil {
		return nil, err
	}
	return pypiOutdated(ctx, p.reg, p.Name(), tools)
}

func (p *pipxProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if err := checkNames("pipx", names); err != nil {
		return err
	}
	if len(names) == 0 {
		return runStream(ctx, p.r, out, "pipx", "upgrade-all")
	}
	for _, n := range names {
		if err := runStream(ctx, p.r, out, "pipx", "upgrade", n); err != nil {
			return err
		}
	}
	return nil
}

func (p *pipxProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if err := checkNames("pipx", []string{name}); err != nil {
		return err
	}
	return runStream(ctx, p.r, out, "pipx", "install", name)
}

func (p *pipxProvider) Search(ctx context.Context, q string) ([]Result, error) {
	return pypiSearch(ctx, p.reg, p.Name(), q)
}

func (p *pipxProvider) WatchPaths() []string {
	return cmdPaths(p.r, "pipx", "environment", "--value", "PIPX_LOCAL_VENVS")
}

// ---- pip ----

type pipProvider struct{ r runner.Runner }

// NewPip returns the pip provider. Only packages installed with --user are
// managed; everything else is read-only.
func NewPip(r runner.Runner) Provider { return &pipProvider{r: r} }

func (p *pipProvider) Name() string { return "pip" }

func (p *pipProvider) Available() bool {
	_, err := p.r.LookPath("pip3")
	return err == nil
}

type pipEntry struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	LatestVersion string `json:"latest_version"`
}

func (p *pipProvider) listJSON(ctx context.Context, args ...string) ([]pipEntry, error) {
	out, err := runOutput(ctx, p.r, "pip3", args...)
	if err != nil {
		return nil, err
	}
	var es []pipEntry
	if err := json.Unmarshal(out, &es); err != nil {
		return nil, fmt.Errorf("pip3: unreadable output: %w", err)
	}
	return es, nil
}

// pipKey normalises a package name the way pip does (PEP 503).
var pipSep = regexp.MustCompile(`[-_.]+`)

func pipKey(name string) string { return strings.ToLower(pipSep.ReplaceAllString(name, "-")) }

func (p *pipProvider) List(ctx context.Context) ([]Tool, error) {
	all, err := p.listJSON(ctx, "list", "--format=json")
	if err != nil {
		return nil, err
	}
	user, err := p.listJSON(ctx, "list", "--user", "--format=json")
	if err != nil {
		return nil, err
	}
	isUser := make(map[string]bool, len(user))
	for _, u := range user {
		isUser[pipKey(u.Name)] = true
	}
	tools := make([]Tool, 0, len(all))
	for _, e := range all {
		tools = append(tools, Tool{Name: e.Name, Source: p.Name(), Version: e.Version, ReadOnly: !isUser[pipKey(e.Name)]})
	}
	return tools, nil
}

func (p *pipProvider) Outdated(ctx context.Context) ([]Update, error) {
	es, err := p.listJSON(ctx, "list", "--outdated", "--format=json")
	if err != nil {
		return nil, err
	}
	ups := make([]Update, 0, len(es))
	for _, e := range es {
		ups = append(ups, Update{Name: e.Name, Latest: e.LatestVersion})
	}
	return ups, nil
}

// teeBuffer remembers what was streamed so the caller can inspect it.
type teeBuffer struct {
	out io.Writer
	buf bytes.Buffer
}

func (t *teeBuffer) Write(b []byte) (int, error) {
	t.buf.Write(b)
	return t.out.Write(b)
}

func (p *pipProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if err := checkNames("pip", names); err != nil {
		return err
	}
	tools, err := p.List(ctx)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		ups, err := p.Outdated(ctx)
		if err != nil {
			return err
		}
		managed := make(map[string]bool, len(tools))
		for _, t := range tools {
			if !t.ReadOnly {
				managed[pipKey(t.Name)] = true
			}
		}
		for _, u := range ups {
			if managed[pipKey(u.Name)] {
				names = append(names, u.Name)
			}
		}
		if len(names) == 0 {
			return nil
		}
	} else {
		known := make(map[string]Tool, len(tools))
		for _, t := range tools {
			known[pipKey(t.Name)] = t
		}
		for _, n := range names {
			t, ok := known[pipKey(n)]
			if !ok {
				return fmt.Errorf("pip: %s is not installed", n)
			}
			if t.ReadOnly {
				return fmt.Errorf("pip: %s: %w", n, ErrReadOnly)
			}
		}
	}
	tee := &teeBuffer{out: out}
	args := append([]string{"install", "--user", "--upgrade"}, names...)
	code, err := p.r.Stream(ctx, tee, "pip3", args...)
	if bytes.Contains(tee.buf.Bytes(), []byte("externally-managed-environment")) {
		return errors.New(pipManagedMsg)
	}
	if err != nil {
		return fmt.Errorf("pip3: %w", err)
	}
	if code != 0 {
		return cmdErr("pip3", runner.Result{ExitCode: code})
	}
	return nil
}

func (p *pipProvider) Install(context.Context, string, io.Writer) error { return ErrUnsupported }

func (p *pipProvider) Search(context.Context, string) ([]Result, error) { return nil, ErrUnsupported }

func (p *pipProvider) WatchPaths() []string {
	return cmdPaths(p.r, "python3", "-m", "site", "--user-site")
}
