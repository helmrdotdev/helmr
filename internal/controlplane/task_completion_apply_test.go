package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTaskCompletionReplayUsesOnlyTerminalReceipt(t *testing.T) {
	workerID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	leaseID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	worker := workerActor{WorkerInstanceID: workerID, WorkerGroupID: controlplaneTestWorkerGroupID}
	request := workerapi.CompleteTaskRequest{Lease: workerapi.RunLeaseFence{
		ID:            leaseID.String(),
		LeaseSequence: 7,
	}}
	completion := parsedTaskCompletion{
		lease:       parsedRunLeaseFence{leaseID: leaseID},
		fingerprint: "sha256:receipt",
	}
	store := &taskCompletionReplayFixture{fingerprint: pgvalue.Text(completion.fingerprint)}

	replayed, err := taskCompletionWasReplayed(context.Background(), store, worker, request, completion)
	if err != nil || !replayed {
		t.Fatalf("replay = %t, %v", replayed, err)
	}
	if store.last.RunLeaseID != pgvalue.UUID(leaseID) ||
		store.last.LeaseSequence != 7 ||
		store.last.WorkerGroupID != pgvalue.UUID(worker.WorkerGroupID) ||
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
		workerapi.CompleteTaskRequest{},
		parsedTaskCompletion{fingerprint: "sha256:changed"},
	)
	if !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion", err)
	}
}

func TestTaskCompletionFailurePointPreservesStaleIdentity(t *testing.T) {
	err := staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointFence, errStaleTaskCompletion)
	if !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion identity", err)
	}
	point, ok := staleAuthorityPointOf(fmt.Errorf("outer: %w", err))
	if !ok || point != string(taskCompletionPointFence) {
		t.Fatalf("failure point = %q, %t", point, ok)
	}
	if got := staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointFinish, err); got != err {
		t.Fatal("outer failure point replaced the owning point")
	}
	plain := errors.New("storage unavailable")
	if got := staleAuthority(staleAuthorityTaskCompletion, taskCompletionPointFence, plain); got != plain {
		t.Fatal("non-stale error was wrapped")
	}
}

func TestTaskCompletionReplayAfterAmbiguousError(t *testing.T) {
	operationErr := errors.New("commit result is unknown")
	completion := parsedTaskCompletion{fingerprint: "sha256:receipt"}
	if err := taskCompletionReplayAfterError(
		context.Background(),
		&taskCompletionReplayFixture{fingerprint: pgvalue.Text(completion.fingerprint)},
		workerActor{},
		workerapi.CompleteTaskRequest{},
		completion,
		operationErr,
	); err != nil {
		t.Fatalf("confirmed replay returned %v", err)
	}
	if err := taskCompletionReplayAfterError(
		context.Background(),
		&taskCompletionReplayFixture{err: pgx.ErrNoRows},
		workerActor{},
		workerapi.CompleteTaskRequest{},
		completion,
		operationErr,
	); !errors.Is(err, operationErr) {
		t.Fatalf("uncommitted error = %v, want original", err)
	}
}

func TestTaskCompletionDeadlineUsesFrozenFinalizationExpiry(t *testing.T) {
	now := time.Date(2026, time.July, 21, 3, 4, 5, 0, time.UTC)
	authority := runLeaseClaimAuthority{
		run: db.Run{},
		runLease: db.RunLease{
			Status: db.RunLeaseStatusFinalizing, ExpiresAt: pgvalue.Timestamptz(now.Add(time.Second)),
			FinalizationStartedAt: pgvalue.Timestamptz(now.Add(-time.Second)),
		},
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
	authority.runLease.ExpiresAt = pgvalue.Timestamptz(now.Add(2 * time.Second))
	if err := validateTaskCompletionDeadline(authority, now); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("mismatched expiry error = %v", err)
	}
	authority.workspaceLease.ExpiresAt = authority.runLease.ExpiresAt
	authority.run.ActiveStartedAt = pgvalue.Timestamptz(now.Add(-time.Second))
	if err := validateTaskCompletionDeadline(authority, now); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("open active interval error = %v", err)
	}
}

func TestTaskCompletionRejectsRunningLease(t *testing.T) {
	if err := validateTaskCompletionAuthority(
		context.Background(),
		nil,
		parsedTaskCompletion{},
		runLeaseClaimAuthority{
			run:      db.Run{EntrypointKind: "task"},
			runLease: db.RunLease{Status: db.RunLeaseStatusRunning},
		},
	); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion", err)
	}
}

