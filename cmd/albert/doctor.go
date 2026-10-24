package main

import (
	"fmt"
	"strings"
)

// cmdDoctor shows which package managers Albert can use. It always exits 0:
// a missing package manager is information, not a failure.
func cmdDoctor(a *app, args []string) int {
	if len(args) > 0 {
		return a.usagef("albert doctor takes no arguments.")
	}
	snap := a.scan("", false)
	for _, p := range snap.Providers {
		switch {
		case !p.Available:
			fmt.Fprintf(a.stdout, "– %s (not installed)\n", p.Name)
		case p.Error != "":
			fmt.Fprintf(a.stdout, "✗ %s: %s\n", p.Name, reason(p.Name, p.Error))
		default:
			fmt.Fprintf(a.stdout, "✓ %s\n", p.Name)
		}
	}
	if err := a.docker.Ready(a.context()); err != nil {
		why := "not running"
		if strings.Contains(err.Error(), "not installed") {
			why = "not installed"
		}
		fmt.Fprintf(a.stdout, "✗ Docker: %s\n", why)
	} else {
		fmt.Fprintln(a.stdout, "✓ Docker")
	}
	return exitOK
}
