package main

import (
	"os"
	"testing"
)

func TestIsTerminalRejectsFiles(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Error("a regular file was treated as a terminal")
	}
}
