//go:build !linux

package deployment

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestProgramSnapshotFailsClosedOutsideLinux(t *testing.T) {
	_, err := snapshotProgram(
		context.Background(),
		t.TempDir(),
		codeArtifact,
		ProgramDescriptor{},
		bytes.NewReader(nil),
	)
	if err == nil || !strings.Contains(err.Error(), "Linux O_TMPFILE") {
		t.Fatalf("snapshot error = %v", err)
	}
}
