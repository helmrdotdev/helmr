//go:build !linux

package deployment

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestArtifactSnapshotFailsClosedOutsideLinux(t *testing.T) {
	_, err := snapshotArtifact(
		context.Background(),
		t.TempDir(),
		codeArtifact,
		artifactSnapshotDescriptor{},
		bytes.NewReader(nil),
	)
	if err == nil || !strings.Contains(err.Error(), "require Linux") {
		t.Fatalf("snapshot error = %v", err)
	}
}
