package control

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type runResumeReleaseStore struct {
	*runLeaseClaimStore
	locators       db.GetRunLeaseStartLocatorsRow
	releaseWrites  int
	checkpointLock *db.LockReadyRunCheckpointParams
}

func (s *runResumeReleaseStore) LockReadyRunCheckpoint(
	_ context.Context, params db.LockReadyRunCheckpointParams,
) (db.RunCheckpoint, error) {
	s.calls = append(s.calls, "checkpoint")
	s.checkpointLock = &params
	return s.authority.checkpoint, nil
}

func (s *runResumeReleaseStore) BeginQuerier(context.Context) (db.Querier, controlTransaction, error) {
	return s, runResumeReleaseTransaction{s}, nil
}

func (s *runResumeReleaseStore) GetRunLeaseStartLocators(
	context.Context, db.GetRunLeaseStartLocatorsParams,
) (db.GetRunLeaseStartLocatorsRow, error) {
	s.calls = append(s.calls, "start_locators")
	return s.locators, nil
}

func (s *runResumeReleaseStore) LockRunStartLease(
	context.Context, db.LockRunStartLeaseParams,
) (db.RunLease, error) {
	s.calls = append(s.calls, "run_lease")
	return s.authority.runLease, nil
}

func (s *runResumeReleaseStore) LockRunStartWait(
	context.Context, db.LockRunStartWaitParams,
) (db.RunWait, error) {
	s.calls = append(s.calls, "run_wait")
	return s.authority.runWait, nil
}

func (s *runResumeReleaseStore) ReleaseRunResumeWait(
	_ context.Context, params db.ReleaseRunResumeWaitParams,
) (db.RunWait, error) {
	s.calls = append(s.calls, "release_wait")
	wait := s.authority.runWait
	if wait.SuspensionState != db.RunWaitStateResuming ||
		wait.ResumeAckVersion >= wait.ResumeRequestVersion ||
		wait.ID != params.ID || wait.CurrentRunLeaseID != params.CurrentRunLeaseID ||
		resumeReleaseCheckpointID(wait) != params.CheckpointID ||
		wait.ResumeAttachID != params.ResumeAttachID ||
		wait.ResumeRequestVersion != params.ResumeRequestVersion {
		return db.RunWait{}, pgx.ErrNoRows
	}
	s.releaseWrites++
	wait.SuspensionState = db.RunWaitStateReleased
	wait.ResumeAckVersion = params.ResumeRequestVersion
	wait.SuspensionTerminalAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	s.authority.runWait = wait
	return wait, nil
}

func resumeReleaseCheckpointID(wait db.RunWait) pgtype.UUID {
	if wait.ConditionState == db.WaitStateCompleted && wait.HandoffResumeCheckpointID.Valid {
		return wait.HandoffResumeCheckpointID
	}
	return wait.SuspendCheckpointID
}

type runResumeReleaseTransaction struct{ store *runResumeReleaseStore }

func (tx runResumeReleaseTransaction) Commit(context.Context) error {
	tx.store.calls = append(tx.store.calls, "commit")
	return nil
}

func (tx runResumeReleaseTransaction) Rollback(context.Context) error {
	tx.store.calls = append(tx.store.calls, "rollback")
	return nil
}

