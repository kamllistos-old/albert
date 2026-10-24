package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/kamllistos/albert/internal/inventory"
	"github.com/kamllistos/albert/internal/provider"
)

// searchTimeout bounds each provider's search.
const searchTimeout = 10 * time.Second

// scan lists the installed tools (of one source, or all) and optionally asks
// for updates. The CLI keeps no cache, so every call looks at the machine.
func (a *app) scan(source string, check bool) inventory.Snapshot {
	ctx := a.context()
	inv := inventory.New(a.providers, "")
	if source != "" {
		_ = inv.Scan(ctx, source) // a failure shows up in the provider's status
	} else {
		inv.ScanAll(ctx)
	}
	if check {
		fmt.Fprintln(a.stderr, "Checking for updates…")
		inv.CheckOutdated(ctx, source)
	}
	return inv.Snapshot()
}

// view filters a snapshot for display.
func view(snap inventory.Snapshot, source string, deps, onlyOutdated bool) []provider.Tool {
	tools := []provider.Tool{}
	for _, t := range snap.Tools {
		switch {
		case source != "" && t.Source != source,
			t.Dependency && !deps,
			onlyOutdated && !t.Outdated:
			continue
		}
		tools = append(tools, t)
	}
	return tools
}

// status is the STATUS column. Without a check it cannot say "up to date", so
// it says nothing for tools that are not outdated.
func status(t provider.Tool) string {
	switch {
	case t.ReadOnly:
		return "read-only"
	case t.Outdated:
		return "update available"
	}
	return "-"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func table(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

func printTools(w io.Writer, tools []provider.Tool) {
	tw := table(w)
	fmt.Fprintln(tw, "NAME\tSOURCE\tVERSION\tLATEST\tSTATUS")
	for _, t := range tools {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Source, dash(t.Version), dash(t.Latest), status(t))
	}
	tw.Flush()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// printFooter writes "<n> tools from <m> sources." and one line per source
// that could not be read.
func printFooter(w io.Writer, snap inventory.Snapshot, tools []provider.Tool, source string) {
	sources := map[string]bool{}
	for _, t := range tools {
		sources[t.Source] = true
	}
	fmt.Fprintf(w, "\n%s from %s.\n", plural(len(tools), "tool", "tools"), plural(len(sources), "source", "sources"))
	printProviderErrors(w, snap, source)
}

func printProviderErrors(w io.Writer, snap inventory.Snapshot, source string) {
	for _, p := range snap.Providers {
		if p.Error != "" && (source == "" || p.Name == source) {
			fmt.Fprintf(w, "! %s: %s\n", p.Name, reason(p.Name, p.Error))
		}
	}
}

// reason drops a leading "<name>: " that providers add to their errors, since
// callers print the name themselves.
func reason(name, msg string) string {
	return strings.TrimPrefix(msg, name+": ")
}

func cmdList(a *app, args []string) int {
	fs := flag.NewFlagSet("albert list", flag.ContinueOnError)
	source := fs.String("source", "", "only show tools from this source")
	outdated := fs.Bool("outdated", false, "only show tools with a newer version")
	deps := fs.Bool("deps", false, "include tools that were installed as dependencies")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	pos, code := a.parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		return a.usagef("albert list takes no arguments. Did you mean --source %s?", pos[0])
	}
	if msg := a.checkSource(*source); msg != "" {
		return a.usagef("%s", msg)
	}

	snap := a.scan(*source, *outdated)
	tools := view(snap, *source, *deps, *outdated)
	if *asJSON {
		enc := json.NewEncoder(a.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(tools); err != nil {
			return a.failf("Couldn't write the list: %v", err)
		}
		printProviderErrors(a.stderr, snap, *source)
		return exitOK
	}
	if len(tools) > 0 {
		printTools(a.stdout, tools)
	}
	printFooter(a.stdout, snap, tools, *source)
	if !*outdated {
		fmt.Fprintln(a.stdout, `Run "albert outdated" to see which tools have updates.`)
	}
	return exitOK
}

func cmdOutdated(a *app, args []string) int {
	fs := flag.NewFlagSet("albert outdated", flag.ContinueOnError)
	source := fs.String("source", "", "only check this source")
	pos, code := a.parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		return a.usagef("albert outdated takes no arguments.")
	}
	if msg := a.checkSource(*source); msg != "" {
		return a.usagef("%s", msg)
	}

	snap := a.scan(*source, true)
	tools := view(snap, *source, false, true)
	if len(tools) == 0 {
		fmt.Fprintln(a.stdout, "Everything is up to date.")
	} else {
		printTools(a.stdout, tools)
	}
	printProviderErrors(a.stdout, snap, *source)
	return exitOK
}

func cmdUpdate(a *app, args []string) int {
	fs := flag.NewFlagSet("albert update", flag.ContinueOnError)
	source := fs.String("source", "", "the source the tool comes from")
	all := fs.Bool("all", false, "update every tool that has an update")
	includePip := fs.Bool("include-pip", false, "with --all, also update pip tools")
	pos, code := a.parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	switch {
	case *all && len(pos) > 0:
		return a.usagef("Use either a tool name or --all, not both.")
	case !*all && len(pos) == 0:
		return a.usagef("Say which tool to update (albert update <tool>), or use --all.")
	case !*all && len(pos) > 1:
		return a.usagef("Update one tool at a time, or use --all.")
	case *includePip && !*all:
		return a.usagef("--include-pip only works together with --all.")
	}
	if !*all && !provider.ValidName(pos[0]) {
		return a.usagef("%q isn't a valid tool name.", pos[0])
	}
	if msg := a.checkSource(*source); msg != "" {
		return a.usagef("%s", msg)
	}

	if *all {
		return a.updateAll(*source, *includePip)
	}
	return a.updateOne(pos[0], *source)
}

func (a *app) updateOne(name, source string) int {
	snap := a.scan(source, false)
	src, err := resolveSource(snap.Tools, name, source, a.stdin, a.stdout, a.isTTY)
	if err != nil {
		return a.failf("%v", err)
	}
	p, _ := a.find(src)
	fmt.Fprintf(a.stdout, "Updating %s with %s…\n", name, src)
	if err := p.Upgrade(a.context(), []string{name}, a.stdout); err != nil {
		return a.failf("Couldn't update %q: %s", name, upgradeMessage(name, err))
	}
	fmt.Fprintf(a.stdout, "Updated %s.\n", name)
	return exitOK
}

func (a *app) updateAll(source string, includePip bool) int {
	snap := a.scan(source, true)
	var sources []string
	for _, s := range inventory.UpdatableSources(snap, includePip) {
		if source == "" || s == source {
			sources = append(sources, s)
		}
	}
	if len(sources) == 0 {
		fmt.Fprintln(a.stdout, "Everything is up to date.")
		printProviderErrors(a.stdout, snap, source)
		return exitOK
	}
	code := exitOK
	for _, s := range sources {
		p, _ := a.find(s)
		fmt.Fprintf(a.stdout, "Updating %s…\n", s)
		if err := p.Upgrade(a.context(), nil, a.stdout); err != nil {
			fmt.Fprintf(a.stderr, "Couldn't update %s: %s\n", s, upgradeMessage("", err))
			code = exitFail
			continue
		}
		fmt.Fprintf(a.stdout, "Updated %s.\n", s)
	}
	return code
}

// upgradeMessage turns an Upgrade error into plain English.
func upgradeMessage(name string, err error) string {
	if errors.Is(err, provider.ErrReadOnly) {
		if name == "" {
			name = "These tools"
		}
		return name + " is read-only: it is managed outside Albert."
	}
	return err.Error()
}

func cmdInstall(a *app, args []string) int {
	fs := flag.NewFlagSet("albert install", flag.ContinueOnError)
	source := fs.String("source", "", "the source to install from")
	pos, code := a.parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	if len(pos) != 1 {
		return a.usagef("Say which tool to install: albert install <tool>.")
	}
	name := pos[0]
	if !provider.ValidName(name) {
		return a.usagef("%q isn't a valid tool name.", name)
	}
	if msg := a.checkSource(*source); msg != "" {
		return a.usagef("%s", msg)
	}

	src, arg := *source, name
	if src == "" {
		var err error
		if src, arg, err = a.pickInstallSource(name); err != nil {
			return a.failf("%v", err)
		}
	}
	p, _ := a.find(src)
	if !p.Available() {
		return a.failf("%s isn't installed on this Mac.", src)
	}
	fmt.Fprintf(a.stdout, "Installing %s with %s…\n", name, src)
	if err := p.Install(a.context(), arg, a.stdout); err != nil {
		return a.failf("Couldn't install %q: %v", name, err)
	}
	fmt.Fprintf(a.stdout, "Installed %s.\n", name)
	return exitOK
}

// pickInstallSource searches every source for an exact name match. It returns
// the source and the name to hand to that source's Install (mas wants an ID).
func (a *app) pickInstallSource(name string) (source, arg string, err error) {
	hits := map[string]string{} // source -> install argument
	var sources []string
	for _, r := range provider.SearchAll(a.context(), a.providers, name, searchTimeout) {
		if !strings.EqualFold(r.Name, name) {
			continue
		}
		if _, dup := hits[r.Source]; dup {
			continue
		}
		arg := r.Name
		if r.ID != "" {
			arg = r.ID
		}
		hits[r.Source] = arg
		sources = append(sources, r.Source)
	}
	switch len(sources) {
	case 0:
		return "", "", fmt.Errorf("Couldn't find %q. Try \"albert search %s\".", name, name)
	case 1:
		return sources[0], hits[sources[0]], nil
	}
	source, err = chooseSource(name, "is available from", sources, a.stdin, a.stdout, a.isTTY)
	return source, hits[source], err
}

func cmdSearch(a *app, args []string) int {
	fs := flag.NewFlagSet("albert search", flag.ContinueOnError)
	pos, code := a.parseFlags(fs, args)
	if code >= 0 {
		return code
	}
	q := strings.TrimSpace(strings.Join(pos, " "))
	if q == "" {
		return a.usagef("Say what to search for: albert search <query>.")
	}

	results := provider.SearchAll(a.context(), a.providers, q, searchTimeout)
	if len(results) == 0 {
		fmt.Fprintf(a.stdout, "Nothing found for %q.\n", q)
		return exitOK
	}
	tw := table(a.stdout)
	fmt.Fprintln(tw, "NAME\tSOURCE\tVERSION\tDESCRIPTION")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Source, dash(r.Version), oneLine(r.Description, 60))
	}
	tw.Flush()
	return exitOK
}

// oneLine collapses s onto one line and shortens it to at most max characters.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-1]) + "…"
}
