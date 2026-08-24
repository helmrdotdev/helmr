package controlplane

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorCompletionAuthorityAcceptsOrdinaryAndRestoredWorkspaceBases(t *testing.T) {
	t.Run("ordinary", func(t *testing.T) {
		completion, authority, store := validActorCompletionAuthority(t, false)
		if err := validateActorCompletionAuthority(t.Context(), store, completion, authority); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(store.calls, "reset_target") {
			t.Fatal("ordinary completion loaded a restored Workspace base")
		}
	})

	t.Run("restored", func(t *testing.T) {
		completion, authority, store := validActorCompletionAuthority(t, true)
		if err := validateActorCompletionAuthority(t.Context(), store, completion, authority); err != nil {
			t.Fatal(err)
		}
		if store.resetTargetParams.VersionID != authority.workspaceLease.BaseVersionID {
			t.Fatalf("restored base lookup = %+v", store.resetTargetParams)
		}
	})
}

func TestActorCompletionAuthorityRejectsInvalidRestoredWorkspaceBase(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runLeaseClaimAuthority, *runLeaseClaimStore)
	}{
		{
			name: "wrong parent",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore) {
				target := store.resetTargets[store.readyCheckpoint.PrivateWorkspaceVersionID]
				target.ParentVersionID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
				store.resetTargets[store.readyCheckpoint.PrivateWorkspaceVersionID] = target
			},
		},
		{
			name: "wrong ownership generation",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore) {
				target := store.resetTargets[store.readyCheckpoint.PrivateWorkspaceVersionID]
				target.OwnershipGeneration++
				store.resetTargets[store.readyCheckpoint.PrivateWorkspaceVersionID] = target
			},
		},
		{
			name: "wrong writer generation",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore) {
				target := store.resetTargets[store.readyCheckpoint.PrivateWorkspaceVersionID]
				target.WriterGeneration++
				store.resetTargets[store.readyCheckpoint.PrivateWorkspaceVersionID] = target
			},
		},
		{
			name: "missing restore checkpoint",
			mutate: func(authority *runLeaseClaimAuthority, _ *runLeaseClaimStore) {
				authority.runtime.RestoreCheckpointID = pgtype.UUID{}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			completion, authority, store := validActorCompletionAuthority(t, true)
			test.mutate(&authority, store)
			if err := validateActorCompletionAuthority(
				t.Context(), store, completion, authority,
			); !errors.Is(err, errStaleActorCompletion) {
				t.Fatalf("error = %v, want stale Actor completion", err)
			}
		})
	}
}

func TestActorCompletionAuthorityDoesNotCompareRetryAttemptWithRunBase(t *testing.T) {
	completion, authority, store := validActorCompletionAuthority(t, false)
	authority.run.BaseWorkspaceVersionID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if authority.attempt.BaseWorkspaceVersionID != authority.workspace.HeadVersionID {
		t.Fatal("test fixture attempt does not start from the current Workspace head")
	}
	if err := validateActorCompletionAuthority(t.Context(), store, completion, authority); err != nil {
		t.Fatalf("Actor retry completion was tied to the older Run base: %v", err)
	}
}

func TestRestoredSameWorkspaceActorCompletionRejectsBrokenProducerReceipts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runLeaseClaimAuthority, *runLeaseClaimStore, pgtype.UUID, pgtype.UUID)
	}{
		{
			name: "C parent is not checkpoint P",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore, _ pgtype.UUID, cID pgtype.UUID) {
				c := store.resetTargets[cID]
				c.ParentVersionID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
				store.resetTargets[cID] = c
			},
		},
		{
			name: "C source writer differs from C receipt",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore, _ pgtype.UUID, cID pgtype.UUID) {
				c := store.resetTargets[cID]
				source := store.workspaceLeases[c.SourceWorkspaceLeaseID]
				source.WriterGeneration++
				store.workspaceLeases[c.SourceWorkspaceLeaseID] = source
			},
		},
		{
			name: "C source lease is missing",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore, _ pgtype.UUID, cID pgtype.UUID) {
				delete(store.workspaceLeases, store.resetTargets[cID].SourceWorkspaceLeaseID)
			},
		},
		{
			name: "child writer receipt differs from C",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore, _ pgtype.UUID, _ pgtype.UUID) {
				store.runWait.ChildWriterGeneration.Int64++
			},
		},
		{
			name: "writer succession is not monotonic",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore, _ pgtype.UUID, _ pgtype.UUID) {
				store.runWait.ParentWriterGeneration = store.runWait.ChildWriterGeneration
			},
		},
		{
			name: "resume writer differs from current lease",
			mutate: func(authority *runLeaseClaimAuthority, _ *runLeaseClaimStore, _ pgtype.UUID, _ pgtype.UUID) {
				authority.workspaceLease.WriterGeneration++
			},
		},
		{
			name: "child wait is not completed",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore, _ pgtype.UUID, _ pgtype.UUID) {
				store.runWait.ConditionState = db.WaitStateFailed
			},
		},
		{
			name: "wait is no longer a same Workspace child edge",
			mutate: func(_ *runLeaseClaimAuthority, store *runLeaseClaimStore, _ pgtype.UUID, _ pgtype.UUID) {
				store.runWait.Kind = db.WaitKindTimer
				store.runWait.ChildParentOwned = pgtype.Bool{}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority, store, pID, cID := validSameWorkspaceActorCompletionBase(t)
			test.mutate(&authority, store, pID, cID)
			if err := validateRestoredActorCompletionBase(
				t.Context(), store, authority, store.resetTargets[cID],
			); !errors.Is(err, errStaleActorCompletion) {
				t.Fatalf("error = %v, want stale Actor completion", err)
			}
		})
	}
}

