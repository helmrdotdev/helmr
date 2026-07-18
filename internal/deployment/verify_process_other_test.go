//go:build !linux

package deployment

import (
	"context"
	"strings"
	"testing"
)

func TestVerifierProcessFailsClosedOffLinux(t *testing.T) {
	_, err := runVerifierProcess(context.Background(), verifierProcessConfig{})
	if err == nil || !strings.Contains(err.Error(), "requires Linux cgroup v2") {
		t.Fatalf("error = %v", err)
	}
}