func TestAcknowledgeRunResumeReleaseCommitsOnceAndReplaysWithoutWrite(t *testing.T) {
	server, store, worker, expected, proof := validRunResumeReleaseFixture(t)

	receipt, err := server.acknowledgeRunResumeRelease(
		context.Background(), worker, store.authority.runLease.ID, expected, proof,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !equalRunLeaseReceipt(receipt, expected) {
		t.Fatalf("receipt = %+v, want %+v", receipt, expected)
	}
	if store.releaseWrites != 1 ||
		store.authority.runWait.SuspensionState != db.RunWaitStateReleased ||
		store.authority.runWait.ResumeAckVersion != proof.resumeRequestVersion ||
		!store.authority.runWait.SuspensionTerminalAt.Valid {
		t.Fatalf("released wait = %+v, writes = %d", store.authority.runWait, store.releaseWrites)
	}

	if _, err := server.acknowledgeRunResumeRelease(
		context.Background(), worker, store.authority.runLease.ID, expected, proof,
	); err != nil {
		t.Fatalf("exact replay failed: %v", err)
	}
	if store.releaseWrites != 1 {
		t.Fatalf("exact replay performed %d release writes, want 1", store.releaseWrites)
	}
	checkpointLocks := 0
	for _, call := range store.calls {
		if call == "checkpoint" {
			checkpointLocks++
		}
	}
	if checkpointLocks != 1 {
		t.Fatalf("exact replay acquired ready Checkpoint %d times, want first commit only", checkpointLocks)
	}
}

func TestAcknowledgeRunResumeReleaseAcceptsParentAttachHandoffCheckpoint(t *testing.T) {
	server, store, worker, expected, proof := validParentAttachRunResumeReleaseFixture(t)
	if _, err := server.acknowledgeRunResumeRelease(
		context.Background(), worker, store.authority.runLease.ID, expected, proof,
	); err != nil {
		t.Fatal(err)
	}
	if store.releaseWrites != 1 || store.checkpointLock == nil ||
		store.checkpointLock.ID != proof.checkpointID ||
		store.checkpointLock.Kind != db.RunCheckpointKindHandoffResume {
		t.Fatalf("parent release writes=%d checkpoint=%+v", store.releaseWrites, store.checkpointLock)
	}
}

func TestAcknowledgeRunResumeReleaseAllowsRunningLeaseWhileGroupDrains(t *testing.T) {
	server, store, worker, expected, proof := validRunResumeReleaseFixture(t)
	store.authority.workerGroup.State = db.WorkerGroupStateDraining
	if _, err := server.acknowledgeRunResumeRelease(
		context.Background(), worker, store.authority.runLease.ID, expected, proof,
	); err != nil {
		t.Fatal(err)
	}
}

func TestAcknowledgeRunResumeReleaseRejectsMismatchedRestoreIdentity(t *testing.T) {
	server, store, worker, expected, proof := validRunResumeReleaseFixture(t)
	proof.resumeAttachID = pgtype.UUID{Bytes: [16]byte{1}, Valid: true}

	_, err := server.acknowledgeRunResumeRelease(
		context.Background(), worker, store.authority.runLease.ID, expected, proof,
	)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale lease conflict", err)
	}
	if store.releaseWrites != 0 {
		t.Fatalf("mismatched proof performed %d writes", store.releaseWrites)
	}
}

func TestParseRunResumeReleaseProofRequiresCanonicalUUIDv7(t *testing.T) {
	valid := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	base := api.WorkerRunResumeReleaseRequest{
		RunWaitID: valid, CheckpointID: valid, ResumeAttachID: valid,
		ResumeRequestVersion: 1,
	}
	for _, test := range []struct {
		name string
		id   string
	}{
		{name: "uuidv4", id: "8fa3431e-c649-4ea0-bf12-b8e9fcdf1d8d"},
		{name: "uppercase", id: "019C10D5-A6F7-7AF1-8F5F-BB97BCC0DC31"},
		{name: "whitespace", id: " " + valid},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := base
			request.RunWaitID = test.id
			_, err := parseRunResumeReleaseProof(request)
			if err == nil || !strings.Contains(err.Error(), "canonical UUIDv7") {
				t.Fatalf("error = %v, want canonical UUIDv7 rejection", err)
			}
		})
	}
}

