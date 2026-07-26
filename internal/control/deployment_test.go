package control

import (
	"testing"
	"time"
)

func TestDeploymentVersionUsesCreationDateAndPublicIDPayload(t *testing.T) {
	createdAt := time.Date(2026, time.July, 26, 23, 59, 0, 0, time.FixedZone("test", 9*60*60))
	const publicID = "dep_abcdefghijklmnopqrstuvwxyz"

	if got, want := deploymentVersion(publicID, createdAt), "20260726.abcdefghijklmnopqrstuvwxyz"; got != want {
		t.Fatalf("deploymentVersion() = %q, want %q", got, want)
	}
}
