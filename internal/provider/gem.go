package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// userDir returns the user gem directory, or "" if it cannot be read.
func (p *gemProvider) userDir() string {
	if dirs := cmdPaths(p.r, "ruby", "-e", "print Gem.user_dir"); len(dirs) > 0 {
		return dirs[0]
	}
	return ""
}

// systemGemDir reports whether dir belongs to macOS's own Ruby, which Albert
// must not touch.
func systemGemDir(dir string) bool {
	return strings.HasPrefix(dir, "/System/") || strings.HasPrefix(dir, "/Library/")
}

// writableGemDir reports whether gems in dir can be changed without sudo. An
// unknown dir fails closed.
func writableGemDir(dir string) bool { return dir != "" && !systemGemDir(dir) }

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
	return p.list(ctx, p.gemDir())
}

// list builds the tool list for the given gem dir. A gem is read-only unless
// its dir is writable or it lives under the user gem dir, which stays
// writable even on system Ruby.
func (p *gemProvider) list(ctx context.Context, dir string) ([]Tool, error) {
	out, err := runOutput(ctx, p.r, "gem", "list", "--local")
	if err != nil {
		return nil, err
	}
	var user string
	if !writableGemDir(dir) {
		user = p.userDir()
	}
	names, versions := gemParse(out, true)
	tools := make([]Tool, len(names))
	for i := range names {
		t := Tool{Name: names[i], Source: p.Name(), Version: versions[i], ReadOnly: !writableGemDir(dir)}
		leaf := names[i] + "-" + versions[i]
		if user != "" {
			if up := filepath.Join(user, "gems", leaf); isDir(up) {
				t.Path, t.ReadOnly = up, false
			}
		}
		if t.Path == "" && dir != "" {
			t.Path = filepath.Join(dir, "gems", leaf)
		}
		tools[i] = t
	}
	return tools, nil
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
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
	if err := checkNames("gem", names); err != nil {
		return err
	}
	dir := p.gemDir()
	if writableGemDir(dir) {
		return runStream(ctx, p.r, out, "gem", append([]string{"update"}, names...)...)
	}
	// System Ruby, or gemdir unknown: only gems under the user gem dir can be
	// updated, with --user-install.
	tools, err := p.list(ctx, dir)
	if err != nil {
		return err
	}
	writable := make(map[string]bool, len(tools))
	known := make(map[string]bool, len(tools))
	var all []string
	for _, t := range tools {
		known[t.Name] = true
		if !t.ReadOnly {
			writable[t.Name] = true
			all = append(all, t.Name)
		}
	}
	if len(names) == 0 {
		names = all
	}
	if len(names) == 0 {
		return ErrReadOnly
	}
	for _, n := range names {
		if !writable[n] {
			if !known[n] {
				return fmt.Errorf("gem: %q is not installed", n)
			}
			return ErrReadOnly
		}
	}
	return runStream(ctx, p.r, out, "gem", append([]string{"update", "--user-install"}, names...)...)
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
	var ps []string
	if dir := p.gemDir(); dir != "" {
		ps = append(ps, filepath.Join(dir, "gems"))
	}
	if user := p.userDir(); user != "" {
		if up := filepath.Join(user, "gems"); !slices.Contains(ps, up) {
			ps = append(ps, up)
		}
	}
	return ps
}
