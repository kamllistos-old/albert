package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kamllistos/albert/internal/state"
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

// albertFiles are the files Albert keeps in its settings folder.
var albertFiles = []string{"state.json", "token", "cache.json", logName}

// isAlbertFile reports whether name is a file Albert creates in its settings
// folder: one of albertFiles, or a temp file its atomic writes leave behind
// if interrupted (".state.json.*" and ".token.*" from internal/state,
// ".cache-*.tmp" from internal/inventory).
func isAlbertFile(name string) bool {
	switch {
	case slices.Contains(albertFiles, name):
		return true
	case strings.HasPrefix(name, ".state.json.") && len(name) > len(".state.json."),
		strings.HasPrefix(name, ".token.") && len(name) > len(".token."):
		return true
	case strings.HasPrefix(name, ".cache-") && strings.HasSuffix(name, ".tmp") && len(name) > len(".cache-.tmp"):
		return true
	}
	return false
}

// refusal is why Albert won't touch anything in a settings folder.
type refusal struct{ reason string }

func (r refusal) Error() string { return r.reason }

// guardDir returns a refusal if dir (described by info) is the user's home
// folder, a folder that contains it, or the top of the disk. Folders are
// compared by identity (os.SameFile), not by name, so other spellings of the
// same folder are caught too: a different case on a case-insensitive disk, a
// symlink, or a firmlink such as /System/Volumes/Data/Users/me.
func (l *lifecycle) guardDir(info os.FileInfo) error {
	if root, err := os.Stat(string(filepath.Separator)); err != nil {
		return refusal{"the top of the disk can't be checked"}
	} else if os.SameFile(info, root) {
		return refusal{"it is the top of the disk"}
	}
	home, err := l.homeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return refusal{"your home folder can't be found"}
	}
	// Walk up from home both as spelled and with symlinks resolved, so every
	// folder that contains it is checked.
	for _, h := range []string{home, realPath(home)} {
		h, err := filepath.Abs(h)
		if err != nil {
			return refusal{"your home folder can't be found"}
		}
		for p := h; ; p = filepath.Dir(p) {
			if pi, err := os.Stat(p); err == nil && os.SameFile(info, pi) {
				if p == h {
					return refusal{"it is your home folder"}
				}
				return refusal{"it contains your home folder"}
			}
			if filepath.Dir(p) == p {
				break
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

// settingsResult is what removeSettings did with the settings folder.
type settingsResult int

const (
	settingsGone    settingsResult = iota // there was no settings folder
	settingsRemoved                       // Albert's files and the folder are gone
	settingsKept                          // Albert's files are gone; the folder holds others
)

// removeSettings deletes Albert's own files from dir, then dir itself if
// nothing else is in it. It never deletes recursively: ALBERT_HOME can point
// anywhere, so only files Albert creates are touched, and a folder that also
// holds anything else is left in place. A refusal means nothing was touched.
func (l *lifecycle) removeSettings(dir string) (settingsResult, error) {
	if strings.TrimSpace(dir) == "" {
		return 0, refusal{"it has no location"}
	}
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return settingsGone, nil
	}
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, refusal{"it is not a folder"}
	}
	if err := l.guardDir(info); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !isAlbertFile(e.Name()) {
			continue
		}
		if err := l.removeFile(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return 0, err
	}
	if left, err := os.ReadDir(dir); err != nil {
		return 0, err
	} else if len(left) > 0 {
		return settingsKept, nil
	}
	if err := l.removeFile(dir); err != nil {
		return 0, err
	}
	return settingsRemoved, nil
}

// uninstall removes Albert from this Mac: it stops Albert, removes its
// Docker image, deletes its own files from its settings folder (and the
// folder, if nothing else is in it) and deletes the albert program
// (unless Homebrew owns it). If Albert can't be stopped nothing else is
// removed; any other failed step is reported and the rest still run. Without yes it asks first, and only in a terminal.
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
	// If it can't be stopped, nothing else is removed: deleting the program
	// and its files under a running agent would leave it orphaned.
	st, _, _ := state.Load(l.dir)
	switch l.stopAlbert(yes) {
	case stopDeclined:
		fmt.Fprintln(l.out, "Nothing was removed.")
		return exitFail
	case stopFailed: // stopAlbert already said why
		if st.PID > 1 {
			fmt.Fprintf(l.out, "Albert couldn't be stopped, so nothing else was removed. Try again, or stop it with: kill %d\n", st.PID)
		} else {
			fmt.Fprintln(l.out, "Albert couldn't be stopped, so nothing else was removed. Try again.")
		}
		return exitFail
	}

	// 2. Docker's copy of the window. No Docker, nothing to remove.
	if l.docker.Ready(l.context()) == nil {
		if err := l.docker.RemoveImages(l.context(), l.version); err != nil {
			fail("Albert couldn't remove its window image from Docker: %v", err)
		}
	}

	// 3. The settings folder.
	var refused refusal
	switch res, err := l.removeSettings(l.dir); {
	case errors.As(err, &refused):
		fail("Albert didn't delete its settings folder (%s) because %v.", l.dir, refused)
	case err != nil:
		fail("Albert couldn't delete its settings folder (%s): %v", l.dir, err)
	case res == settingsKept:
		fmt.Fprintf(l.out, "Albert left %s in place because it contains files Albert didn't create.\n", l.dir)
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
