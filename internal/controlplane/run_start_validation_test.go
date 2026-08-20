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
		{name: "restore resume", mode: runLeaseClaimRestore, attempt: entered, ok: true},
		{name: "fresh cannot reenter", mode: runLeaseClaimFresh, attempt: entered},
		{name: "restore requires prior entry", mode: runLeaseClaimRestore},
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
	leaseID, runID := id(1), id(2)
	waitID, checkpointID, attachID := id(4), id(5), id(6)
	runtimeID := id(7)
	base := runStartValidationAuthority{
		run:       db.Run{ID: runID, EntrypointKind: "task"},
		runLease:  db.RunLease{ID: leaseID, State: db.RunLeaseStateStarting},
		runtime:   db.RuntimeInstance{ID: runtimeID, RestoreCheckpointID: checkpointID},
		workspace: db.Workspace{OwnershipGeneration: 11, WriterGeneration: 13},
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
	t.Run("same Workspace parent restores suspend checkpoint", func(t *testing.T) {
		authority := base
		authority.runWait.Kind = db.WaitKindChild
		authority.runWait.ConditionState = db.WaitStateCompleted
		authority.runWait.ChildRunID = id(9)
		authority.runWait.ChildParentOwned = pgtype.Bool{Bool: true, Valid: true}
		if err := validateRunStartArm(restore, authority); err != nil {
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

}