func validRunResumeReleaseFixture(
	t *testing.T,
) (*Server, *runResumeReleaseStore, workerActor, api.WorkerRunLeaseReceipt, runResumeReleaseProof) {
	t.Helper()
	worker, claimLocators, authority := validCheckpointRestoreRunLeaseClaimFixture(false)
	authority.run.Status = db.RunStatusRunning
	authority.run.MaxActiveDurationMs = 60_000
	authority.runLease.State = db.RunLeaseStateRunning
	authority.runLease.StartDeadlineAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
	authority.runLease.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}
	authority.workspaceMount.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.WorkspaceMountID = authority.workspaceMount.ID
	authority.workspaceLease.WorkspaceID = authority.workspace.ID

	locators := db.GetRunLeaseStartLocatorsRow{
		OrgID: claimLocators.OrgID, ProjectID: claimLocators.ProjectID,
		EnvironmentID: claimLocators.EnvironmentID, RunID: claimLocators.RunID,
		WorkspaceID: claimLocators.WorkspaceID, AttemptNumber: claimLocators.AttemptNumber,
		RegionID: claimLocators.RegionID, RuntimeInstanceID: claimLocators.RuntimeInstanceID,
		NetworkSlotID: claimLocators.NetworkSlotID, NetworkSlotGeneration: claimLocators.NetworkSlotGeneration,
		WorkspaceLeaseID: claimLocators.WorkspaceLeaseID, WorkspaceMountID: claimLocators.WorkspaceMountID,
		RunWaitID: authority.runWait.ID, RunWaitCheckpointID: authority.runWait.SuspendCheckpointID,
		ResumeAttachID:       authority.runWait.ResumeAttachID,
		ResumeRequestVersion: pgtype.Int8{Int64: authority.runWait.ResumeRequestVersion, Valid: true},
	}
	store := &runResumeReleaseStore{
		runLeaseClaimStore: &runLeaseClaimStore{authority: authority},
		locators:           locators,
	}
	expected, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		networkSlot: authority.networkSlot, runLease: authority.runLease,
		workspace: authority.workspace, workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := runResumeReleaseProof{
		runWaitID: authority.runWait.ID, checkpointID: authority.runWait.SuspendCheckpointID,
		resumeAttachID:       authority.runWait.ResumeAttachID,
		resumeRequestVersion: authority.runWait.ResumeRequestVersion,
	}
	return &Server{db: store}, store, worker, expected, proof
}

func validParentAttachRunResumeReleaseFixture(
	t *testing.T,
) (*Server, *runResumeReleaseStore, workerActor, api.WorkerRunLeaseReceipt, runResumeReleaseProof) {
	t.Helper()
	worker, claimLocators, authority := validSameWorkspaceParentResumeRunLeaseClaimFixture(false, true)
	authority.run.Status = db.RunStatusRunning
	authority.run.MaxActiveDurationMs = 60_000
	authority.runLease.State = db.RunLeaseStateRunning
	authority.runLease.StartDeadlineAt = pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true}
	authority.runLease.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true}
	authority.runtime.RestoreCheckpointID = pgtype.UUID{}
	authority.workspaceMount.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.WorkspaceMountID = authority.workspaceMount.ID
	authority.workspaceLease.WorkspaceID = authority.workspace.ID

	locators := db.GetRunLeaseStartLocatorsRow{
		OrgID: claimLocators.OrgID, ProjectID: claimLocators.ProjectID,
		EnvironmentID: claimLocators.EnvironmentID, RunID: claimLocators.RunID,
		WorkspaceID: claimLocators.WorkspaceID, AttemptNumber: claimLocators.AttemptNumber,
		RegionID: claimLocators.RegionID, RuntimeInstanceID: claimLocators.RuntimeInstanceID,
		NetworkSlotID: claimLocators.NetworkSlotID, NetworkSlotGeneration: claimLocators.NetworkSlotGeneration,
		WorkspaceLeaseID: claimLocators.WorkspaceLeaseID, WorkspaceMountID: claimLocators.WorkspaceMountID,
		RunWaitID: authority.runWait.ID, RunWaitCheckpointID: authority.runWait.HandoffResumeCheckpointID,
		ResumeAttachID:         authority.runWait.ResumeAttachID,
		ResumeRequestVersion:   pgtype.Int8{Int64: authority.runWait.ResumeRequestVersion, Valid: true},
		ResumeChildRunID:       authority.runWait.ChildRunID,
		ResumeChildParentOwned: authority.runWait.ChildParentOwned,
	}
	store := &runResumeReleaseStore{
		runLeaseClaimStore: &runLeaseClaimStore{authority: authority},
		locators:           locators,
	}
	expected, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
		run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
		networkSlot: authority.networkSlot, runLease: authority.runLease,
		workspace: authority.workspace, workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := runResumeReleaseProof{
		runWaitID: authority.runWait.ID, checkpointID: authority.runWait.HandoffResumeCheckpointID,
		resumeAttachID:       authority.runWait.ResumeAttachID,
		resumeRequestVersion: authority.runWait.ResumeRequestVersion,
	}
	return &Server{db: store}, store, worker, expected, proof
}
