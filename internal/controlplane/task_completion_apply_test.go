package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	worker := workerActor{WorkerInstanceID: workerID, WorkerGroupID: "workers"}
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
		workerapi.CompleteTaskRequest{},
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
			State: db.RunLeaseStateFinalizing, ExpiresAt: pgvalue.Timestamptz(now.Add(time.Second)),
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

func TestTaskCompletionRejectsRollbackOutsideRunBase(t *testing.T) {
	runBase := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	authority := runLeaseClaimAuthority{
		run: db.Run{EntrypointKind: "task", BaseWorkspaceVersionID: runBase},
		attempt: db.RunAttempt{
			EntrypointEnteredAt:    pgvalue.Timestamptz(time.Now()),
			BaseWorkspaceVersionID: runBase,
		},
		runLease: db.RunLease{State: db.RunLeaseStateFinalizing},
		workspace: db.Workspace{
			HeadVersionID: runBase,
		},
	}
	completion := parsedTaskCompletion{
		kind: taskCompletionFailed,
		rollback: &parsedTaskWorkspaceRollback{
			baseID: uuid.Must(uuid.NewV7()),
		},
	}
	if err := validateTaskCompletionAuthority(
		context.Background(),
		nil,
		workerapi.CompleteTaskRequest{},
		completion,
		authority,
	); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion", err)
	}
}

func TestTaskCompletionRejectsRunningLease(t *testing.T) {
	if err := validateTaskCompletionAuthority(
		context.Background(),
		nil,
		workerapi.CompleteTaskRequest{},
		parsedTaskCompletion{},
		runLeaseClaimAuthority{
			run:      db.Run{EntrypointKind: "task"},
			runLease: db.RunLease{State: db.RunLeaseStateRunning},
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
			State: db.RunLeaseStateFinalizing, FinalizationOperationID: operationID,
			FinalizationKind:               pgvalue.Text(string(workerapi.RunFinalizationReset)),
			FinalizationStartedAt:          pgvalue.Timestamptz(time.Now()),
			FinalizationRequestFingerprint: pgvalue.Text("sha256:frozen"),
		},
		workspace: db.Workspace{HeadVersionID: baseID},
	}
	if err := validateTaskCompletionAuthority(
		context.Background(), nil, request, completion, authority,
	); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("error = %v, want stale completion", err)
	}
}

func TestTaskCompletionRejectsForgedHandoffAuthority(t *testing.T) {
	id := func() pgtype.UUID { return pgvalue.UUID(uuid.Must(uuid.NewV7())) }
	parentID, childID, workspaceID, waitID, checkpointID := id(), id(), id(), id(), id()
	baseID, operationID := id(), id()
	authority := runLeaseClaimAuthority{
		run: db.Run{
			ID: childID, EntrypointKind: "task", ParentRunID: parentID,
			ParentOwnsLifecycle: pgtype.Bool{Bool: true, Valid: true},
			WorkspaceID:         workspaceID, BaseWorkspaceVersionID: baseID,
		},
		parentRun: db.Run{ID: parentID, WorkspaceID: workspaceID},
		attempt: db.RunAttempt{
			EntrypointKind: "task", EntrypointEnteredAt: pgvalue.Timestamptz(time.Now()),
			BaseWorkspaceVersionID: baseID,
		},
		parentAttempt: db.RunAttempt{Number: 3},
		runLease: db.RunLease{
			State: db.RunLeaseStateFinalizing, FinalizationOperationID: operationID,
			FinalizationKind:               pgvalue.Text(string(workerapi.RunFinalizationCapture)),
			FinalizationStartedAt:          pgvalue.Timestamptz(time.Now()),
			FinalizationRequestFingerprint: pgvalue.Text("sha256:frozen"),
		},
		enclosingWait: db.RunWait{ID: waitID},
		checkpoint: db.RunCheckpoint{
			ID: checkpointID, RunID: parentID, AttemptNumber: 3, RunWaitID: waitID,
			RestoreManifest: testCheckpointManifest(t, checkpointID, parentID, 3, waitID),
		},
	}
	completion := parsedTaskCompletion{
		kind: taskCompletionSucceeded,
		capture: &parsedTaskWorkspaceCapture{receipt: workspace.FinalizationRequest{
			OperationID: pgvalue.UUIDString(operationID),
		}},
		handoff: &parsedTaskHandoffCheckpoint{
			checkpointID: uuid.Must(uuid.NewV7()), parentRunID: pgvalue.MustUUIDValue(parentID),
			waitID: pgvalue.MustUUIDValue(waitID), attemptNumber: 3,
		},
	}
	request := workerapi.CompleteTaskRequest{Handoff: &workerapi.TaskHandoffCheckpoint{
		Manifest: workerapi.CheckpointManifest{RecoveryPoint: workerapi.CheckpointRecoveryPoint{
			CorrelationID: "correlation-1",
		}},
	}}
	tests := []struct {
		name   string
		mutate func(*workerapi.CompleteTaskRequest, *parsedTaskCompletion)
	}{
		{name: "parent run", mutate: func(_ *workerapi.CompleteTaskRequest, completion *parsedTaskCompletion) {
			completion.handoff.parentRunID = uuid.Must(uuid.NewV7())
		}},
		{name: "parent attempt", mutate: func(_ *workerapi.CompleteTaskRequest, completion *parsedTaskCompletion) {
			completion.handoff.attemptNumber++
		}},
		{name: "wait", mutate: func(_ *workerapi.CompleteTaskRequest, completion *parsedTaskCompletion) {
			completion.handoff.waitID = uuid.Must(uuid.NewV7())
		}},
		{name: "correlation", mutate: func(request *workerapi.CompleteTaskRequest, _ *parsedTaskCompletion) {
			request.Handoff.Manifest.RecoveryPoint.CorrelationID = "forged"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changedRequest := request
			changedRequest.Handoff = &workerapi.TaskHandoffCheckpoint{
				Manifest: request.Handoff.Manifest,
			}
			changedCompletion := completion
			changedHandoff := *completion.handoff
			changedCompletion.handoff = &changedHandoff
			test.mutate(&changedRequest, &changedCompletion)
			if err := validateTaskCompletionAuthority(
				context.Background(), nil, changedRequest, changedCompletion, authority,
			); !errors.Is(err, errStaleTaskCompletion) {
				t.Fatalf("error = %v, want stale completion", err)
			}
		})
	}
}

