//go:build !darwin

package main

import "os"

// isTerminal reports whether f is a character device. Albert ships for macOS
// only (see terminal_darwin.go); this approximation, which also accepts
// /dev/null, just lets the package build elsewhere.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
