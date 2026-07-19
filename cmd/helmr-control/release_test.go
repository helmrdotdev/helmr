package main

import (
	"context"
	"strings"
	"testing"
)

func TestRuntimeCommandRejectsMissingOrUnknownCommand(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}} {
		if err := runReleaseCommand(context.Background(), args); err == nil ||
			!strings.Contains(err.Error(), "release command") {
			t.Fatalf("args %v: error = %v", args, err)
		}
	}
}
