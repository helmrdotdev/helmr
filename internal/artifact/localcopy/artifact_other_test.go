//go:build !linux

package localcopy

import (
	"bytes"
	"context"
	"github.com/helmrdotdev/helmr/internal/cas"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

func TestArtifactCopyFailsClosedOutsideLinux(t *testing.T) {
	_, err := copyArtifact(
		context.Background(),
		t.TempDir(),
		artifact.RoleProgram,
		cas.Descriptor{},
		bytes.NewReader(nil),
	)
	if err == nil || !strings.Contains(err.Error(), "require Linux") {
		t.Fatalf("local error = %v", err)
	}
}
