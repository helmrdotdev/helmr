package guestd

import (
	"slices"
	"testing"
)

func TestManagedRuntimeProcessEnvRemovesDynamicLoaderAuthority(t *testing.T) {
	environment := managedRuntimeProcessEnv([]string{
		"A=B",
		"LD_LIBRARY_PATH=/untrusted",
		"LD_PRELOAD=/untrusted/preload.so",
		"NODE_OPTIONS=--require=/untrusted/preload.cjs",
	})
	want := []string{
		"A=B",
	}
	if !slices.Equal(environment, want) {
		t.Fatalf("managed runtime environment = %q, want %q", environment, want)
	}
}
