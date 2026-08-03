package control

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDeploymentVersionUsesCreationDateAndID(t *testing.T) {
	id := uuid.Must(uuid.NewV7())
	seconds, nanoseconds := id.Time().UnixTime()
	createdAt := time.Unix(seconds, nanoseconds)

	if got, want := deploymentVersion(id), createdAt.UTC().Format("20060102")+"."+id.String(); got != want {
		t.Fatalf("deploymentVersion() = %q, want %q", got, want)
	}
}
