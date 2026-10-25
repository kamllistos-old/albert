package main

import (
	"os"
	"testing"
)

func TestConfigureChildEnvStopsHomebrewAsking(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ASK", "")
	os.Unsetenv("HOMEBREW_NO_ASK")
	configureChildEnv()
	if got := os.Getenv("HOMEBREW_NO_ASK"); got != "1" {
		t.Fatalf("HOMEBREW_NO_ASK = %q, want 1", got)
	}
}
