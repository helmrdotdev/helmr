package main

import (
	"context"
	"strings"
	"testing"
)

func TestReleaseCommandAcceptsOnlyCompletePublishInvocation(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"publish"}, {"publish", "--store", "s3://bucket"}} {
		if err := runReleaseCommand(context.Background(), args); err == nil ||
			!strings.Contains(err.Error(), "release publish") {
			t.Fatalf("args %v: error = %v", args, err)
		}
	}
}
