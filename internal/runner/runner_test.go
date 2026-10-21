package runner

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExecNonZeroExitIsNotError(t *testing.T) {
	res, err := Exec{}.Run(context.Background(), "sh", "-c", "echo hi; exit 3")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if string(res.Stdout) != "hi\n" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "hi\n")
	}
}

func TestExecTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Exec{}.Run(ctx, "sleep", "5")
	if err == nil {
		t.Fatal("err = nil, want non-nil on timeout")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("took %v, timeout did not stop the command", elapsed)
	}
}

func TestExecStartFailure(t *testing.T) {
	_, err := Exec{}.Run(context.Background(), "albert-no-such-binary")
	if err == nil {
		t.Fatal("err = nil, want non-nil when command cannot start")
	}
}

func TestExecStreamCombined(t *testing.T) {
	var out bytes.Buffer
	code, err := Exec{}.Stream(context.Background(), &out, "sh", "-c", "echo a; echo b 1>&2")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	got := out.String()
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") {
		t.Errorf("out = %q, want both a and b", got)
	}
}

func TestExecStreamExitCode(t *testing.T) {
	var out bytes.Buffer
	code, err := Exec{}.Stream(context.Background(), &out, "sh", "-c", "exit 4")
	if err != nil || code != 4 {
		t.Errorf("code, err = %d, %v; want 4, nil", code, err)
	}
}

func TestExecStdin(t *testing.T) {
	res, err := Exec{Stdin: strings.NewReader("piped")}.Run(context.Background(), "cat")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(res.Stdout) != "piped" {
		t.Errorf("Stdout = %q, want %q", res.Stdout, "piped")
	}
}

func TestExecLookPath(t *testing.T) {
	if _, err := (Exec{}).LookPath("sh"); err != nil {
		t.Errorf("LookPath(sh) err = %v", err)
	}
	if _, err := (Exec{}).LookPath("albert-no-such-binary"); err == nil {
		t.Error("LookPath(missing) err = nil, want non-nil")
	}
}

func TestFakeRecordsCalls(t *testing.T) {
	f := &Fake{Responses: map[string]Result{
		"brew list --versions": {Stdout: []byte("wget 1.0\n")},
	}}
	res, err := f.Run(context.Background(), "brew", "list", "--versions")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if string(res.Stdout) != "wget 1.0\n" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
	var out bytes.Buffer
	if _, err := f.Stream(context.Background(), &out, "brew", "list", "--versions"); err != nil {
		t.Fatalf("Stream err = %v", err)
	}
	want := []string{"brew list --versions", "brew list --versions"}
	if len(f.Calls) != len(want) {
		t.Fatalf("Calls = %v, want %v", f.Calls, want)
	}
	for i := range want {
		if f.Calls[i] != want[i] {
			t.Errorf("Calls[%d] = %q, want %q", i, f.Calls[i], want[i])
		}
	}
}

func TestFakeUnknownKey(t *testing.T) {
	f := &Fake{}
	res, err := f.Run(context.Background(), "npm", "ls", "-g")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.ExitCode != 127 {
		t.Errorf("ExitCode = %d, want 127", res.ExitCode)
	}
	if want := "fake: no response for npm ls -g"; string(res.Stderr) != want {
		t.Errorf("Stderr = %q, want %q", res.Stderr, want)
	}
}

func TestFakeStreamWritesStdoutThenStderr(t *testing.T) {
	f := &Fake{Responses: map[string]Result{
		"x ": {Stdout: []byte("out;"), Stderr: []byte("err;"), ExitCode: 2},
	}}
	var out bytes.Buffer
	code, err := f.Stream(context.Background(), &out, "x")
	if err != nil || code != 2 {
		t.Fatalf("code, err = %d, %v; want 2, nil", code, err)
	}
	if out.String() != "out;err;" {
		t.Errorf("out = %q, want %q", out.String(), "out;err;")
	}
}

func TestFakeLookPath(t *testing.T) {
	f := &Fake{Missing: map[string]bool{"uv": true}}
	if _, err := f.LookPath("uv"); err == nil {
		t.Error("LookPath(uv) err = nil, want non-nil")
	}
	if _, err := f.LookPath("brew"); err != nil {
		t.Errorf("LookPath(brew) err = %v", err)
	}
}

func TestFakeConcurrent(t *testing.T) {
	f := &Fake{Responses: map[string]Result{"a ": {}}}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Run(context.Background(), "a")
			f.LookPath("a")
		}()
	}
	wg.Wait()
	if len(f.Calls) != 50 {
		t.Errorf("len(Calls) = %d, want 50", len(f.Calls))
	}
}
