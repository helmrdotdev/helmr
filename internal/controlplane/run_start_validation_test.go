package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestValidateRunStartLifecycleSeparatesFirstStartFromResume(t *testing.T) {
	entered := db.RunAttempt{EntrypointEnteredAt: pgvalue.Timestamptz(time.Now())}
	tests := []struct {
		name    string
		mode    runLeaseClaimMode
		attempt db.RunAttempt
		ok      bool
	}{
		{name: "fresh first start", mode: runLeaseClaimFresh, ok: true},
		{name: "child first start", mode: runLeaseClaimAttachChild, ok: true},
		{name: "restore resume", mode: runLeaseClaimRestore, attempt: entered, ok: true},
		{name: "parent resume", mode: runLeaseClaimAttachParent, attempt: entered, ok: true},
		{name: "fresh cannot reenter", mode: runLeaseClaimFresh, attempt: entered},
		{name: "child cannot reenter", mode: runLeaseClaimAttachChild, attempt: entered},
		{name: "restore requires prior entry", mode: runLeaseClaimRestore},
		{name: "parent requires prior entry", mode: runLeaseClaimAttachParent},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateRunStartLifecycle(test.mode, db.Run{Status: db.RunStatusQueued}, test.attempt)
			if (err == nil) != test.ok {
				t.Fatalf("validateRunStartLifecycle() error = %v, want ok=%t", err, test.ok)
			}
		})
	}
	if err := validateRunStartLifecycle(
		runLeaseClaimRestore,
		db.Run{Status: db.RunStatusRunning},
		entered,
	); err == nil {
		t.Fatal("non-queued Run accepted for first restore start commit")
	}
}

func TestStartRunCommitsCheckpointRestoreWithPriorEntrypointAndReplays(t *testing.T) {
	worker, claimLocators, authority := validCheckpointRestoreRunLeaseClaimFixture(false)
	authority.runLease.State = db.RunLeaseStateStarting
	enteredAt := pgvalue.Timestamptz(time.Now().Add(-time.Minute))
	authority.attempt.EntrypointEnteredAt = enteredAt
	store := &runLeaseClaimStore{
		authority: authority,
		startLocators: db.GetRunLeaseStartLocatorsRow{
			OrgID: claimLocators.OrgID, ProjectID: claimLocators.ProjectID,
			EnvironmentID: claimLocators.EnvironmentID, RunID: claimLocators.RunID,
			WorkspaceID: claimLocators.WorkspaceID, AttemptNumber: claimLocators.AttemptNumber,
			RegionID: claimLocators.RegionID, RuntimeInstanceID: claimLocators.RuntimeInstanceID,
			RuntimeRestoreCheckpointID: authority.runtime.RestoreCheckpointID,
			WorkspaceLeaseID:           claimLocators.WorkspaceLeaseID,
			WorkspaceMountID:           claimLocators.WorkspaceMountID,
			RunWaitID:                  claimLocators.RunWaitID,
			RunWaitCheckpointID:        claimLocators.SuspendCheckpointID,
			ResumeAttachID:             claimLocators.ResumeAttachID,
			ResumeRequestVersion:       claimLocators.ResumeRequestVersion,
		},
	}
	server := &Server{db: store}
	expected := workerapi.RunLeaseFence{
		ID:            pgvalue.UUIDString(authority.runLease.ID),
		LeaseSequence: authority.runLease.LeaseSequence,
	}
	requested := runStartArm{
		mode:                 runLeaseClaimRestore,
		runWaitID:            authority.runWait.ID,
		checkpointID:         authority.checkpoint.ID,
		resumeAttachID:       authority.runWait.ResumeAttachID,
		resumeRequestVersion: authority.runWait.ResumeRequestVersion,
	}

	receipt, err := server.startRun(
		context.Background(), worker, authority.runLease.ID, expected, requested,
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt != expected {
		t.Fatalf("receipt = %+v, want %+v", receipt, expected)
	}
	if store.authority.runLease.State != db.RunLeaseStateRunning ||
		store.authority.run.Status != db.RunStatusRunning {
		t.Fatalf("start states = lease:%q run:%q", store.authority.runLease.State, store.authority.run.Status)
	}
	if store.authority.attempt.EntrypointEnteredAt != enteredAt {
		t.Fatal("restore start changed the original Attempt entrypoint timestamp")
	}
	if store.startLeaseWrites != 1 || store.startRunWrites != 1 || store.startWorkspaceWrites != 1 {
		t.Fatalf("first start writes = lease:%d run:%d workspace:%d", store.startLeaseWrites, store.startRunWrites, store.startWorkspaceWrites)
	}

	if _, err := server.startRun(
		context.Background(), worker, authority.runLease.ID, expected, requested,
	); err != nil {
		t.Fatalf("exact start replay: %v", err)
	}
	if store.startLeaseWrites != 1 || store.startRunWrites != 1 || store.startWorkspaceWrites != 1 {
		t.Fatalf("replay mutated state: lease:%d run:%d workspace:%d", store.startLeaseWrites, store.startRunWrites, store.startWorkspaceWrites)
	}
	if store.authority.attempt.EntrypointEnteredAt != enteredAt {
		t.Fatal("restore start replay changed the original Attempt entrypoint timestamp")
	}
}

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
		authority.run.SessionID = id(11)
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
	t.Run("different Workspace child restores suspend checkpoint", func(t *testing.T) {
		authority := base
		authority.runWait.Kind = db.WaitKindChild
		authority.runWait.ConditionState = db.WaitStateCompleted
		authority.runWait.ChildRunID = id(9)
		authority.runWait.ChildParentOwned = pgtype.Bool{Bool: true, Valid: true}
		if err := validateRunStartArm(restore, authority); err != nil {
			t.Fatal(err)
		}

		_, _, claimAuthority := validCheckpointRestoreRunLeaseClaimFixture(false)
		claimAuthority.runWait.Kind = db.WaitKindChild
		claimAuthority.runWait.ChildRunID = id(10)
		claimAuthority.runWait.ChildParentOwned = pgtype.Bool{Bool: true, Valid: true}
		store := &runLeaseClaimStore{authority: claimAuthority}
		locked, err := lockRunStartCheckpointAuthority(
			context.Background(), store, runLeaseClaimRestore, claimAuthority,
		)
		if err != nil {
			t.Fatal(err)
		}
		if locked.checkpoint.ID != claimAuthority.runWait.SuspendCheckpointID ||
			locked.checkpoint.Kind != db.RunCheckpointKindSuspend {
			t.Fatalf("checkpoint = %+v, want suspend %s", locked.checkpoint, claimAuthority.runWait.SuspendCheckpointID)
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
