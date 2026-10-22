package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/kamllistos/albert/internal/runner"
)

var (
	masListLine     = regexp.MustCompile(`^\s*(\d+)\s+(.+?)\s+\((.+)\)$`)
	masOutdatedLine = regexp.MustCompile(`^(\d+)\s+(.+?)\s+\((\S+)\s+->\s+(\S+)\)$`)
	masIDOnly       = regexp.MustCompile(`^\d+$`)
)

type masProvider struct{ r runner.Runner }

// NewMas returns the Mac App Store provider.
func NewMas(r runner.Runner) Provider { return &masProvider{r: r} }

func (p *masProvider) Name() string { return "mas" }

func (p *masProvider) Available() bool {
	_, err := p.r.LookPath("mas")
	return err == nil
}

// masParse reads `mas list` / `mas search` output: ID, name and version.
func masParse(out []byte) (ids, names, versions []string) {
	for _, l := range strings.Split(string(out), "\n") {
		if m := masListLine.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil {
			ids, names, versions = append(ids, m[1]), append(names, m[2]), append(versions, m[3])
		}
	}
	return
}

func (p *masProvider) List(ctx context.Context) ([]Tool, error) {
	out, err := runOutput(ctx, p.r, "mas", "list")
	if err != nil {
		return nil, err
	}
	ids, names, versions := masParse(out)
	tools := make([]Tool, len(ids))
	for i := range ids {
		tools[i] = Tool{Name: names[i], Source: p.Name(), Version: versions[i], ID: ids[i]}
	}
	return tools, nil
}

func (p *masProvider) Outdated(ctx context.Context) ([]Update, error) {
	out, err := runOutput(ctx, p.r, "mas", "outdated")
	if err != nil {
		return nil, err
	}
	var ups []Update
	for _, l := range strings.Split(string(out), "\n") {
		if m := masOutdatedLine.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			ups = append(ups, Update{Name: m[2], Latest: m[4]})
		}
	}
	return ups, nil
}

// Upgrade upgrades the named apps (names or App Store IDs); empty names means
// all. Only numeric IDs reach the command line, so app names, which may hold
// spaces, never need to pass ValidName.
func (p *masProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if len(names) == 0 {
		return runStream(ctx, p.r, out, "mas", "upgrade")
	}
	tools, err := p.List(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]string, len(tools))
	for _, t := range tools {
		byName[t.Name] = t.ID
	}
	ids := make([]string, 0, len(names))
	for _, n := range names {
		switch id, ok := byName[n]; {
		case ok:
			ids = append(ids, id)
		case masIDOnly.MatchString(n):
			ids = append(ids, n)
		default:
			return fmt.Errorf("mas: no installed app named %q", n)
		}
	}
	return runStream(ctx, p.r, out, "mas", append([]string{"upgrade"}, ids...)...)
}

func (p *masProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if !masIDOnly.MatchString(name) {
		return errors.New("mas: use the App Store ID from search")
	}
	return runStream(ctx, p.r, out, "mas", "install", name)
}

func (p *masProvider) Search(ctx context.Context, q string) ([]Result, error) {
	q = strings.TrimSpace(q)
	if q == "" || strings.HasPrefix(q, "-") {
		return nil, nil // blank, or would be read as a flag
	}
	out, err := runOutput(ctx, p.r, "mas", "search", q)
	if err != nil {
		return nil, err
	}
	ids, names, versions := masParse(out)
	rs := make([]Result, len(ids))
	for i := range ids {
		rs[i] = Result{Name: names[i], Source: p.Name(), Version: versions[i], ID: ids[i]}
	}
	return rs, nil
}

func (p *masProvider) WatchPaths() []string { return []string{"/Applications"} }
