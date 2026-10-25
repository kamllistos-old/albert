package main

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
)

// brewHint is shown when Homebrew owns the albert program.
const brewHint = "Albert was installed with Homebrew. Finish with: brew uninstall albert"

// isHomebrewPath reports whether path is a program Homebrew installed (or
// links to): deleting it behind brew's back would leave brew confused.
func isHomebrewPath(path string) bool {
	p := filepath.ToSlash(path)
	return strings.Contains(p, "/Cellar/") || strings.Contains(p, "/Caskroom/") || strings.HasPrefix(p, "/opt/homebrew/")
}

// program is the albert program uninstall will delete.
type program struct {
	path string // resolved, symlink-free path; "" if it couldn't be found
	err  error  // why it couldn't be found
	brew bool   // Homebrew owns it, so it must not be deleted
}

// locateProgram finds the running albert program, following symlinks.
func (l *lifecycle) locateProgram() program {
	exe, err := l.executable()
	if err != nil {
		return program{err: err}
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		resolved = exe // deleting it will report the real problem
	}
	return program{path: resolved, brew: isHomebrewPath(exe) || isHomebrewPath(resolved)}
}

// removableDir returns an error if dir must never be deleted: nothing, the
// root, the user's home folder, or a folder that contains the home folder.
func (l *lifecycle) removableDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("it has no location")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if abs == string(filepath.Separator) {
		return fmt.Errorf("it is the top of the disk")
	}
	home, err := l.homeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return fmt.Errorf("your home folder can't be found")
	}
	for _, d := range []string{abs, realPath(abs)} {
		for _, h := range []string{filepath.Clean(home), realPath(filepath.Clean(home))} {
			if d == h {
				return fmt.Errorf("it is your home folder")
			}
			if rel, err := filepath.Rel(d, h); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("it contains your home folder")
			}
		}
	}
	return nil
}

// realPath resolves symlinks if it can, else returns p unchanged.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// uninstall removes Albert from this Mac: it stops Albert, removes its
// Docker image, deletes its settings folder and deletes the albert program
// (unless Homebrew owns it). A failed step is reported and the rest still
// run. Without yes it asks first, and only in a terminal.
func (l *lifecycle) uninstall(yes bool) int {
	prog := l.locateProgram()
	if !yes {
		if !l.tty {
			fmt.Fprintln(l.out, `Run "albert uninstall --yes" to confirm.`)
			return exitUsage
		}
		fmt.Fprintf(l.out, "This removes Albert from this Mac: it stops Albert, deletes its settings folder (%s), ", l.dir)
		if prog.path != "" && !prog.brew {
			fmt.Fprintf(l.out, "removes its window image from Docker, and deletes the albert program (%s). Continue? [y/N] ", prog.path)
		} else {
			fmt.Fprint(l.out, "and removes its window image from Docker. Continue? [y/N] ")
		}
		answer, ok := l.readLine(l.context())
		if a := strings.ToLower(strings.TrimSpace(answer)); !ok || (a != "y" && a != "yes") {
			fmt.Fprintln(l.out, "Nothing was removed.")
			return exitOK
		}
	}

	failed := false
	fail := func(format string, args ...any) {
		failed = true
		fmt.Fprintf(l.out, format+"\n", args...)
	}

	// 1. Stop Albert. With --yes the user already accepted cutting off an
	// update; without it, stopAlbert asks the same question as "albert down".
	switch l.stopAlbert(yes) {
	case stopDeclined:
		fmt.Fprintln(l.out, "Nothing was removed.")
		return exitFail
	case stopFailed:
		failed = true // stopAlbert already said why
	}

	// 2. Docker's copy of the window. No Docker, nothing to remove.
	if l.docker.Ready(l.context()) == nil {
		if err := l.docker.RemoveImages(l.context(), l.version); err != nil {
			fail("Albert couldn't remove its window image from Docker: %v", err)
		}
	}

	// 3. The settings folder.
	if err := l.removableDir(l.dir); err != nil {
		fail("Albert didn't delete its settings folder (%s) because %v.", l.dir, err)
	} else if err := l.removeAll(l.dir); err != nil {
		fail("Albert couldn't delete its settings folder (%s): %v", l.dir, err)
	}

	// 4. The program itself. A running program can be deleted on macOS.
	switch {
	case prog.err != nil:
		fail("Albert couldn't find the albert program to delete: %v", prog.err)
	case prog.brew:
	default:
		if err := l.removeFile(prog.path); err != nil {
			fail("Albert couldn't delete the albert program (%s): %v", prog.path, err)
		}
	}

	if failed {
		fmt.Fprintln(l.out, "Albert was not fully removed.")
	} else {
		fmt.Fprintln(l.out, "Albert has been removed.")
	}
	if prog.brew {
		fmt.Fprintln(l.out, brewHint)
	}
	if failed {
		return exitFail
	}
	return exitOK
}

func cmdUninstall(a *app, args []string) int {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	yes := flags.Bool("yes", false, "don't ask first")
	pos, code := a.parseFlags(flags, args)
	if code >= 0 {
		return code
	}
	if len(pos) > 0 {
		return a.usagef("albert uninstall takes no arguments. Option: --yes.")
	}
	return a.lifecycle().uninstall(*yes)
}
