package snapshot

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

func TestRuntimeDescriptorDomainIsIndependentFromArtifactAdmission(t *testing.T) {
	descriptor := testRuntimeDescriptor()
	descriptor.SizeBytes = artifact.MaxRuntimePhysicalBytes + 1
	if err := artifact.ValidateRuntimeDescriptor(descriptor); err != nil {
		t.Fatalf("descriptor scalar domain rejected physical oversize: %v", err)
	}
	if _, err := CopyRuntime(
		context.Background(),
		t.TempDir(),
		descriptor,
		bytes.NewReader(nil),
	); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("runtime Artifact snapshot error = %v", err)
	}
}
