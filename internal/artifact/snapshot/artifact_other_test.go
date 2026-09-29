//go:build !linux

package snapshot

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

func TestArtifactSnapshotFailsClosedOutsideLinux(t *testing.T) {
	_, err := snapshotArtifact(
		context.Background(),
		t.TempDir(),
		artifact.RoleProgram,
		artifactSnapshotDescriptor{},
		bytes.NewReader(nil),
	)
	if err == nil || !strings.Contains(err.Error(), "require Linux") {
		t.Fatalf("snapshot error = %v", err)
	}
}
