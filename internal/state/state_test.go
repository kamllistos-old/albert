package state

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := State{PID: 42, AgentPort: 7878, UIPort: 7879, Mode: "docker", Started: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(dir)
	if err != nil || !ok {
		t.Fatalf("Load = %v, %v, %v", got, ok, err)
	}
	if got.PID != want.PID || got.AgentPort != want.AgentPort || got.UIPort != want.UIPort ||
		got.Mode != want.Mode || !got.Started.Equal(want.Started) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadMissing(t *testing.T) {
	_, ok, err := Load(t.TempDir())
	if err != nil || ok {
		t.Fatalf("Load on empty dir = ok %v, err %v; want false, nil", ok, err)
	}
}

func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Load(dir); err == nil || ok {
		t.Fatalf("Load on corrupt file = ok %v, err %v; want false, error", ok, err)
	}
}

func TestTokenFilePerm0600(t *testing.T) {
	dir := t.TempDir()
	tok, err := WriteToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(tok))
	}
	fi, err := os.Stat(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token perm = %v, want 0600", fi.Mode().Perm())
	}
	got, err := ReadToken(dir)
	if err != nil || got != tok {
		t.Fatalf("ReadToken = %q, %v; want %q", got, err, tok)
	}
	// Overwrite keeps the mode and yields a new token.
	tok2, err := WriteToken(dir)
	if err != nil || tok2 == tok {
		t.Fatalf("second WriteToken = %q, %v; want a different token", tok2, err)
	}
	fi, _ = os.Stat(filepath.Join(dir, "token"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token perm after overwrite = %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files in state dir: %v", entries)
	}
}

func TestClearRemovesStateAndToken(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, State{PID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteToken(dir); err != nil {
		t.Fatal(err)
	}
	if err := Clear(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Load(dir); ok {
		t.Fatal("state.json still present")
	}
	if _, err := ReadToken(dir); err == nil {
		t.Fatal("token still present")
	}
	if err := Clear(dir); err != nil {
		t.Fatalf("Clear on empty dir = %v, want nil", err)
	}
}

func TestDirUsesAlbertHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "custom")
	t.Setenv("ALBERT_HOME", home)
	if got := Dir(); got != home {
		t.Fatalf("Dir = %q, want %q", got, home)
	}
	fi, err := os.Stat(home)
	if err != nil || !fi.IsDir() {
		t.Fatalf("Dir did not create %s: %v", home, err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir perm = %v, want 0700", fi.Mode().Perm())
	}
}

func TestDirDefaultsToHomeDotAlbert(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ALBERT_HOME", "")
	t.Setenv("HOME", home)
	want := filepath.Join(home, ".albert")
	if got := Dir(); got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
}

func TestFreePortPrefersRequested(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	got, err := FreePort(port)
	if err != nil || got != port {
		t.Fatalf("FreePort(%d) = %d, %v; want %d", port, got, err, port)
	}
}

func TestFreePortSkipsBusy(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	busy := l.Addr().(*net.TCPAddr).Port
	got, err := FreePort(busy)
	if err != nil {
		t.Fatal(err)
	}
	if got == busy || got < busy || got > busy+50 {
		t.Fatalf("FreePort(%d) = %d; want a different port within +50", busy, got)
	}
	// The returned port must really be bindable.
	l2, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(got))
	if err != nil {
		t.Fatalf("returned port %d is not bindable: %v", got, err)
	}
	l2.Close()
}