func validSameWorkspaceActorCompletionBase(
	t *testing.T,
) (runLeaseClaimAuthority, *runLeaseClaimStore, pgtype.UUID, pgtype.UUID) {
	t.Helper()
	_, authority, store := validActorCompletionAuthority(t, true)
	pID := authority.workspaceLease.BaseVersionID
	p := store.resetTargets[pID]
	cID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	childSourceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	c := p
	c.VersionID = cID
	c.ParentVersionID = pID
	c.SourceWorkspaceLeaseID = childSourceID
	c.WriterGeneration = p.WriterGeneration + 1
	store.resetTargets[cID] = c
	store.workspaceLeases[childSourceID] = db.WorkspaceLease{
		ID: childSourceID, WorkspaceID: authority.workspace.ID,
		State: db.WorkspaceLeaseStateReleased, BaseVersionID: pID,
		OwnershipGeneration: authority.workspace.OwnershipGeneration,
		WriterGeneration:    c.WriterGeneration,
	}
	authority.workspace.WriterGeneration = c.WriterGeneration + 1
	authority.workspaceLease.BaseVersionID = cID
	authority.workspaceLease.WriterGeneration = authority.workspace.WriterGeneration
	authority.workspaceMount.MaterializedVersionID = cID
	store.runWait.Kind = db.WaitKindChild
	store.runWait.ChildParentOwned = pgtype.Bool{Bool: true, Valid: true}
	store.runWait.ConditionState = db.WaitStateCompleted
	store.runWait.BaseWorkspaceVersionID = pID
	store.runWait.ResumeWorkspaceVersionID = cID
	store.runWait.OwnershipGeneration = pgtype.Int8{Int64: authority.workspace.OwnershipGeneration, Valid: true}
	store.runWait.ParentWriterGeneration = pgtype.Int8{Int64: p.WriterGeneration, Valid: true}
	store.runWait.ChildWriterGeneration = pgtype.Int8{Int64: c.WriterGeneration, Valid: true}
	store.runWait.ResumeWriterGeneration = pgtype.Int8{Int64: authority.workspace.WriterGeneration, Valid: true}
	return authority, store, pID, cID
}

