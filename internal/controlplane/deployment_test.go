package controlplane

import (
	"testing"

	"uuid"
)

func TestDeploymentVersionUsesCreationDateAndID(t *testing.T) {
	id := uuid.MustParse("019b76da-a800-7000-8000-000000000000")

	if got, want := deploymentVersion(id), "20260101."+id.String(); got != want {
		t.Fatalf("deploymentVersion() = %q, want %q", got, want)
	}
}