func TestTaskCompletionRejectsFinalizationKindMismatch(t *testing.T) {
	request := validTaskCompletionRequest(t)
	completion, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	baseID := pgvalue.UUID(uuid.MustParse(request.Workspace.Captured.Receipt.Fence.BaseWorkspaceVersionID))
	operationID := pgvalue.UUID(uuid.MustParse(completion.capture.receipt.OperationID))
	authority := runLeaseClaimAuthority{
		run: db.Run{
			EntrypointKind: "task", BaseWorkspaceVersionID: baseID,
		},
		attempt: db.RunAttempt{
			EntrypointKind: "task", EntrypointEnteredAt: pgvalue.Timestamptz(time.Now()),
			BaseWorkspaceVersionID: baseID,
		},
		runLease: db.RunLease{
			Status: db.RunLeaseStatusFinalizing, FinalizationOperationID: operationID,
			FinalizationKind:               pgvalue.Text("reset"),
			FinalizationStartedAt:          pgvalue.Timestamptz(time.Now()),
			FinalizationRequestFingerprint: pgvalue.Text("sha256:a798aa14ee85550172dc7b9e352e89bb54a1bdd6c44282ec539f1d6cfd323b6e"),
		},
		workspace: db.LockRunLeaseClaimWorkspaceRow{HeadVersionID: baseID},
	}
	if err := validateTaskCompletionAuthority(
		context.Background(), nil, completion, authority,
	); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion", err)
	}
}

func TestTaskCompletionMountUpdateUsesLeaseFrontier(t *testing.T) {
	runBase := pgvalue.UUID(uuid.NewV7())
	leaseBase := pgvalue.UUID(uuid.NewV7())
	newVersion := pgvalue.UUID(uuid.NewV7())
	authority := runLeaseClaimAuthority{
		run:            db.Run{BaseWorkspaceVersionID: runBase},
		workspaceLease: db.WorkspaceLease{BaseWorkspaceVersionID: leaseBase},
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
	if store.params.BaseWorkspaceVersionID != leaseBase || store.params.NewVersionID != newVersion {
		t.Fatalf("mount update = %+v", store.params)
	}
}

func TestRecordTaskWorkspaceVersionSeparatesTreeAndArtifactIdentity(t *testing.T) {
	authority := runLeaseClaimAuthority{
		run: db.Run{
			OrgID: pgvalue.UUID(uuid.NewV7()), ProjectID: pgvalue.UUID(uuid.NewV7()),
			EnvironmentID: pgvalue.UUID(uuid.NewV7()),
		},
		workspace: db.LockRunLeaseClaimWorkspaceRow{
			ID: pgvalue.UUID(uuid.NewV7()), OwnershipGeneration: 1, WriterGeneration: 2,
		},
		workspaceLease: db.WorkspaceLease{
			ID: pgvalue.UUID(uuid.NewV7()), BaseWorkspaceVersionID: pgvalue.UUID(uuid.NewV7()),
		},
		workspaceMount: db.WorkspaceMount{
			ID: pgvalue.UUID(uuid.NewV7()), RuntimeInstanceID: pgvalue.UUID(uuid.NewV7()),
			FencingGeneration: 3,
		},
		runtime: db.RuntimeInstance{ID: pgvalue.UUID(uuid.NewV7())},
	}
	versionID := pgvalue.UUID(uuid.NewV7())
	store := &taskWorkspaceVersionFixture{versionID: versionID}
	capture := parsedWorkspaceTreeCapture{
		tree: workspace.TreeIdentity{
			Digest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 12, EntryCount: 2,
		},
		artifact: workerapi.WorkspaceArtifact{
			Digest: "sha256:" + strings.Repeat("a", 64), MediaType: workspace.ArtifactMediaType,
			Encoding: workspace.ArtifactEncoding, SizeBytes: 1024, EntryCount: 2,
		},
	}
	got, err := recordTaskWorkspaceVersion(
		context.Background(), store, workerActor{WorkerInstanceID: uuid.NewV7()},
		authority, capture.version(), pgvalue.Timestamptz(time.Now()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != versionID || store.publish.ContentDigest.String != capture.tree.Digest ||
		store.publish.SizeBytes != capture.tree.SizeBytes || store.publish.EntryCount != int32(capture.tree.EntryCount) ||
		store.artifact.Digest != capture.artifact.Digest || store.artifact.SizeBytes != capture.artifact.SizeBytes {
		t.Fatalf("published version = %+v, Artifact = %+v", store.publish, store.artifact)
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

type taskWorkspaceVersionFixture struct {
	versionID pgtype.UUID
	artifact  db.CreateArtifactParams
	publish   db.PublishTaskWorkspaceVersionParams
}

func (f *taskWorkspaceVersionFixture) UpsertCasObject(
	_ context.Context,
	_ db.UpsertCasObjectParams,
) (db.CasObject, error) {
	return db.CasObject{}, nil
}

func (f *taskWorkspaceVersionFixture) CreateArtifact(
	_ context.Context,
	params db.CreateArtifactParams,
) (db.Artifact, error) {
	f.artifact = params
	return db.Artifact{ID: params.ID}, nil
}

func (f *taskWorkspaceVersionFixture) PublishTaskWorkspaceVersion(
	_ context.Context,
	params db.PublishTaskWorkspaceVersionParams,
) (db.WorkspaceVersion, error) {
	f.publish = params
	return db.WorkspaceVersion{ID: f.versionID}, nil
}

func (f *taskWorkspaceVersionFixture) UpdateTaskWorkspaceMountFrontier(
	_ context.Context,
	_ db.UpdateTaskWorkspaceMountFrontierParams,
) (db.WorkspaceMount, error) {
	return db.WorkspaceMount{}, nil
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
