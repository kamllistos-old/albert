package provider

import (
	"context"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kamllistos/albert/internal/runner"
)

var (
	gemListLine     = regexp.MustCompile(`^(\S+) \((.+)\)$`)
	gemOutdatedLine = regexp.MustCompile(`^(\S+) \((\S+) < (\S+)\)$`)
)

type gemProvider struct{ r runner.Runner }

// NewGem returns the RubyGems provider.
func NewGem(r runner.Runner) Provider { return &gemProvider{r: r} }

func (p *gemProvider) Name() string { return "gem" }

func (p *gemProvider) Available() bool {
	_, err := p.r.LookPath("gem")
	return err == nil
}

// gemDir returns `gem env gemdir`, or "" if it cannot be read.
func (p *gemProvider) gemDir() string {
	if dirs := cmdPaths(p.r, "gem", "env", "gemdir"); len(dirs) > 0 {
		return dirs[0]
	}
	return ""
}

// systemGemDir reports whether dir belongs to macOS's own Ruby, which Albert
// must not touch.
func systemGemDir(dir string) bool {
	return strings.HasPrefix(dir, "/System/") || strings.HasPrefix(dir, "/Library/")
}

// gemParse reads `gem list` / `gem search` lines into name and first version.
// Default gems, which ship with Ruby, are skipped when skipDefault is set.
func gemParse(out []byte, skipDefault bool) (names, versions []string) {
	for _, l := range strings.Split(string(out), "\n") {
		m := gemListLine.FindStringSubmatch(strings.TrimSpace(l))
		if m == nil || skipDefault && strings.HasPrefix(m[2], "default:") {
			continue
		}
		first, _, _ := strings.Cut(m[2], ",")
		v := strings.Fields(first) // drop a platform suffix such as "java"
		if len(v) == 0 {
			continue
		}
		names, versions = append(names, m[1]), append(versions, v[0])
	}
	return
}

func (p *gemProvider) List(ctx context.Context) ([]Tool, error) {
	out, err := runOutput(ctx, p.r, "gem", "list", "--local")
	if err != nil {
		return nil, err
	}
	dir := p.gemDir()
	readOnly := systemGemDir(dir)
	names, versions := gemParse(out, true)
	tools := make([]Tool, len(names))
	for i := range names {
		t := Tool{Name: names[i], Source: p.Name(), Version: versions[i], ReadOnly: readOnly}
		if dir != "" {
			t.Path = filepath.Join(dir, "gems", names[i]+"-"+versions[i])
		}
		tools[i] = t
	}
	return tools, nil
}

func (p *gemProvider) Outdated(ctx context.Context) ([]Update, error) {
	out, err := runOutput(ctx, p.r, "gem", "outdated")
	if err != nil {
		return nil, err
	}
	var ups []Update
	for _, l := range strings.Split(string(out), "\n") {
		if m := gemOutdatedLine.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			ups = append(ups, Update{Name: m[1], Latest: m[3]})
		}
	}
	return ups, nil
}

func (p *gemProvider) Upgrade(ctx context.Context, names []string, out io.Writer) error {
	if systemGemDir(p.gemDir()) {
		return ErrReadOnly
	}
	if err := checkNames("gem", names); err != nil {
		return err
	}
	return runStream(ctx, p.r, out, "gem", append([]string{"update"}, names...)...)
}

func (p *gemProvider) Install(ctx context.Context, name string, out io.Writer) error {
	if err := checkNames("gem", []string{name}); err != nil {
		return err
	}
	return runStream(ctx, p.r, out, "gem", "install", "--user-install", name)
}

func (p *gemProvider) Search(ctx context.Context, q string) ([]Result, error) {
	q = strings.TrimSpace(q)
	if !ValidName(q) {
		return nil, nil // cannot be a gem name, and must not look like a flag
	}
	out, err := runOutput(ctx, p.r, "gem", "search", q)
	if err != nil {
		return nil, err
	}
	names, versions := gemParse(out, false)
	rs := make([]Result, len(names))
	for i := range names {
		rs[i] = Result{Name: names[i], Source: p.Name(), Version: versions[i]}
	}
	return rs, nil
}

func (p *gemProvider) WatchPaths() []string {
	if dir := p.gemDir(); dir != "" {
		return []string{filepath.Join(dir, "gems")}
	}
	return nil
}
