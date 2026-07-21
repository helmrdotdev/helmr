package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTaskCompletionReplayUsesOnlyTerminalReceipt(t *testing.T) {
	workerID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	leaseID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	runID := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	workspaceID := uuid.MustParse("00000000-0000-0000-0000-000000000004")
	worker := workerActor{WorkerInstanceID: workerID, WorkerGroupID: "workers"}
	request := api.WorkerCompleteTaskRequest{Lease: api.WorkerRunLeaseReceipt{
		AttemptNumber: 2,
		LeaseSequence: 7,
	}}
	completion := parsedTaskCompletion{
		lease: parsedRunLeaseReceipt{
			leaseID: leaseID, runID: runID, workspaceID: workspaceID,
		},
		fingerprint: "sha256:receipt",
	}
	store := &taskCompletionReplayFixture{fingerprint: pgvalue.Text(completion.fingerprint)}

	replayed, err := taskCompletionWasReplayed(context.Background(), store, worker, request, completion)
	if err != nil || !replayed {
		t.Fatalf("replay = %t, %v", replayed, err)
	}
	if store.last.RunLeaseID != pgvalue.UUID(leaseID) ||
		store.last.RunID != pgvalue.UUID(runID) ||
		store.last.WorkspaceID != pgvalue.UUID(workspaceID) ||
		store.last.AttemptNumber != 2 || store.last.LeaseSequence != 7 ||
		store.last.WorkerGroupID != worker.WorkerGroupID ||
		store.last.WorkerInstanceID != pgvalue.UUID(workerID) {
		t.Fatalf("unexpected replay selector: %+v", store.last)
	}
}

func TestTaskCompletionReplayRejectsChangedFingerprint(t *testing.T) {
	store := &taskCompletionReplayFixture{fingerprint: pgvalue.Text("sha256:committed")}
	_, err := taskCompletionWasReplayed(
		context.Background(),
		store,
		workerActor{},
		api.WorkerCompleteTaskRequest{},
		parsedTaskCompletion{fingerprint: "sha256:changed"},
	)
	if !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion", err)
	}
}

func TestTaskCompletionReplayAfterAmbiguousError(t *testing.T) {
	operationErr := errors.New("commit result is unknown")
	completion := parsedTaskCompletion{fingerprint: "sha256:receipt"}
	if err := taskCompletionReplayAfterError(
		context.Background(),
		&taskCompletionReplayFixture{fingerprint: pgvalue.Text(completion.fingerprint)},
		workerActor{},
		api.WorkerCompleteTaskRequest{},
		completion,
		operationErr,
	); err != nil {
		t.Fatalf("confirmed replay returned %v", err)
	}
	if err := taskCompletionReplayAfterError(
		context.Background(),
		&taskCompletionReplayFixture{err: pgx.ErrNoRows},
		workerActor{},
		api.WorkerCompleteTaskRequest{},
		completion,
		operationErr,
	); !errors.Is(err, operationErr) {
		t.Fatalf("uncommitted error = %v, want original", err)
	}
}

func TestTaskCompletionDeadlineUsesLeaseAndActiveTime(t *testing.T) {
	now := time.Date(2026, time.July, 21, 3, 4, 5, 0, time.UTC)
	authority := runLeaseClaimAuthority{
		run: db.Run{
			MaxActiveDurationMs: 5_000,
			ActiveElapsedMs:     1_000,
			ActiveStartedAt:     pgvalue.Timestamptz(now.Add(-time.Second)),
		},
		runLease: db.RunLease{ExpiresAt: pgvalue.Timestamptz(now.Add(time.Second))},
		workspaceLease: db.WorkspaceLease{
			ExpiresAt: pgvalue.Timestamptz(now.Add(time.Second)),
		},
	}
	if err := validateTaskCompletionDeadline(authority, now); err != nil {
		t.Fatalf("valid completion rejected: %v", err)
	}
	if err := validateTaskCompletionDeadline(authority, now.Add(time.Second)); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("expiry boundary error = %v", err)
	}
	authority.runLease.ExpiresAt = pgvalue.Timestamptz(now.Add(10 * time.Second))
	authority.workspaceLease.ExpiresAt = authority.runLease.ExpiresAt
	hardDeadline := authority.run.ActiveStartedAt.Time.Add(4 * time.Second)
	if err := validateTaskCompletionDeadline(authority, hardDeadline); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("active deadline boundary error = %v", err)
	}
}

func TestTaskCompletionRejectsRollbackOutsideRunBase(t *testing.T) {
	runBase := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	authority := runLeaseClaimAuthority{
		run: db.Run{EntrypointKind: "task", BaseWorkspaceVersionID: runBase},
		attempt: db.RunAttempt{
			EntrypointEnteredAt:    pgvalue.Timestamptz(time.Now()),
			BaseWorkspaceVersionID: runBase,
		},
		runLease: db.RunLease{State: db.RunLeaseStateRunning},
		workspace: db.Workspace{
			HeadVersionID: runBase,
		},
	}
	completion := parsedTaskCompletion{
		kind:           taskCompletionFailed,
		rollbackBaseID: uuid.Must(uuid.NewV7()),
	}
	if err := validateTaskCompletionAuthority(
		context.Background(),
		nil,
		api.WorkerCompleteTaskRequest{},
		completion,
		authority,
	); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion", err)
	}
}

func TestTaskCompletionMountUpdateUsesLeaseFrontier(t *testing.T) {
	runBase := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	leaseBase := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	newVersion := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	authority := runLeaseClaimAuthority{
		run:            db.Run{BaseWorkspaceVersionID: runBase},
		workspaceLease: db.WorkspaceLease{BaseVersionID: leaseBase},
	}
	store := &taskWorkspaceMountFixture{}
	if err := updateTaskWorkspaceMountFrontier(
		context.Background(),
		store,
		authority,
		newVersion,
		pgvalue.Timestamptz(time.Now()),
	); err != nil {
		t.Fatal(err)
	}
	if store.params.BaseVersionID != leaseBase || store.params.NewVersionID != newVersion {
		t.Fatalf("mount update = %+v", store.params)
	}
}

type taskCompletionReplayFixture struct {
	fingerprint pgtype.Text
	err         error
	last        db.GetTaskCompletionReplayParams
}

type taskWorkspaceMountFixture struct {
	params db.UpdateTaskWorkspaceMountFrontierParams
}

func (f *taskWorkspaceMountFixture) UpdateTaskWorkspaceMountFrontier(
	_ context.Context,
	params db.UpdateTaskWorkspaceMountFrontierParams,
) (db.WorkspaceMount, error) {
	f.params = params
	return db.WorkspaceMount{}, nil
}

func (fixture *taskCompletionReplayFixture) GetTaskCompletionReplay(
	_ context.Context,
	params db.GetTaskCompletionReplayParams,
) (pgtype.Text, error) {
	fixture.last = params
	return fixture.fingerprint, fixture.err
}
