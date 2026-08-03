package control

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestValidateRunStartArmModesAndReplay(t *testing.T) {
	id := func(value byte) pgtype.UUID {
		return pgtype.UUID{Bytes: [16]byte{value}, Valid: true}
	}
	leaseID, runID, parentRunID := id(1), id(2), id(3)
	waitID, checkpointID, attachID := id(4), id(5), id(6)
	runtimeID, mountID := id(7), id(8)
	base := runStartValidationAuthority{
		run:            db.Run{ID: runID, EntrypointKind: "task"},
		parentRun:      db.Run{ID: parentRunID, Status: db.RunStatusWaiting},
		runLease:       db.RunLease{ID: leaseID, State: db.RunLeaseStateStarting},
		runtime:        db.RuntimeInstance{ID: runtimeID},
		workspace:      db.Workspace{OwnershipGeneration: 11, WriterGeneration: 13},
		workspaceMount: db.WorkspaceMount{ID: mountID, FencingGeneration: 17},
		runWait: db.RunWait{
			ID: checkpointID, SuspendCheckpointID: checkpointID, ResumeAttachID: attachID,
			CheckpointRequestVersion: 1, CheckpointAckVersion: 1,
			ResumeRequestVersion: 2, ResumeAckVersion: 1,
			CurrentRunLeaseID: leaseID, SuspensionState: db.RunWaitStateResuming,
		},
	}
	base.runWait.ID = waitID

	t.Run("fresh", func(t *testing.T) {
		if err := validateRunStartArm(runStartArm{mode: runLeaseClaimFresh}, base); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("fresh Actor", func(t *testing.T) {
		authority := base
		authority.run.ActorID = id(11)
		authority.run.EntrypointKind = "actor"
		if err := validateRunStartArm(runStartArm{mode: runLeaseClaimFresh}, authority); err != nil {
			t.Fatal(err)
		}
	})

	restore := runStartArm{mode: runLeaseClaimRestore, runWaitID: waitID,
		checkpointID: checkpointID, resumeAttachID: attachID, resumeRequestVersion: 2}
	t.Run("restore first commit", func(t *testing.T) {
		if err := validateRunStartArm(restore, base); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("restore released replay", func(t *testing.T) {
		authority := base
		authority.runLease.State = db.RunLeaseStateRunning
		authority.runWait.SuspensionState = db.RunWaitStateReleased
		authority.runWait.ResumeAckVersion = authority.runWait.ResumeRequestVersion
		if err := validateRunStartArm(restore, authority); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("restore release before commit", func(t *testing.T) {
		authority := base
		authority.runWait.SuspensionState = db.RunWaitStateReleased
		authority.runWait.ResumeAckVersion = authority.runWait.ResumeRequestVersion
		if err := validateRunStartArm(restore, authority); err == nil {
			t.Fatal("released Wait accepted before Run start commit")
		}
	})

	parent := restore
	parent.mode = runLeaseClaimAttachParent
	parentAuthority := base
	parentAuthority.runWait.Kind = db.WaitKindChild
	parentAuthority.runWait.ConditionState = db.WaitStateCompleted
	parentAuthority.runWait.ChildRunID = id(9)
	parentAuthority.runWait.ChildParentOwned = pgtype.Bool{Bool: true, Valid: true}
	parentAuthority.runWait.HandoffResumeCheckpointID = checkpointID
	parentAuthority.runWait.HandoffRuntimeInstanceID = runtimeID
	parentAuthority.runWait.HandoffWorkspaceMountID = mountID
	parentAuthority.runWait.HandoffMountGeneration = pgtype.Int8{Int64: 17, Valid: true}
	parentAuthority.runWait.OwnershipGeneration = pgtype.Int8{Int64: 11, Valid: true}
	parentAuthority.runWait.ResumeWriterGeneration = pgtype.Int8{Int64: 13, Valid: true}
	t.Run("parent first commit", func(t *testing.T) {
		if err := validateRunStartArm(parent, parentAuthority); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("parent released replay", func(t *testing.T) {
		authority := parentAuthority
		authority.runLease.State = db.RunLeaseStateRunning
		authority.runWait.SuspensionState = db.RunWaitStateReleased
		authority.runWait.ResumeAckVersion = authority.runWait.ResumeRequestVersion
		if err := validateRunStartArm(parent, authority); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("parent stale writer generation", func(t *testing.T) {
		authority := parentAuthority
		authority.runWait.ResumeWriterGeneration.Int64++
		if err := validateRunStartArm(parent, authority); err == nil {
			t.Fatal("stale resume writer generation accepted")
		}
	})

	child := runStartArm{mode: runLeaseClaimAttachChild, runWaitID: waitID,
		checkpointID: checkpointID, resumeAttachID: attachID}
	childAuthority := parentAuthority
	childAuthority.run.ParentRunID = parentRunID
	childAuthority.runWait.ConditionState = db.WaitStatePending
	childAuthority.runWait.SuspensionState = db.RunWaitStateParked
	childAuthority.runWait.ChildRunID = runID
	childAuthority.runWait.CurrentRunLeaseID = pgtype.UUID{}
	childAuthority.runWait.HandoffResumeCheckpointID = pgtype.UUID{}
	childAuthority.runWait.ChildWriterGeneration = pgtype.Int8{Int64: 13, Valid: true}
	childAuthority.runWait.ResumeWriterGeneration = pgtype.Int8{}
	childAuthority.runWait.ResumeAckVersion = childAuthority.runWait.ResumeRequestVersion
	t.Run("child first commit", func(t *testing.T) {
		if err := validateRunStartArm(child, childAuthority); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("child exact replay authority", func(t *testing.T) {
		authority := childAuthority
		authority.runLease.State = db.RunLeaseStateRunning
		authority.runWait.ConditionState = db.WaitStateCancelled
		authority.runWait.SuspensionState = db.RunWaitStateReleased
		if err := validateRunStartArm(child, authority); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("child cross-run linkage", func(t *testing.T) {
		authority := childAuthority
		authority.run.ParentRunID = id(10)
		if err := validateRunStartArm(child, authority); err == nil {
			t.Fatal("cross-Run child linkage accepted")
		}
	})
}