func TestSameWorkspaceChildRejectsCheckpointReuse(t *testing.T) {
	checkpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	err := finishSameWorkspaceChild(
		context.Background(),
		nil,
		workerActor{},
		runLeaseClaimAuthority{enclosingWait: db.RunWait{SuspendCheckpointID: checkpointID}},
		parsedTaskCompletion{
			kind: taskCompletionSucceeded,
			handoff: &parsedTaskHandoffCheckpoint{
				checkpointID: pgvalue.MustUUIDValue(checkpointID),
			},
		},
		pgvalue.Timestamptz(time.Now()),
		pgvalue.UUID(uuid.Must(uuid.NewV7())),
	)
	if !errors.Is(err, errStaleTaskCompletion) {
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

func TestCascadeSameWorkspaceChildFailureUnwindsToAttachmentOwner(t *testing.T) {
	id := func() pgtype.UUID { return pgvalue.UUID(uuid.Must(uuid.NewV7())) }
	environmentID := id()
	workspaceID := id()
	runtimeID := id()
	mountID := id()
	now := pgvalue.Timestamptz(time.Now())
	runA, runB, runC, runD := id(), id(), id(), id()
	waitAB, waitBC, waitCD := id(), id(), id()
	handoffWait := func(waitID, parentID, childID pgtype.UUID, attempt int32) db.RunWait {
		return db.RunWait{
			ID: waitID, EnvironmentID: environmentID, RunID: parentID,
			WorkspaceID: workspaceID, Kind: db.WaitKindChild,
			ConditionState: db.WaitStatePending, SuspensionState: db.RunWaitStateParked,
			AttemptNumber: attempt, ChildRunID: childID,
			ChildParentOwned: pgtype.Bool{Bool: true, Valid: true},
			PriorRunLeaseID:  id(), SuspendCheckpointID: id(),
			HandoffRuntimeInstanceID: runtimeID, HandoffWorkspaceMountID: mountID,
			HandoffMountGeneration:  pgtype.Int8{Int64: 2, Valid: true},
			OwnershipGeneration:     pgtype.Int8{Int64: 1, Valid: true},
			ChildWriterGeneration:   pgtype.Int8{Int64: int64(attempt + 1), Valid: true},
			ExpectedRunStateVersion: 7,
		}
	}
	authority := runLeaseClaimAuthority{
		workspace: db.Workspace{ID: workspaceID},
		run:       db.Run{ID: runD},
		parentRun: db.Run{
			ID: runC, OrgID: id(), EnvironmentID: environmentID,
			WorkspaceID: workspaceID, Status: db.RunStatusWaiting,
			CurrentAttemptNumber: 3,
		},
		parentAttempt: db.RunAttempt{
			RunID: runC, Number: 3, EntrypointKind: "task", WorkspaceID: workspaceID,
		},
		enclosingWait: handoffWait(waitCD, runC, runD, 3),
		handoffAncestors: []db.LockSameWorkspaceHandoffAncestorsRow{
			{
				RunWait: handoffWait(waitAB, runA, runB, 1),
				Run: db.Run{
					ID: runA, OrgID: id(), EnvironmentID: environmentID,
					WorkspaceID: workspaceID, Status: db.RunStatusWaiting,
					CurrentAttemptNumber: 1,
				},
				RunAttempt: db.RunAttempt{
					RunID: runA, Number: 1, EntrypointKind: "task", WorkspaceID: workspaceID,
				},
				Depth: 1,
			},
			{
				RunWait: handoffWait(waitBC, runB, runC, 2),
				Run: db.Run{
					ID: runB, OrgID: id(), EnvironmentID: environmentID,
					WorkspaceID: workspaceID, Status: db.RunStatusWaiting,
					CurrentAttemptNumber: 2,
				},
				RunAttempt: db.RunAttempt{
					RunID: runB, Number: 2, EntrypointKind: "task", WorkspaceID: workspaceID,
				},
				Depth: 0,
			},
		},
	}
	store := &sameWorkspaceFailureCascadeStore{
		completed: authority.handoffAncestors[0].RunWait,
	}

	completed, err := cascadeSameWorkspaceChildFailure(
		context.Background(),
		store,
		authority,
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.ID != waitAB {
		t.Fatalf("completed Wait = %s, want outer %s", pgvalue.UUIDString(completed.ID), pgvalue.UUIDString(waitAB))
	}
	want := []string{
		"wait:" + pgvalue.UUIDString(waitCD),
		"attempt:" + pgvalue.UUIDString(runC),
		"run:" + pgvalue.UUIDString(runC),
		"event:" + pgvalue.UUIDString(runC),
		"wait:" + pgvalue.UUIDString(waitBC),
		"attempt:" + pgvalue.UUIDString(runB),
		"run:" + pgvalue.UUIDString(runB),
		"event:" + pgvalue.UUIDString(runB),
		"resume:" + pgvalue.UUIDString(waitAB),
	}
	if strings.Join(store.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("cascade calls = %v, want %v", store.calls, want)
	}
}

func TestTaskWorkspaceRollbackMatchesCanonicalRootVersion(t *testing.T) {
	baseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	authority := runLeaseClaimAuthority{
		run:       db.Run{BaseWorkspaceVersionID: baseID},
		workspace: db.Workspace{ID: workspaceID},
	}
	store := &taskWorkspaceRollbackFixture{version: db.WorkspaceVersion{
		ID: baseID, WorkspaceID: workspaceID, Kind: db.WorkspaceVersionKindSystem,
		ContentDigest: workspace.CanonicalEmptyTreeDigest, State: db.WorkspaceVersionStateCommitted,
	}}
	rollback := parsedTaskWorkspaceRollback{
		baseID: pgvalue.MustUUIDValue(baseID),
		target: workspace.ResetTarget{
			Kind: workspace.ResetTargetEmpty, BaseVersionID: pgvalue.UUIDString(baseID),
			Tree: workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
		},
	}
	if err := validateTaskWorkspaceRollback(context.Background(), store, authority, rollback); err != nil {
		t.Fatal(err)
	}
	store.version.ArtifactID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if err := validateTaskWorkspaceRollback(context.Background(), store, authority, rollback); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("root with Artifact error = %v", err)
	}
}

func TestTaskWorkspaceRollbackMatchesVersionArtifact(t *testing.T) {
	baseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	workspaceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	artifactID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	parentID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	sourceLeaseID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	tree := workspace.TreeIdentity{Digest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 12, EntryCount: 2}
	artifact := workspace.ArtifactIdentity{
		Digest: "sha256:" + strings.Repeat("a", 64), MediaType: workspace.ArtifactMediaType,
		Encoding: workspace.ArtifactEncoding, SizeBytes: 1024, EntryCount: tree.EntryCount,
	}
	authority := runLeaseClaimAuthority{
		run:       db.Run{BaseWorkspaceVersionID: baseID},
		workspace: db.Workspace{ID: workspaceID},
	}
	store := &taskWorkspaceRollbackFixture{
		version: db.WorkspaceVersion{
			ID: baseID, WorkspaceID: workspaceID, ParentVersionID: parentID, ArtifactID: artifactID,
			ArtifactKind: db.NullArtifactKind{ArtifactKind: db.ArtifactKindWorkspaceVersion, Valid: true},
			Kind:         db.WorkspaceVersionKindUser, ContentDigest: tree.Digest, SizeBytes: tree.SizeBytes,
			EntryCount: int32(tree.EntryCount), State: db.WorkspaceVersionStateCommitted,
			SourceWorkspaceLeaseID: sourceLeaseID,
		},
		artifact: db.Artifact{
			ID: artifactID, Digest: artifact.Digest, Kind: db.ArtifactKindWorkspaceVersion,
			SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType,
		},
	}
	rollback := parsedTaskWorkspaceRollback{
		baseID: pgvalue.MustUUIDValue(baseID),
		target: workspace.ResetTarget{
			Kind: workspace.ResetTargetArtifact, BaseVersionID: pgvalue.UUIDString(baseID),
			Tree: tree, Artifact: &artifact,
		},
	}
	if err := validateTaskWorkspaceRollback(context.Background(), store, authority, rollback); err != nil {
		t.Fatal(err)
	}
	store.artifact.Digest = "sha256:" + strings.Repeat("c", 64)
	if err := validateTaskWorkspaceRollback(context.Background(), store, authority, rollback); !errors.Is(err, errStaleTaskCompletion) {
		t.Fatalf("retargeted Artifact error = %v", err)
	}
}

func TestRecordTaskWorkspaceVersionSeparatesTreeAndArtifactIdentity(t *testing.T) {
	authority := runLeaseClaimAuthority{
		run: db.Run{
			OrgID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ProjectID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
			EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		},
		workspace: db.Workspace{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OwnershipGeneration: 1, WriterGeneration: 2,
		},
		workspaceLease: db.WorkspaceLease{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), BaseVersionID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		},
		workspaceMount: db.WorkspaceMount{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), RuntimeInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
			FencingGeneration: 3,
		},
		runtime: db.RuntimeInstance{ID: pgvalue.UUID(uuid.Must(uuid.NewV7()))},
	}
	versionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	store := &taskWorkspaceVersionFixture{versionID: versionID}
	capture := parsedTaskWorkspaceCapture{
		tree: workspace.TreeIdentity{
			Digest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 12, EntryCount: 2,
		},
		artifact: workerapi.WorkspaceArtifact{
			Digest: "sha256:" + strings.Repeat("a", 64), MediaType: workspace.ArtifactMediaType,
			Encoding: workspace.ArtifactEncoding, SizeBytes: 1024, EntryCount: 2,
		},
	}
	got, err := recordTaskWorkspaceVersion(
		context.Background(), store, workerActor{WorkerInstanceID: uuid.Must(uuid.NewV7())},
		authority, capture, pgvalue.Timestamptz(time.Now()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got != versionID || store.publish.ContentDigest != capture.tree.Digest ||
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

type sameWorkspaceFailureCascadeStore struct {
	db.Querier
	completed db.RunWait
	calls     []string
}

func (s *sameWorkspaceFailureCascadeStore) FailNestedSameWorkspaceWait(
	_ context.Context,
	params db.FailNestedSameWorkspaceWaitParams,
) (db.RunWait, error) {
	s.calls = append(s.calls, "wait:"+pgvalue.UUIDString(params.RunWaitID))
	return db.RunWait{ID: params.RunWaitID}, nil
}

func (s *sameWorkspaceFailureCascadeStore) FailNestedSameWorkspaceAttempt(
	_ context.Context,
	params db.FailNestedSameWorkspaceAttemptParams,
) (db.RunAttempt, error) {
	s.calls = append(s.calls, "attempt:"+pgvalue.UUIDString(params.RunID))
	return db.RunAttempt{RunID: params.RunID}, nil
}

func (s *sameWorkspaceFailureCascadeStore) FailNestedSameWorkspaceRun(
	_ context.Context,
	params db.FailNestedSameWorkspaceRunParams,
) (db.Run, error) {
	s.calls = append(s.calls, "run:"+pgvalue.UUIDString(params.RunID))
	return db.Run{ID: params.RunID}, nil
}

func (s *sameWorkspaceFailureCascadeStore) AppendRunEvent(
	_ context.Context,
	params db.AppendRunEventParams,
) (db.AppendRunEventRow, error) {
	s.calls = append(s.calls, "event:"+pgvalue.UUIDString(params.RunID))
	return db.AppendRunEventRow{}, nil
}

func (s *sameWorkspaceFailureCascadeStore) CompleteSameWorkspaceChildFailure(
	_ context.Context,
	params db.CompleteSameWorkspaceChildFailureParams,
) (db.RunWait, error) {
	s.calls = append(s.calls, "resume:"+pgvalue.UUIDString(params.RunWaitID))
	return s.completed, nil
}

type taskWorkspaceMountFixture struct {
	params db.UpdateTaskWorkspaceMountFrontierParams
}

type taskWorkspaceRollbackFixture struct {
	version  db.WorkspaceVersion
	artifact db.Artifact
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

func (f *taskWorkspaceRollbackFixture) GetTaskWorkspaceResetVersion(
	_ context.Context,
	_ db.GetTaskWorkspaceResetVersionParams,
) (db.WorkspaceVersion, error) {
	return f.version, nil
}

func (f *taskWorkspaceRollbackFixture) GetArtifact(
	_ context.Context,
	_ db.GetArtifactParams,
) (db.Artifact, error) {
	return f.artifact, nil
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
