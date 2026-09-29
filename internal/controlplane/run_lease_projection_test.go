package controlplane

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestProjectSecretDeliveriesUsesCanonicalPlacementOrder(t *testing.T) {
	deliveries, err := projectSecretDeliveries([]secret.DeliveryMaterial{
		{PlacementKind: "file", PlacementTarget: "/run/token", Value: []byte("file-value")},
		{PlacementKind: "env", PlacementTarget: "TOKEN", Value: []byte("env-value")},
	})
	if err != nil {
		t.Fatalf("projectSecretDeliveries: %v", err)
	}
	if len(deliveries) != 2 ||
		deliveries[0].Env == nil ||
		deliveries[0].Env.Name != "TOKEN" ||
		deliveries[1].File == nil ||
		deliveries[1].File.Path != "/run/token" {
		t.Fatalf("unexpected Secret delivery order: %#v", deliveries)
	}
}

func TestProjectRunLeaseAssignmentAndComputer(t *testing.T) {
	authority := validRunLeaseProjectionAuthority()
	authority.run.BaseComputerDiskVersionID = pgvalue.UUID(uuid.NewV7())
	authority.runtime.SourceDiskVersionID = pgvalue.UUID(uuid.NewV7())
	assignment, err := projectRunLeaseAssignment(authority)
	if err != nil {
		t.Fatalf("projectRunLeaseAssignment: %v", err)
	}
	if assignment.LeaseSequence != 2 ||
		assignment.ComputerID != pgvalue.UUIDString(authority.computer.ID) ||
		assignment.BaseComputerDiskVersionID != pgvalue.UUIDString(authority.attempt.BaseComputerDiskVersionID) ||
		assignment.WriterGeneration != authority.runtime.WriterGeneration ||
		assignment.MaxActiveDurationMs != authority.run.MaxActiveDurationMs {
		t.Fatalf("unexpected Run Lease assignment: %#v", assignment)
	}
	authority.runLease.StartDeadlineAt = authority.runLease.ExpiresAt
	if _, err := projectRunLeaseAssignment(authority); err != nil {
		t.Fatalf("equal Run Lease deadlines: %v", err)
	}
	resetAuthority := validComputerMountTargetAuthority(authority)
	computer, err := projectComputerAttachment(authority, "write-capability", resetAuthority)
	if err != nil {
		t.Fatalf("projectComputerAttachment: %v", err)
	}
	if computer.WriteCapability != "write-capability" ||
		computer.Target.BaseComputerDiskVersionID != assignment.BaseComputerDiskVersionID {
		t.Fatalf("unexpected Computer attachment: %#v", computer)
	}

	for name, mutate := range map[string]func(*runLeaseProjectionAuthority){
		"writer generation": func(a *runLeaseProjectionAuthority) { a.runtime.WriterGeneration++ },
		"Instance":          func(a *runLeaseProjectionAuthority) { a.runtime.ID = pgvalue.UUID(uuid.New()) },
		"Computer":          func(a *runLeaseProjectionAuthority) { a.runtime.ComputerID = pgvalue.UUID(uuid.New()) },
		"Worker host":       func(a *runLeaseProjectionAuthority) { a.runtime.WorkerHostID = pgvalue.UUID(uuid.New()) },
		"Worker group":      func(a *runLeaseProjectionAuthority) { a.runtime.WorkerGroupID = pgvalue.UUID(uuid.New()) },
		"Worker epoch":      func(a *runLeaseProjectionAuthority) { a.runtime.WorkerEpoch++ },
		"environment":       func(a *runLeaseProjectionAuthority) { a.runtime.EnvironmentID = pgvalue.UUID(uuid.New()) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := authority
			mutate(&changed)
			if _, err := projectComputerAttachment(changed, "write-capability", resetAuthority); err == nil {
				t.Fatal("mismatched physical authority accepted")
			}
		})
	}
}

func validComputerMountTargetAuthority(
	authority runLeaseProjectionAuthority,
) db.GetComputerDiskVersionAuthorityRow {
	return db.GetComputerDiskVersionAuthorityRow{
		VersionID: authority.attempt.BaseComputerDiskVersionID,
	}
}

func TestProjectComputerAttachmentAcceptsGenerationOnlyVersion(t *testing.T) {
	authority := validRunLeaseProjectionAuthority()
	version := db.GetComputerDiskVersionAuthorityRow{VersionID: authority.attempt.BaseComputerDiskVersionID, ParentVersionID: pgvalue.UUID(uuid.New()), SourceComputerInstanceID: pgvalue.UUID(uuid.New()), WriterGeneration: 6}
	attachment, err := projectComputerAttachment(authority, "write-capability", version)
	if err != nil {
		t.Fatal(err)
	}
	if attachment.Target.BaseComputerDiskVersionID != pgvalue.UUIDString(version.VersionID) {
		t.Fatalf("attachment = %+v", attachment)
	}
	version.VersionID = pgvalue.UUID(uuid.New())
	if _, err := projectComputerAttachment(authority, "write-capability", version); err == nil {
		t.Fatal("wrong version accepted")
	}
}

func validRunLeaseProjectionAuthority() runLeaseProjectionAuthority {
	runID := pgvalue.UUID(uuid.NewV7())
	computerID := pgvalue.UUID(uuid.NewV7())
	versionID := pgvalue.UUID(uuid.NewV7())
	attemptNumber := int32(1)
	runtimeID := pgvalue.UUID(uuid.New())
	workerID := pgvalue.UUID(uuid.New())
	groupID := pgvalue.UUID(uuid.New())
	runLeaseID := pgvalue.UUID(uuid.New())
	now := time.Now().UTC()
	return runLeaseProjectionAuthority{
		run: db.Run{
			ID: runID, ComputerID: computerID, BaseComputerDiskVersionID: versionID, CurrentAttemptNumber: attemptNumber,
			MaxActiveDurationMs: 300000, ActiveElapsedMs: 1000,
		},
		attempt: db.RunAttempt{RunID: runID, Number: attemptNumber, ComputerID: computerID, BaseComputerDiskVersionID: versionID},
		runtime: db.ComputerInstance{ID: runtimeID, ComputerID: computerID, WorkerGroupID: groupID, WorkerHostID: workerID, WorkerEpoch: 3, WriterGeneration: 6, VMPlatformID: "runtime"},
		runLease: db.RunLease{
			ID: runLeaseID, RunID: runID, ComputerID: computerID,
			AttemptNumber: attemptNumber, LeaseSequence: 2,
			WorkerGroupID: groupID, WorkerHostID: workerID,
			WorkerEpoch: 3, ComputerInstanceID: runtimeID,
			RequestedCPUMillis: 1000, RequestedMemoryBytes: 1024,
			RequestedGuestEphemeralDiskBytes: 2048,
			RequestedExecutionSlots:          1,
			StartDeadlineAt:                  pgtype.Timestamptz{Time: now.Add(time.Minute), Valid: true},
			ExpiresAt:                        pgtype.Timestamptz{Time: now.Add(5 * time.Minute), Valid: true},
		},
		computer: db.LockRunLeaseClaimComputerRow{
			ID: computerID, WriterGeneration: 6,
		},
	}
}

func validDigest(fill byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = fill
	}
	return "sha256:" + string(value)
}

func validDigestBytes(t *testing.T, fill byte) []byte {
	t.Helper()
	value, err := artifact.RuntimeDigestBytes(validDigest(fill))
	if err != nil {
		t.Fatal(err)
	}
	return value
}
