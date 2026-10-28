package main

import (
	"os"
	"testing"
)

func TestConfigureChildEnvStopsHomebrewAsking(t *testing.T) {
	for _, k := range []string{"HOMEBREW_NO_ASK", "HOMEBREW_NO_AUTOREMOVE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	configureChildEnv()
	for _, k := range []string{"HOMEBREW_NO_ASK", "HOMEBREW_NO_AUTOREMOVE"} {
		if got := os.Getenv(k); got != "1" {
			t.Fatalf("%s = %q, want 1", k, got)
		}
	}
}
