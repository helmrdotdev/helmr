package controlplane

import (
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestCheckpointFailureSettlementDoesNotGrantExecution(t *testing.T) {
	for _, mount := range []db.WorkspaceMountStatus{db.WorkspaceMountStatusMounted, db.WorkspaceMountStatusUnmounting, db.WorkspaceMountStatusFailed} {
		t.Run(string(mount), func(t *testing.T) {
			worker, _, authority := validRunLeaseClaimFixture()
			authority.runLease.Status = db.RunLeaseStatusCheckpointing
			authority.runtime.ObservedState = db.RuntimeObservedStateFailed
			authority.runtime.TerminalReasonCode = pgvalue.Text("guest_exited")
			authority.workspaceMount.Status = mount
			if err := validateCheckpointFailureAuthority(worker, authority); err != nil {
				t.Fatalf("failure settlement rejected: %v", err)
			}
			if err := validateExecutingRunLeaseAuthority(worker, authority); !errors.Is(err, errStaleRunLeaseClaim) {
				t.Fatalf("failed source allowed to execute: %v", err)
			}
			for _, field := range []string{"worker_epoch", "runtime_identity", "writer", "mount_fence", "run_owner"} {
				bad := authority
				switch field {
				case "worker_epoch":
					bad.runLease.WorkerEpoch++
				case "runtime_identity":
					bad.runtime.RuntimeIdentityID = "other"
				case "writer":
					bad.workspaceLease.WriterGeneration++
				case "mount_fence":
					bad.workspaceLease.MountFencingGeneration++
				case "run_owner":
					bad.workspaceLease.OwnerRunLeaseID = pgvalue.UUID(uuid.NewV7())
				}
				if err := validateCheckpointFailureAuthority(worker, bad); !errors.Is(err, errStaleRunLeaseClaim) {
					t.Fatalf("stale %s allowed: %v", field, err)
				}
			}
		})
	}
}
