package main

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY opens a new pseudo-terminal and returns its terminal side, which
// answers the tty driver like a Terminal window does (posix_openpt, grantpt,
// unlockpt and ptsname, done with ioctls).
func openPTY(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("can't open a pseudo-terminal here: %v", err)
	}
	t.Cleanup(func() { master.Close() })
	ioctl := func(req uintptr, arg unsafe.Pointer) {
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), req, uintptr(arg)); errno != 0 {
			t.Skipf("can't set up a pseudo-terminal here: %v", errno)
		}
	}
	ioctl(syscall.TIOCPTYGRANT, nil)
	ioctl(syscall.TIOCPTYUNLK, nil)
	name := make([]byte, 128)
	ioctl(syscall.TIOCPTYGNAME, unsafe.Pointer(&name[0]))
	if i := bytes.IndexByte(name, 0); i > 0 {
		name = name[:i]
	}
	tty, err := os.OpenFile(string(name), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("can't open %s: %v", name, err)
	}
	t.Cleanup(func() { tty.Close() })
	return tty
}

func TestIsTerminalAcceptsAPseudoTerminal(t *testing.T) {
	if !isTerminal(openPTY(t)) {
		t.Error("a pseudo-terminal was not treated as a terminal")
	}
}

func TestIsTerminalRejectsDevNull(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if isTerminal(null) {
		t.Error("/dev/null was treated as a terminal")
	}
}
