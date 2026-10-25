package main

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f is an interactive terminal. A character device
// is not enough: /dev/null is one too, so ask the tty driver directly.
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&t)), 0, 0, 0)
	return errno == 0
}
