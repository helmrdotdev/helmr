//go:build !linux

package deployment

import (
	"context"
	"strings"
	"testing"
)

func TestProgramVerifierProcessFailsClosedOffLinux(t *testing.T) {
	_, err := runProgramVerifierProcess(context.Background(), programVerifierProcessConfig{})
	if err == nil || !strings.Contains(err.Error(), "requires Linux cgroup v2") {
		t.Fatalf("error = %v", err)
	}
}
