package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kamllistos/albert/internal/state"
)

const (
	uninstallPromptFmt = "This removes Albert from this Mac: it stops Albert, deletes Albert's files in its settings folder (%s), removes its window image from Docker, and deletes the albert program (%s). Continue? [y/N]"
	wantBrewHint       = "Albert was installed with Homebrew. Finish with: brew uninstall albert"
)

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

// installedAlbert makes the state dir look used: a running agent, a log file.
func installedAlbert(t *testing.T, h *lcHarness) {
	t.Helper()
	seed(t, h.l.dir, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
	if err := os.WriteFile(filepath.Join(h.l.dir, logName), []byte("log"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.w.healthy = true
}

// removedLines is what a successful uninstall says it removed.
func removedLines(h *lcHarness) string {
	return "Removed the settings folder (" + h.l.dir + ").\n" +
		"Removed the albert program (" + h.exe + ").\n"
}

func assertNothingRemoved(t *testing.T, h *lcHarness) {
	t.Helper()
	if !exists(t, h.l.dir) || !exists(t, h.exe) {
		t.Fatalf("settings folder or program was removed:\n%s", h.out)
	}
	if len(h.w.killed) != 0 || len(h.d.removed) != 0 || len(h.deleted) != 0 {
		t.Fatalf("killed %v, images %v, deleted %v; want nothing", h.w.killed, h.d.removed, h.deleted)
	}
}

func TestUninstallNonTTYNeedsYes(t *testing.T) {
	h := newLifecycle(t, "y\n", false)
	installedAlbert(t, h)
	if code := h.l.uninstall(false); code != exitUsage {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitUsage, h.out)
	}
	if want := "Run \"albert uninstall --yes\" to confirm.\n"; h.out.String() != want {
		t.Fatalf("output %q, want %q", h.out, want)
	}
	assertNothingRemoved(t, h)
}

func TestUninstallTTYDeclines(t *testing.T) {
	for _, answer := range []string{"\n", "n\n", "no\n", "maybe\n", ""} {
		h := newLifecycle(t, answer, true)
		installedAlbert(t, h)
		if code := h.l.uninstall(false); code != exitFail {
			t.Fatalf("answer %q: uninstall = %d, want %d (like declining down); out:\n%s", answer, code, exitFail, h.out)
		}
		want := "This removes Albert from this Mac: it stops Albert, deletes Albert's files in its settings folder (" + h.l.dir +
			"), removes its window image from Docker, and deletes the albert program (" + h.exe +
			"). Continue? [y/N] Nothing was removed.\n"
		if h.out.String() != want {
			t.Fatalf("answer %q: output\n%q\nwant\n%q", answer, h.out, want)
		}
		assertNothingRemoved(t, h)
	}
}

func TestUninstallTTYConfirms(t *testing.T) {
	for _, answer := range []string{"y\n", "Y\n", "yes\n", " YES \n"} {
		h := newLifecycle(t, answer, true)
		installedAlbert(t, h)
		if code := h.l.uninstall(false); code != exitOK {
			t.Fatalf("answer %q: uninstall = %d; out:\n%s", answer, code, h.out)
		}
		if want := fmt.Sprintf(uninstallPromptFmt, h.l.dir, h.exe) + " " + removedLines(h) + "Albert has been removed.\n"; h.out.String() != want {
			t.Fatalf("answer %q: output\n%q\nwant\n%q", answer, h.out, want)
		}
		if exists(t, h.l.dir) || exists(t, h.exe) {
			t.Fatalf("answer %q: settings folder or program still there", answer)
		}
	}
}

func TestUninstallYesRemovesEverything(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.l.version = "1.2.3"
	installedAlbert(t, h)
	h.w.jobs = 3 // --yes means stop anyway
	if code := h.l.uninstall(true); code != exitOK {
		t.Fatalf("uninstall --yes = %d; out:\n%s", code, h.out)
	}
	if want := removedLines(h) + "Albert has been removed.\n"; h.out.String() != want {
		t.Fatalf("output\n%q\nwant\n%q", h.out, want)
	}
	if !slices.Equal(h.w.killed, []int{4242}) {
		t.Fatalf("killed %v, want the running agent", h.w.killed)
	}
	if h.w.jobsCalls != 0 {
		t.Fatalf("--yes still asked about running updates")
	}
	if !slices.Equal(h.d.removed, []string{"1.2.3"}) {
		t.Fatalf("images removed for %v, want [1.2.3]", h.d.removed)
	}
	if exists(t, h.l.dir) || exists(t, h.exe) {
		t.Fatal("settings folder or program still there")
	}
	if n := len(h.deleted); n < 2 || !slices.Equal(h.deleted[n-2:], []string{h.l.dir, h.exe}) {
		t.Fatalf("deleted %v", h.deleted)
	}
	if !exists(t, h.home) {
		t.Fatal("home folder was removed")
	}
}

func TestUninstallTTYAsksAboutRunningUpdateFirst(t *testing.T) {
	h := newLifecycle(t, "y\nn\n", true)
	installedAlbert(t, h)
	h.w.jobs = 1
	if code := h.l.uninstall(false); code != exitFail {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
	}
	if !strings.Contains(h.out.String(), "An update is still running. Stop Albert anyway? [y/N] ") {
		t.Fatalf("missing the update question:\n%s", h.out)
	}
	if !strings.HasSuffix(h.out.String(), "Nothing was removed.\n") {
		t.Fatalf("output:\n%s", h.out)
	}
	assertNothingRemoved(t, h)

	h = newLifecycle(t, "y\ny\n", true)
	installedAlbert(t, h)
	h.w.jobs = 1
	if code := h.l.uninstall(false); code != exitOK {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	if exists(t, h.l.dir) || exists(t, h.exe) || !slices.Equal(h.w.killed, []int{4242}) {
		t.Fatalf("not removed; killed %v", h.w.killed)
	}
}

func TestUninstallWhenNothingRunning(t *testing.T) {
	h := newLifecycle(t, "", false)
	if code := h.l.uninstall(true); code != exitOK {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	if h.out.String() != removedLines(h)+"Albert has been removed.\n" || len(h.w.killed) != 0 {
		t.Fatalf("output %q, killed %v", h.out, h.w.killed)
	}
}

func TestUninstallHomebrewKeepsTheProgram(t *testing.T) {
	h := newLifecycle(t, "y\n", true)
	h.exe = filepath.Join(t.TempDir(), "Cellar", "albert", "1.0.0", "bin", "albert")
	if err := os.MkdirAll(filepath.Dir(h.exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	installedAlbert(t, h)
	if code := h.l.uninstall(false); code != exitOK {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	want := "This removes Albert from this Mac: it stops Albert, deletes Albert's files in its settings folder (" + h.l.dir +
		"), and removes its window image from Docker. Continue? [y/N] Removed the settings folder (" + h.l.dir + ").\n" +
		"Albert has been removed.\n" + wantBrewHint + "\n"
	if h.out.String() != want {
		t.Fatalf("output\n%q\nwant\n%q", h.out, want)
	}
	if !exists(t, h.exe) {
		t.Fatal("the Homebrew program was deleted")
	}
	if exists(t, h.l.dir) {
		t.Fatal("settings folder still there")
	}
}

func TestUninstallHomebrewSymlinkIsResolved(t *testing.T) {
	h := newLifecycle(t, "", false)
	root := t.TempDir()
	real := filepath.Join(root, "Cellar", "albert", "1.0.0", "bin", "albert")
	link := filepath.Join(root, "bin", "albert")
	for _, d := range []string{filepath.Dir(real), filepath.Dir(link)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(real, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	h.exe = link
	if code := h.l.uninstall(true); code != exitOK {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	if !exists(t, real) || !strings.Contains(h.out.String(), wantBrewHint) {
		t.Fatalf("Homebrew program deleted or no hint:\n%s", h.out)
	}
}

func TestUninstallDeletesTheSymlinkTarget(t *testing.T) {
	h := newLifecycle(t, "", false)
	link := filepath.Join(filepath.Dir(h.exe), "albert-link")
	if err := os.Symlink(h.exe, link); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(h.exe)
	if err != nil {
		t.Fatal(err)
	}
	h.l.executable = func() (string, error) { return link, nil }
	if code := h.l.uninstall(true); code != exitOK {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	if exists(t, h.exe) || !slices.Contains(h.deleted, real) {
		t.Fatalf("deleted %v; want the real file %s gone", h.deleted, real)
	}
}

func TestIsHomebrewPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/opt/homebrew/Cellar/albert/1.0.0/bin/albert":          true,
		"/opt/homebrew/bin/albert":                              true,
		"/usr/local/Cellar/albert/1.0.0/bin/albert":             true,
		"/usr/local/Caskroom/albert/0.1.0/albert":               true,
		"/opt/homebrew/Caskroom/albert/0.1.0/albert":            true,
		"/home/linuxbrew/.linuxbrew/Cellar/albert/1/bin/albert": true,
		"/usr/local/bin/albert":                                 false,
		"/Users/me/bin/albert":                                  false,
		"/Users/me/opt/homebrew-notes/albert":                   false,
	} {
		if got := isHomebrewPath(path); got != want {
			t.Errorf("isHomebrewPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestUninstallSkipsImagesWhenDockerNotReady(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.d.readyErr = errors.New("docker is not running")
	installedAlbert(t, h)
	if code := h.l.uninstall(true); code != exitOK {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	if len(h.d.removed) != 0 {
		t.Fatalf("removed images %v although Docker isn't ready", h.d.removed)
	}
	if h.out.String() != removedLines(h)+"Albert has been removed.\n" {
		t.Fatalf("output %q: skipping Docker should be silent", h.out)
	}
	if exists(t, h.l.dir) || exists(t, h.exe) {
		t.Fatal("settings folder or program still there")
	}
}

func TestUninstallRefusesDangerousSettingsFolder(t *testing.T) {
	for name, pick := range map[string]func(h *lcHarness) string{
		"home":        func(h *lcHarness) string { return h.home },
		"home slash":  func(h *lcHarness) string { return h.home + "/" },
		"parent":      func(h *lcHarness) string { return filepath.Dir(h.home) },
		"root":        func(*lcHarness) string { return "/" },
		"empty":       func(*lcHarness) string { return "" },
		"home dotdot": func(h *lcHarness) string { return filepath.Join(h.home, "x", "..") },
	} {
		t.Run(name, func(t *testing.T) {
			h := newLifecycle(t, "", false)
			h.l.dir = pick(h)
			marker := filepath.Join(h.home, "precious.txt")
			if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			if code := h.l.uninstall(true); code != exitFail {
				t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
			}
			if len(h.deleted) != 0 || len(h.d.removed) != 0 || len(h.w.killed) != 0 {
				t.Fatalf("touched something: deleted %v, images %v, killed %v", h.deleted, h.d.removed, h.w.killed)
			}
			if !exists(t, marker) || !exists(t, h.exe) {
				t.Fatal("the home folder or the program was touched")
			}
			out := h.out.String()
			if !strings.HasPrefix(out, "Albert didn't delete its settings folder (") || !strings.HasSuffix(out, "Nothing was removed.\n") {
				t.Fatalf("output:\n%s", out)
			}
		})
	}
}

func TestUninstallPartialFailureContinues(t *testing.T) {
	h := newLifecycle(t, "", false)
	installedAlbert(t, h)
	h.d.removeErr = errors.New("image is being used by a container")
	h.l.removeFile = func(p string) error {
		if strings.HasPrefix(p, h.l.dir) {
			return errors.New("permission denied")
		}
		return os.Remove(p)
	}
	if code := h.l.uninstall(true); code != exitFail {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
	}
	out := h.out.String()
	for _, want := range []string{
		"Albert couldn't remove its window image from Docker: image is being used by a container",
		"Albert couldn't delete its settings folder (" + h.l.dir + "): permission denied",
		"Albert was not fully removed.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Albert has been removed.") {
		t.Errorf("claimed success:\n%s", out)
	}
	if exists(t, h.exe) {
		t.Error("the program should still be deleted after earlier failures")
	}
	if !slices.Equal(h.w.killed, []int{4242}) {
		t.Errorf("killed %v", h.w.killed)
	}
}

func TestUninstallProgramDeleteFailure(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.l.removeFile = func(p string) error {
		if p == h.exe {
			return errors.New("read-only file system")
		}
		return os.Remove(p)
	}
	if code := h.l.uninstall(true); code != exitFail {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	if !strings.Contains(h.out.String(), "Albert couldn't delete the albert program ("+h.exe+"): read-only file system") {
		t.Fatalf("output:\n%s", h.out)
	}
	if exists(t, h.l.dir) {
		t.Fatal("settings folder should still be removed")
	}
}

func TestUninstallCantFindProgram(t *testing.T) {
	h := newLifecycle(t, "y\n", true)
	h.l.executable = func() (string, error) { return "", errors.New("no such file") }
	if code := h.l.uninstall(false); code != exitFail {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	if strings.Contains(h.out.String(), "deletes the albert program") {
		t.Fatalf("prompt mentions a program it can't find:\n%s", h.out)
	}
	if !strings.Contains(h.out.String(), "Albert couldn't find the albert program to delete: no such file") {
		t.Fatalf("output:\n%s", h.out)
	}
	if exists(t, h.l.dir) {
		t.Fatal("settings folder should still be removed")
	}
}

func TestUninstallStopFailureRemovesNothingElse(t *testing.T) {
	h := newLifecycle(t, "", false)
	installedAlbert(t, h)
	h.l.kill = func(int) error { return errors.New("operation not permitted") }
	if code := h.l.uninstall(true); code != exitFail {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
	}
	want := "Albert couldn't stop (process 4242): operation not permitted\n" +
		"Albert couldn't be stopped, so nothing else was removed. Try again, or stop it with: kill 4242\n"
	if h.out.String() != want {
		t.Fatalf("output\n%q\nwant\n%q", h.out, want)
	}
	if !exists(t, filepath.Join(h.l.dir, logName)) || !exists(t, h.exe) || len(h.d.removed) != 0 || len(h.deleted) != 0 {
		t.Fatalf("later steps ran; images %v, deleted %v", h.d.removed, h.deleted)
	}
}

func TestUninstallCommandFlags(t *testing.T) {
	t.Setenv("ALBERT_HOME", t.TempDir())
	h := newApp("", false)
	if code := h.run("uninstall", "extra"); code != exitUsage {
		t.Errorf("uninstall extra = %d, want %d", code, exitUsage)
	}
	h = newApp("", false)
	if code := h.run("uninstall"); code != exitUsage {
		t.Errorf("uninstall (no TTY, no --yes) = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(h.out.String(), "Run \"albert uninstall --yes\" to confirm.") {
		t.Errorf("stdout %q", h.out)
	}
}

// albertTempFiles are the temp files Albert's atomic writes leave behind if
// they're interrupted (internal/state and internal/inventory).
var albertTempFiles = []string{".state.json.123456", ".token.98765", ".cache-4242.tmp"}

// userLookalikes look like Albert's files but aren't what os.CreateTemp makes.
var userLookalikes = []string{
	"thesis.docx", ".token", "state.json.bak", "cache-1.tmp",
	".token.bak", ".state.json.old", ".cache-old.tmp", ".token.", ".cache-.tmp", ".cache-12.tmp.old",
}

func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUninstallRemovesOnlyAlbertsFilesFromAFolderItDidntCreate(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.l.dir = filepath.Join(h.home, "Documents")
	if err := os.MkdirAll(filepath.Join(h.l.dir, "project", "cache.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	installedAlbert(t, h)
	writeFiles(t, h.l.dir, "cache.json")
	writeFiles(t, h.l.dir, albertTempFiles...)
	writeFiles(t, h.l.dir, userLookalikes...)
	writeFiles(t, filepath.Join(h.l.dir, "project"), "main.go")

	if code := h.l.uninstall(true); code != exitOK {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitOK, h.out)
	}
	for _, n := range append([]string{"state.json", "token", "cache.json", logName}, albertTempFiles...) {
		if exists(t, filepath.Join(h.l.dir, n)) {
			t.Errorf("Albert's file %s is still there", n)
		}
	}
	for _, n := range append(userLookalikes, "project/main.go", "project/cache.json") {
		if !exists(t, filepath.Join(h.l.dir, n)) {
			t.Errorf("the user's %s was deleted", n)
		}
	}
	if want := "Albert left " + h.l.dir + " in place because it contains files Albert didn't create.\n"; !strings.Contains(h.out.String(), want) {
		t.Fatalf("output\n%s\nwant it to contain %q", h.out, want)
	}
	if strings.Contains(h.out.String(), "Albert was not fully removed.") {
		t.Fatalf("a kept folder is not a failure:\n%s", h.out)
	}
	if want := "Removed the albert program (" + h.exe + ").\nAlbert has been removed.\n"; !strings.HasSuffix(h.out.String(), want) ||
		strings.Contains(h.out.String(), "Removed the settings folder") {
		t.Fatalf("output\n%s\nwant it to end with %q and not claim the folder", h.out, want)
	}
	if exists(t, h.exe) {
		t.Fatal("the program should still be deleted")
	}
}

func TestUninstallRemovesAFolderThatIsOnlyAlberts(t *testing.T) {
	h := newLifecycle(t, "", false)
	installedAlbert(t, h)
	writeFiles(t, h.l.dir, "cache.json")
	writeFiles(t, h.l.dir, albertTempFiles...)
	if code := h.l.uninstall(true); code != exitOK {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	if exists(t, h.l.dir) {
		t.Fatal("Albert's own settings folder is still there")
	}
	if strings.Contains(h.out.String(), "in place") {
		t.Fatalf("output:\n%s", h.out)
	}
}

func TestUninstallNeverDeletesRecursively(t *testing.T) {
	h := newLifecycle(t, "", false)
	installedAlbert(t, h)
	for _, p := range h.deleted {
		t.Fatalf("deleted %s before uninstall", p)
	}
	if code := h.l.uninstall(true); code != exitOK {
		t.Fatalf("uninstall = %d; out:\n%s", code, h.out)
	}
	// Stopping Albert already cleared state.json and token.
	want := []string{filepath.Join(h.l.dir, logName), h.l.dir, h.exe}
	if !slices.Equal(h.deleted, want) {
		t.Fatalf("deleted\n%v\nwant\n%v", h.deleted, want)
	}
}

// caseAlias returns another spelling of dir that the file system treats as the
// same folder (a different case), or "" if the disk is case-sensitive.
func caseAlias(t *testing.T, dir string) string {
	t.Helper()
	alias := filepath.Join(filepath.Dir(dir), strings.ToUpper(filepath.Base(dir)))
	a, err := os.Stat(alias)
	if err != nil {
		return ""
	}
	d, err := os.Stat(dir)
	if err != nil || !os.SameFile(a, d) {
		return ""
	}
	return alias
}

func TestUninstallRefusesOtherSpellingsOfHome(t *testing.T) {
	for name, pick := range map[string]func(t *testing.T, h *lcHarness) string{
		"case variant": func(t *testing.T, h *lcHarness) string {
			a := caseAlias(t, h.home)
			if a == "" {
				t.Skip("the disk is case-sensitive")
			}
			return a
		},
		"case variant of parent": func(t *testing.T, h *lcHarness) string {
			a := caseAlias(t, filepath.Dir(h.home))
			if a == "" {
				t.Skip("the disk is case-sensitive")
			}
			return a
		},
		"symlink to home": func(t *testing.T, h *lcHarness) string {
			link := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(h.home, link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"symlink to parent": func(t *testing.T, h *lcHarness) string {
			link := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(filepath.Dir(h.home), link); err != nil {
				t.Fatal(err)
			}
			return link
		},
		"home through a symlinked path": func(t *testing.T, h *lcHarness) string {
			link := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(filepath.Dir(h.home), link); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(link, filepath.Base(h.home))
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLifecycle(t, "", false)
			// The home folder holds files with Albert's names: none may go.
			seed(t, h.home, state.State{PID: 4242, AgentPort: 7900, UIPort: 7900, Mode: "native"})
			writeFiles(t, h.home, "cache.json", logName, "precious.txt")
			writeFiles(t, h.home, albertTempFiles...)
			h.l.dir = pick(t, h)
			if code := h.l.uninstall(true); code != exitFail {
				t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
			}
			if len(h.deleted) != 0 || len(h.w.killed) != 0 || !exists(t, h.exe) {
				t.Fatalf("touched something: deleted %v, killed %v", h.deleted, h.w.killed)
			}
			entries, err := os.ReadDir(h.home)
			if err != nil || len(entries) != 5+len(albertTempFiles) {
				t.Fatalf("home folder changed: %v %v", entries, err)
			}
			if !strings.Contains(h.out.String(), "Albert didn't delete its settings folder") || !strings.HasSuffix(h.out.String(), "Nothing was removed.\n") {
				t.Fatalf("output:\n%s", h.out)
			}
		})
	}
}

func TestUninstallCtrlCAtThePromptFails(t *testing.T) {
	h := newLifecycle(t, "", true)
	installedAlbert(t, h)
	r, w := io.Pipe() // nobody answers
	t.Cleanup(func() { w.Close() })
	h.l.in = r
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.l.ctx = ctx
	if code := h.l.uninstall(false); code != exitFail {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
	}
	if !strings.HasSuffix(h.out.String(), "Nothing was removed.\n") {
		t.Fatalf("output:\n%s", h.out)
	}
	assertNothingRemoved(t, h)
}

func TestUninstallCancelledBeforeStartingRemovesNothing(t *testing.T) {
	h := newLifecycle(t, "", false)
	installedAlbert(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.l.ctx = ctx
	if code := h.l.uninstall(true); code != exitFail {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
	}
	if h.out.String() != "Nothing was removed.\n" {
		t.Fatalf("output %q", h.out)
	}
	assertNothingRemoved(t, h)
}

func TestUninstallCtrlCPartWayStopsAndFails(t *testing.T) {
	h := newLifecycle(t, "", false)
	installedAlbert(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.l.ctx = ctx
	kill := h.l.kill
	h.l.kill = func(pid int) error { cancel(); return kill(pid) } // Ctrl-C while stopping
	if code := h.l.uninstall(true); code != exitFail {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
	}
	if h.out.String() != "Albert was not fully removed.\n" {
		t.Fatalf("output %q", h.out)
	}
	if len(h.d.removed) != 0 || len(h.deleted) != 0 || !exists(t, h.exe) {
		t.Fatalf("kept going after Ctrl-C: images %v, deleted %v", h.d.removed, h.deleted)
	}
}

func TestUninstallRefusingHomeKeepsItsStateFiles(t *testing.T) {
	h := newLifecycle(t, "", false)
	h.l.dir = h.home // ALBERT_HOME=~
	installedAlbert(t, h)
	if code := h.l.uninstall(true); code != exitFail {
		t.Fatalf("uninstall = %d, want %d; out:\n%s", code, exitFail, h.out)
	}
	for _, n := range []string{"state.json", "token", logName} {
		if !exists(t, filepath.Join(h.home, n)) {
			t.Errorf("%s was deleted from the home folder", n)
		}
	}
	if len(h.w.killed) != 0 || len(h.deleted) != 0 || len(h.d.removed) != 0 {
		t.Fatalf("touched something: killed %v, deleted %v, images %v", h.w.killed, h.deleted, h.d.removed)
	}
	want := "Albert didn't delete its settings folder (" + h.home + ") because it is your home folder.\nNothing was removed.\n"
	if h.out.String() != want {
		t.Fatalf("output\n%q\nwant\n%q", h.out, want)
	}
}
