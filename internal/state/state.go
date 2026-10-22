// Package state reads and writes Albert's small state files: state.json
// (what is running) and token (the agent's API token).
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	stateFile = "state.json"
	tokenFile = "token"
	// portSpan is how many ports above the preferred one FreePort tries.
	portSpan = 50
)

// State describes a running Albert. The token is stored separately.
type State struct {
	PID       int       `json:"pid"`
	AgentPort int       `json:"agentPort"`
	UIPort    int       `json:"uiPort"`
	Mode      string    `json:"mode"`
	Started   time.Time `json:"started"`
}

// Dir returns the state directory: $ALBERT_HOME, else ~/.albert. It is
// created with mode 0700 if missing.
func Dir() string {
	dir := os.Getenv("ALBERT_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".albert")
	}
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// Load reads state.json. The bool is false, with a nil error, if there is no
// state file.
func Load(dir string) (State, bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, false, fmt.Errorf("read %s: %w", stateFile, err)
	}
	return s, true, nil
}

// Save writes state.json atomically.
func Save(dir string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, stateFile), b, 0o600)
}

// Clear removes state.json and the token. Missing files are not an error.
func Clear(dir string) error {
	var errs []error
	for _, name := range []string{stateFile, tokenFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WriteToken creates a new random 32-byte hex token, stores it in <dir>/token
// with mode 0600 (replacing any old one atomically) and returns it.
func WriteToken(dir string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf)
	if err := writeFileAtomic(filepath.Join(dir, tokenFile), []byte(tok), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// ReadToken returns the token stored in <dir>/token.
func ReadToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, tokenFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// FreePort returns preferred if it can be bound on 127.0.0.1, else the next
// bindable port up to preferred+50.
func FreePort(preferred int) (int, error) {
	for p := preferred; p <= preferred+portSpan && p <= 65535; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		l.Close()
		return p, nil
	}
	return 0, fmt.Errorf("no free port in %d-%d", preferred, preferred+portSpan)
}

// writeFileAtomic writes via a temp file in the same directory plus rename, so
// readers never see a partial file. The temp file is created 0600 by
// os.CreateTemp and chmod-ed to perm before the rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return err
	}
	return nil
}