func validActorCompletionAuthority(
	t *testing.T,
	restored bool,
) (parsedActorCompletion, runLeaseClaimAuthority, *runLeaseClaimStore) {
	t.Helper()
	var authority runLeaseClaimAuthority
	if restored {
		_, _, authority = validCheckpointRestoreRunLeaseClaimFixture(true)
		restoredBase := pgvalue.UUID(uuid.Must(uuid.NewV7()))
		authority.workspaceLease.BaseVersionID = restoredBase
		authority.workspaceMount.MaterializedVersionID = restoredBase
		authority.checkpoint.PrivateWorkspaceVersionID = restoredBase
		authority.workspace.WriterGeneration = 6
		authority.workspaceLease.WriterGeneration = 6
	} else {
		_, _, authority = validActorRunLeaseClaimFixture()
	}
	now := time.Date(2026, time.August, 22, 3, 46, 27, 0, time.UTC)
	authority.run.Status = db.RunStatusRunning
	authority.run.MaxActiveDurationMs = 300_000
	authority.run.ActiveStartedAt = pgtype.Timestamptz{}
	authority.attempt.EntrypointEnteredAt = pgvalue.Timestamptz(now.Add(-time.Minute))
	authority.runLease.State = db.RunLeaseStateFinalizing
	authority.runLease.StartDeadlineAt = pgvalue.Timestamptz(now.Add(-time.Minute))
	authority.runLease.ExpiresAt = pgvalue.Timestamptz(now.Add(30 * time.Minute))
	authority.runLease.FinalizationKind = pgvalue.Text(string(workerapi.RunFinalizationCapture))
	authority.runLease.FinalizationStartedAt = pgvalue.Timestamptz(now)
	authority.runLease.FinalizationRequestFingerprint = pgvalue.Text("sha256:frozen")
	authority.workspaceLease.ExpiresAt = authority.runLease.ExpiresAt
	authority.workspaceMount.WorkspaceID = authority.workspace.ID
	authority.workspaceMount.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.WorkspaceID = authority.workspace.ID
	authority.workspaceLease.WorkspaceMountID = authority.workspaceMount.ID
	authority.workspaceLease.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceMount.MaterializedVersionID = authority.workspaceLease.BaseVersionID

	projection := runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		runLease: authority.runLease, workspace: authority.workspace,
		workspaceMount: authority.workspaceMount, workspaceLease: authority.workspaceLease,
	}
	assignment, err := projectRunLeaseAssignment(projection)
	if err != nil {
		t.Fatal(err)
	}
	capture := validTaskWorkspaceCapture(t, assignment)
	authority.runLease.FinalizationOperationID = pgvalue.UUID(
		uuid.MustParse(capture.Receipt.OperationID),
	)
	request := workerapi.CompleteActorRequest{
		Lease: assignment.Fence(),
		Outcome: workerapi.ActorOutcome{
			TerminalInputSequence: 2,
			Succeeded:             &workerapi.ActorSucceeded{},
		},
		Workspace: workerapi.TaskWorkspaceProof{Captured: capture},
	}
	completion, err := parseActorCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	base := validWorkspaceResetTargetAuthority(projection)
	store := &runLeaseClaimStore{
		authority: authority, resetTarget: base,
		finalizationClear: pgtype.Bool{Bool: true, Valid: true},
	}
	if restored {
		base.ParentVersionID = authority.workspace.HeadVersionID
		base.OwnershipGeneration = authority.workspace.OwnershipGeneration
		base.WriterGeneration = authority.workspace.WriterGeneration - 1
		sourceLeaseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
		base.SourceWorkspaceLeaseID = sourceLeaseID
		checkpointID := authority.runtime.RestoreCheckpointID
		waitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
		sourceRunLeaseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
		store.resetTargets = map[pgtype.UUID]db.GetWorkspaceResetTargetAuthorityRow{
			authority.workspaceLease.BaseVersionID: base,
		}
		store.readyCheckpoint = db.RunCheckpoint{
			ID: checkpointID, RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
			RunWaitID: waitID, SourceRunLeaseID: sourceRunLeaseID,
			SourceWorkspaceLeaseID: sourceLeaseID, WorkspaceID: authority.workspace.ID,
			BaseWorkspaceVersionID:    authority.workspace.HeadVersionID,
			PrivateWorkspaceVersionID: authority.workspaceLease.BaseVersionID,
			State:                     db.RunCheckpointStateReady,
		}
		store.runWait = db.RunWait{
			ID: waitID, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
			Kind: db.WaitKindTimer, ConditionState: db.WaitStateCompleted,
			SuspensionState: db.RunWaitStateReleased, AttemptNumber: authority.attempt.Number,
			PriorRunLeaseID: sourceRunLeaseID, SuspendCheckpointID: checkpointID,
			CheckpointRequestVersion: 1, CheckpointAckVersion: 1,
			ResumeRequestVersion: 1, ResumeAckVersion: 1,
		}
		store.workspaceLeases = map[pgtype.UUID]db.WorkspaceLease{
			sourceLeaseID: {
				ID: sourceLeaseID, WorkspaceID: authority.workspace.ID,
				State:               db.WorkspaceLeaseStateReleased,
				BaseVersionID:       authority.workspace.HeadVersionID,
				OwnershipGeneration: authority.workspace.OwnershipGeneration,
				WriterGeneration:    base.WriterGeneration,
			},
		}
	}
	return completion, authority, store
}
