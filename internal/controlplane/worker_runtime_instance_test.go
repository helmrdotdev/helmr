package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	runauthority "github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type runtimeRestoreProjectionStore struct {
	db.Querier
	checkpoint db.RunCheckpoint
	artifacts  []db.ListRunCheckpointArtifactAuthorityRow
	base       db.GetCheckpointWorkspaceBaseAuthorityRow
	baseCalls  int
}

func (s *runtimeRestoreProjectionStore) GetReadyRunCheckpoint(
	context.Context,
	db.GetReadyRunCheckpointParams,
) (db.RunCheckpoint, error) {
	return s.checkpoint, nil
}

func (s *runtimeRestoreProjectionStore) ListRunCheckpointArtifactAuthority(
	context.Context,
	pgtype.UUID,
) ([]db.ListRunCheckpointArtifactAuthorityRow, error) {
	return s.artifacts, nil
}

func (s *runtimeRestoreProjectionStore) GetCheckpointWorkspaceBaseAuthority(
	_ context.Context,
	params db.GetCheckpointWorkspaceBaseAuthorityParams,
) (db.GetCheckpointWorkspaceBaseAuthorityRow, error) {
	s.baseCalls++
	if params.VersionID != s.checkpoint.BaseWorkspaceVersionID {
		return db.GetCheckpointWorkspaceBaseAuthorityRow{}, errors.New("unexpected source base version")
	}
	return s.base, nil
}

func TestPopulateRuntimeRestoreSourceKeepsCapturedAndSourceFrontiersDistinct(t *testing.T) {
	checkpointID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	waitID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	sourceVersionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	capturedVersionID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	manifest, err := json.Marshal(workerapi.CheckpointManifest{
		WorkspaceState: workerapi.CheckpointWorkspaceState{Base: workerapi.CheckpointWorkspaceBase{
			ArtifactDigest: validDigest('e'), ArtifactSizeBytes: 512,
			ArtifactMediaType: workspace.ArtifactMediaType,
			ArtifactEncoding:  workspace.ArtifactEncoding,
			MountPath:         "/workspace",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &runtimeRestoreProjectionStore{
		checkpoint: db.RunCheckpoint{
			ID: checkpointID, RunID: runID, RunWaitID: waitID, AttemptNumber: 2,
			State:                  db.RunCheckpointStateReady,
			BaseWorkspaceVersionID: sourceVersionID, RestoreManifest: manifest,
		},
		artifacts: []db.ListRunCheckpointArtifactAuthorityRow{
			{Role: db.RunCheckpointArtifactRoleRuntimeConfig, Digest: validDigest('a'), SizeBytes: 1, MediaType: "application/example"},
			{Role: db.RunCheckpointArtifactRoleVMState, Digest: validDigest('b'), SizeBytes: 2, MediaType: "application/example"},
			{Role: db.RunCheckpointArtifactRoleMemory, Digest: validDigest('c'), SizeBytes: 3, MediaType: "application/example"},
			{Role: db.RunCheckpointArtifactRoleScratchDisk, Digest: validDigest('d'), SizeBytes: 4, MediaType: "application/example"},
		},
		base: db.GetCheckpointWorkspaceBaseAuthorityRow{
			VersionID:       sourceVersionID,
			ParentVersionID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
			ArtifactID:      pgvalue.UUID(uuid.Must(uuid.NewV7())),
			ArtifactKind:    db.NullArtifactKind{ArtifactKind: db.ArtifactKindWorkspaceVersion, Valid: true},
			VersionKind:     db.WorkspaceVersionKindUser,
			ContentDigest:   validDigest('f'), LogicalSizeBytes: 64, EntryCount: 1,
			SourceWorkspaceLeaseID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OwnershipGeneration:    2, WriterGeneration: 3,
			ArtifactRowKind:   db.NullArtifactKind{ArtifactKind: db.ArtifactKindWorkspaceVersion, Valid: true},
			ArtifactDigest:    pgvalue.Text(validDigest('e')),
			ArtifactSizeBytes: pgtype.Int8{Int64: 512, Valid: true},
			ArtifactMediaType: pgvalue.Text(workspace.ArtifactMediaType),
		},
	}
	targetArtifact := workerapi.WorkspaceArtifact{
		Digest: validDigest('9'), SizeBytes: 1024,
		MediaType: workspace.ArtifactMediaType, Encoding: workspace.ArtifactEncoding,
	}
	source := workerapi.RuntimeSource{WorkspaceTarget: workerapi.WorkspaceResetTarget{
		BaseWorkspaceVersionID: pgvalue.UUIDString(capturedVersionID),
		Tree:                   workerapi.WorkspaceTreeIdentity{Digest: validDigest('8'), SizeBytes: 2048, EntryCount: 2},
		Artifact:               &targetArtifact,
	}}
	row := db.GetNextRuntimeReconcileTargetRow{
		RestoreCheckpointID:   checkpointID,
		ReservedRunID:         runID,
		ReservedAttemptNumber: pgtype.Int4{Int32: 2, Valid: true},
	}
	if err := populateRuntimeRestoreSource(context.Background(), store, &source, row); err != nil {
		t.Fatal(err)
	}
	if source.WorkspaceTarget.BaseWorkspaceVersionID != pgvalue.UUIDString(capturedVersionID) ||
		source.WorkspaceTarget.Artifact == nil ||
		source.WorkspaceTarget.Artifact.Digest != validDigest('9') {
		t.Fatalf("captured frontier was rewritten: %+v", source)
	}
	if source.Restore == nil || source.Restore.SourceWorkspaceBase == nil ||
		source.Restore.SourceWorkspaceBase.VersionID != pgvalue.UUIDString(sourceVersionID) ||
		source.Restore.SourceWorkspaceBase.Base.ArtifactDigest != validDigest('e') || store.baseCalls != 1 {
		t.Fatalf("source frontier was not projected independently: %+v", source.Restore)
	}
}

func TestRuntimeInstanceResponsePreservesActualCPUShape(t *testing.T) {
	row := db.RuntimeInstance{
		VMVCPUCount:     3,
		CPUConfigDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	response := runtimeInstanceResponse(row)
	if response.VMVCPUCount != row.VMVCPUCount || response.CPUConfigDigest != row.CPUConfigDigest {
		t.Fatalf("response CPU shape = %d/%q, want %d/%q", response.VMVCPUCount, response.CPUConfigDigest, row.VMVCPUCount, row.CPUConfigDigest)
	}
}

func TestValidateRuntimeCleanupProofIsTypedAndTimeBounded(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{
		workerapi.RuntimeCleanupSessionClosed,
		workerapi.RuntimeCleanupHostReconciled,
		workerapi.RuntimeCleanupNotMaterialized,
	} {
		if err := validateRuntimeCleanupProof(workerapi.RuntimeCleanupProof{Method: method, CompletedAt: now}, now); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	for _, proof := range []workerapi.RuntimeCleanupProof{
		{Method: "assumed", CompletedAt: now},
		{Method: workerapi.RuntimeCleanupHostReconciled},
		{Method: workerapi.RuntimeCleanupHostReconciled, CompletedAt: now.Add(2 * time.Minute)},
	} {
		if err := validateRuntimeCleanupProof(proof, now); err == nil {
			t.Fatalf("invalid proof accepted: %+v", proof)
		}
	}
}

func TestValidateRuntimeClosedCleanupProofRequiresPhysicalTeardown(t *testing.T) {
	now := time.Now().UTC()
	for _, method := range []string{
		workerapi.RuntimeCleanupSessionClosed,
		workerapi.RuntimeCleanupHostReconciled,
	} {
		if err := validateRuntimeClosedCleanupProof(workerapi.RuntimeCleanupProof{Method: method, CompletedAt: now}, now); err != nil {
			t.Fatalf("method %q rejected: %v", method, err)
		}
	}
	if err := validateRuntimeClosedCleanupProof(workerapi.RuntimeCleanupProof{
		Method: workerapi.RuntimeCleanupNotMaterialized, CompletedAt: now,
	}, now); err == nil {
		t.Fatal("not_materialized proof released a closed runtime")
	}
}

func TestMarkRuntimeInstanceFailedChargesExactReservedRun(t *testing.T) {
	ctx := context.Background()
	fixture, work, runtimeID := prepareReservedRuntimeFailure(t)
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool}
	failed, err := server.markRuntimeInstanceFailed(ctx, runtest.WorkerGroup, db.MarkRuntimeInstanceFailedParams{
		ReasonCode: pgtype.Text{String: "runtime_reconcile_failed", Valid: true},
		Error:      []byte(`{"message":"worker runtime infrastructure failed"}`),
		ID:         runtimeID, WorkerInstanceID: pgvalue.UUID(fixture.WorkerID), WorkerEpoch: 1,
		DesiredVersion: 1, ExpectedObservedVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.ObservedState != db.RuntimeObservedStateFailed || failed.ReservedRunID.Valid {
		t.Fatalf("failed runtime = %+v", failed)
	}
	var count int32
	var next pgtype.Timestamptz
	var status db.RunStatus
	if err := fixture.Pool.QueryRow(ctx, `
SELECT runtime_preparation_count, next_runtime_preparation_at, status
  FROM runs WHERE id = $1`, work.RunID).Scan(&count, &next, &status); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !next.Valid || status != db.RunStatusQueued || !next.Time.After(time.Now()) {
		t.Fatalf("charged Run = count:%d next:%v status:%s", count, next, status)
	}
}

func TestMarkRuntimeInstanceFailedFencesFatalWorkerEpoch(t *testing.T) {
	ctx := t.Context()
	fixture, work, runtimeID := prepareReservedRuntimeFailure(t)
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool}
	if _, err := server.markRuntimeInstanceFailed(ctx, runtest.WorkerGroup, db.MarkRuntimeInstanceFailedParams{
		ReasonCode: pgtype.Text{String: workerapi.RuntimeFailureWorkerInvalid, Valid: true},
		Error:      []byte(`{"message":"worker runtime infrastructure failed"}`),
		ID:         runtimeID, WorkerInstanceID: pgvalue.UUID(fixture.WorkerID), WorkerEpoch: 1,
		DesiredVersion: 1, ExpectedObservedVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
	var workerState db.WorkerInstanceState
	var count int32
	if err := fixture.Pool.QueryRow(ctx, `
SELECT worker_instances.state, runs.runtime_preparation_count
  FROM worker_instances
  JOIN runs ON runs.id = $2
 WHERE worker_instances.id = $1`, fixture.WorkerID, work.RunID).Scan(&workerState, &count); err != nil {
		t.Fatal(err)
	}
	if workerState != db.WorkerInstanceStateDraining || count != 1 {
		t.Fatalf("fatal runtime failure = Worker:%s count:%d", workerState, count)
	}
}

func TestMarkRuntimeInstanceFailedFencesFatalWorkerWithStaleRunAuthority(t *testing.T) {
	for _, test := range []struct {
		name      string
		prepare   func(t *testing.T) (runtest.Fixture, runtest.RunLease, pgtype.UUID)
		wantError bool
	}{
		{
			name: "active lease",
			prepare: func(t *testing.T) (runtest.Fixture, runtest.RunLease, pgtype.UUID) {
				fixture := runtest.New(t)
				work := fixture.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
				var runtimeID pgtype.UUID
				if err := fixture.Pool.QueryRow(t.Context(), `
SELECT runtime_instance_id FROM run_leases WHERE id = $1`, work.LeaseID).Scan(&runtimeID); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'preparing', observed_version = 2,
       observed_desired_version = 0, ready_at = NULL
 WHERE id = $1`, runtimeID)
				return fixture, work, runtimeID
			},
		},
		{
			name: "terminal Run and stale Runtime fence",
			prepare: func(t *testing.T) (runtest.Fixture, runtest.RunLease, pgtype.UUID) {
				fixture, work, runtimeID := prepareReservedRuntimeFailure(t)
				canceler, err := runauthority.NewCanceler(fixture.Pool)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := canceler.Cancel(t.Context(), runauthority.CancellationRequest{
					OrgID: fixture.OrgID, ProjectID: fixture.ProjectID,
					EnvironmentID: fixture.EnvironmentID, RunID: work.RunID,
				}); err != nil {
					t.Fatal(err)
				}
				return fixture, work, runtimeID
			},
			wantError: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, work, runtimeID := test.prepare(t)
			server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool}
			_, err := server.markRuntimeInstanceFailed(t.Context(), runtest.WorkerGroup, db.MarkRuntimeInstanceFailedParams{
				ReasonCode: pgtype.Text{String: workerapi.RuntimeFailureWorkerInvalid, Valid: true},
				Error:      []byte(`{"message":"worker runtime infrastructure failed"}`),
				ID:         runtimeID, WorkerInstanceID: pgvalue.UUID(fixture.WorkerID), WorkerEpoch: 1,
				DesiredVersion: 1, ExpectedObservedVersion: 2,
			})
			if (err != nil) != test.wantError {
				t.Fatalf("fatal stale failure error = %v, want error %v", err, test.wantError)
			}
			var workerState db.WorkerInstanceState
			var count int32
			if err := fixture.Pool.QueryRow(t.Context(), `
SELECT worker_instances.state, runs.runtime_preparation_count
  FROM worker_instances
  JOIN runs ON runs.id = $2
 WHERE worker_instances.id = $1`, fixture.WorkerID, work.RunID).Scan(&workerState, &count); err != nil {
				t.Fatal(err)
			}
			if workerState != db.WorkerInstanceStateDraining || count != 0 {
				t.Fatalf("fatal stale authority = Worker:%s count:%d", workerState, count)
			}
		})
	}
}

func prepareReservedRuntimeFailure(t *testing.T) (runtest.Fixture, runtest.RunLease, pgtype.UUID) {
	t.Helper()
	ctx := t.Context()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
	var runtimeID pgtype.UUID
	if err := fixture.Pool.QueryRow(ctx, `
SELECT runtime_instance_id FROM run_leases WHERE id = $1`, work.LeaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.Pool,
		`DELETE FROM workspace_leases WHERE owner_run_lease_id = $1`, work.LeaseID)
	dbtest.MustExec(t, ctx, fixture.Pool,
		`UPDATE runs SET current_run_lease_id = NULL, first_lease_at = NULL WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, ctx, fixture.Pool,
		`DELETE FROM run_leases WHERE id = $1`, work.LeaseID)
	dbtest.MustExec(t, ctx, fixture.Pool,
		`DELETE FROM workspace_mounts WHERE runtime_instance_id = $1`, runtimeID)
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'preparing', observed_version = 2,
       observed_desired_version = 0, ready_at = NULL,
       reserved_run_id = $2, reserved_attempt_number = 1,
       reserved_workspace_version_id = (
           SELECT base_workspace_version_id FROM runs WHERE id = $2
       ), reservation_expires_at = now() + interval '5 minutes'
 WHERE id = $1`, runtimeID, work.RunID)
	return fixture, work, runtimeID
}

func TestMarkRuntimeInstanceFailedRejectsStaleRunAuthority(t *testing.T) {
	for _, test := range []struct {
		name  string
		stale func(t *testing.T, fixture runtest.Fixture, work runtest.RunLease)
	}{
		{
			name: "active lease",
			stale: func(_ *testing.T, _ runtest.Fixture, _ runtest.RunLease) {
				// AddRunLease leaves the exact Run Lease active.
			},
		},
		{
			name: "new attempt",
			stale: func(t *testing.T, fixture runtest.Fixture, work runtest.RunLease) {
				ctx := t.Context()
				dbtest.MustExec(t, ctx, fixture.Pool, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id,
    base_workspace_version_id, created_at
)
SELECT run_id, 2, entrypoint_kind, workspace_id,
       base_workspace_version_id, now()
  FROM run_attempts
 WHERE run_id = $1 AND number = 1`, work.RunID)
				dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runs
   SET current_run_lease_id = NULL,
       current_attempt_number = 2
 WHERE id = $1`, work.RunID)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			fixture := runtest.New(t)
			work := fixture.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
			var runtimeID pgtype.UUID
			if err := fixture.Pool.QueryRow(ctx, `
SELECT runtime_instance_id FROM run_leases WHERE id = $1`, work.LeaseID).Scan(&runtimeID); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'preparing', observed_version = 2,
       observed_desired_version = 0, ready_at = NULL,
       reserved_run_id = $2, reserved_attempt_number = 1,
       reserved_workspace_version_id = (
           SELECT base_workspace_version_id FROM runs WHERE id = $2
       ), reservation_expires_at = now() + interval '5 minutes'
 WHERE id = $1`, runtimeID, work.RunID)
			test.stale(t, fixture, work)

			server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool}
			_, err := server.markRuntimeInstanceFailed(ctx, runtest.WorkerGroup, db.MarkRuntimeInstanceFailedParams{
				ReasonCode: pgtype.Text{String: "runtime_reconcile_failed", Valid: true},
				Error:      []byte(`{"message":"worker runtime infrastructure failed"}`),
				ID:         runtimeID, WorkerInstanceID: pgvalue.UUID(fixture.WorkerID), WorkerEpoch: 1,
				DesiredVersion: 1, ExpectedObservedVersion: 2,
			})
			if err == nil {
				t.Fatal("stale Run authority marked its runtime failed")
			}
			var state db.RuntimeObservedState
			var count int32
			if err := fixture.Pool.QueryRow(ctx, `
SELECT runtime_instances.observed_state, runs.runtime_preparation_count
  FROM runtime_instances
  JOIN runs ON runs.id = $2
 WHERE runtime_instances.id = $1`, runtimeID, work.RunID).Scan(&state, &count); err != nil {
				t.Fatal(err)
			}
			if state != db.RuntimeObservedStatePreparing || count != 0 {
				t.Fatalf("stale authority changed state = runtime:%s count:%d", state, count)
			}
		})
	}
}

func TestMarkRuntimeInstanceFailedChargesOnceUnderConcurrentReplay(t *testing.T) {
	ctx := t.Context()
	fixture, work, runtimeID := prepareReservedRuntimeFailure(t)
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool}
	params := db.MarkRuntimeInstanceFailedParams{
		ReasonCode: pgtype.Text{String: workerapi.RuntimeFailureReconcile, Valid: true},
		Error:      []byte(`{"message":"runtime preparation failed"}`),
		ID:         runtimeID, WorkerInstanceID: pgvalue.UUID(fixture.WorkerID), WorkerEpoch: 1,
		DesiredVersion: 1, ExpectedObservedVersion: 2,
	}
	start := make(chan struct{})
	errorsByCall := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, err := server.markRuntimeInstanceFailed(ctx, runtest.WorkerGroup, params)
			errorsByCall <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errorsByCall)
	succeeded := 0
	failed := 0
	for err := range errorsByCall {
		if err == nil {
			succeeded++
		} else {
			failed++
		}
	}
	var count int32
	if err := fixture.Pool.QueryRow(ctx,
		`SELECT runtime_preparation_count FROM runs WHERE id = $1`, work.RunID,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if succeeded != 1 || failed != 1 || count != 1 {
		t.Fatalf("concurrent replay = succeeded:%d failed:%d count:%d", succeeded, failed, count)
	}
}

func TestMarkRuntimeInstanceFailedSerializesWithLeaseGrant(t *testing.T) {
	ctx := t.Context()
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
	var runtimeID pgtype.UUID
	var stateVersion int64
	if err := fixture.Pool.QueryRow(ctx, `
SELECT run_leases.runtime_instance_id, runs.state_version
  FROM run_leases
  JOIN runs ON runs.id = run_leases.run_id
 WHERE run_leases.id = $1`, work.LeaseID).Scan(&runtimeID, &stateVersion); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runs SET current_run_lease_id = NULL WHERE id = $1`, work.RunID)
	dbtest.MustExec(t, ctx, fixture.Pool, `
UPDATE runtime_instances
   SET observed_state = 'preparing', observed_version = 2,
       observed_desired_version = 0, ready_at = NULL,
       reserved_run_id = $2, reserved_attempt_number = 1,
       reserved_workspace_version_id = (
           SELECT base_workspace_version_id FROM runs WHERE id = $2
       ), reservation_expires_at = now() + interval '5 minutes'
 WHERE id = $1`, runtimeID, work.RunID)
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := server.markRuntimeInstanceFailed(ctx, runtest.WorkerGroup, db.MarkRuntimeInstanceFailedParams{
			ReasonCode: pgtype.Text{String: workerapi.RuntimeFailureReconcile, Valid: true},
			Error:      []byte(`{"message":"runtime preparation failed"}`),
			ID:         runtimeID, WorkerInstanceID: pgvalue.UUID(fixture.WorkerID), WorkerEpoch: 1,
			DesiredVersion: 1, ExpectedObservedVersion: 2,
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := db.New(fixture.Pool).SetRunCurrentLease(ctx, db.SetRunCurrentLeaseParams{
			RunLeaseID: pgvalue.UUID(work.LeaseID), ID: pgvalue.UUID(work.RunID),
			OrgID: pgvalue.UUID(fixture.OrgID), ExpectedStateVersion: stateVersion,
			AttemptNumber: 1,
		})
		results <- err
	}()
	close(start)
	first := <-results
	second := <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("runtime failure/Lease grant results = %v / %v", first, second)
	}
	var count int32
	var currentLease pgtype.UUID
	var runtimeState db.RuntimeObservedState
	if err := fixture.Pool.QueryRow(ctx, `
SELECT runs.runtime_preparation_count,
       runs.current_run_lease_id,
       runtime_instances.observed_state
  FROM runs
  JOIN runtime_instances ON runtime_instances.id = $2
 WHERE runs.id = $1`, work.RunID, runtimeID).Scan(&count, &currentLease, &runtimeState); err != nil {
		t.Fatal(err)
	}
	if currentLease.Valid {
		if count != 0 || runtimeState != db.RuntimeObservedStatePreparing {
			t.Fatalf("Lease won = count:%d runtime:%s", count, runtimeState)
		}
	} else if count != 1 || runtimeState != db.RuntimeObservedStateFailed {
		t.Fatalf("failure won = count:%d runtime:%s", count, runtimeState)
	}
}

func TestMarkRuntimeInstanceFailedSerializesWithFullPlacement(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	fixture, work, runtimeID := prepareReservedRuntimeFailure(t)
	var stateVersion int64
	if err := fixture.Pool.QueryRow(ctx,
		`SELECT state_version FROM runs WHERE id = $1`, work.RunID,
	).Scan(&stateVersion); err != nil {
		t.Fatal(err)
	}
	fencingKey, err := workspace.NewFencingKey(bytes.Repeat([]byte{9}, workspace.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(fixture.Pool, fencingKey)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := server.markRuntimeInstanceFailed(ctx, runtest.WorkerGroup, db.MarkRuntimeInstanceFailedParams{
			ReasonCode: pgtype.Text{String: workerapi.RuntimeFailureReconcile, Valid: true},
			Error:      []byte(`{"message":"runtime preparation failed"}`),
			ID:         runtimeID, WorkerInstanceID: pgvalue.UUID(fixture.WorkerID), WorkerEpoch: 1,
			DesiredVersion: 1, ExpectedObservedVersion: 2,
		})
		results <- err
	}()
	go func() {
		<-start
		_, err := authority.PlaceReadyRun(ctx, dispatch.ReadyRunCandidate{
			OrgID: pgvalue.UUID(fixture.OrgID), RunID: pgvalue.UUID(work.RunID),
			ExpectedRunStateVersion: stateVersion,
		})
		results <- err
	}()
	close(start)
	for range 2 {
		err := <-results
		var postgresError *pgconn.PgError
		if errors.As(err, &postgresError) && postgresError.Code == "40P01" {
			t.Fatalf("placement/runtime failure deadlocked: %v", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("placement/runtime failure did not make progress: %v", err)
		}
	}
}

func TestMarkRuntimeInstanceFailedSerializesWithTerminalization(t *testing.T) {
	ctx := t.Context()
	fixture, work, runtimeID := prepareReservedRuntimeFailure(t)
	server := &Server{db: db.New(fixture.Pool), tx: fixture.Pool}
	canceler, err := runauthority.NewCanceler(fixture.Pool)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	markResult := make(chan error, 1)
	cancelResult := make(chan error, 1)
	go func() {
		<-start
		_, err := server.markRuntimeInstanceFailed(ctx, runtest.WorkerGroup, db.MarkRuntimeInstanceFailedParams{
			ReasonCode: pgtype.Text{String: workerapi.RuntimeFailureReconcile, Valid: true},
			Error:      []byte(`{"message":"runtime preparation failed"}`),
			ID:         runtimeID, WorkerInstanceID: pgvalue.UUID(fixture.WorkerID), WorkerEpoch: 1,
			DesiredVersion: 1, ExpectedObservedVersion: 2,
		})
		markResult <- err
	}()
	go func() {
		<-start
		_, err := canceler.Cancel(ctx, runauthority.CancellationRequest{
			OrgID: fixture.OrgID, ProjectID: fixture.ProjectID,
			EnvironmentID: fixture.EnvironmentID, RunID: work.RunID,
		})
		cancelResult <- err
	}()
	close(start)
	markErr := <-markResult
	if err := <-cancelResult; err != nil {
		t.Fatalf("terminalization error = %v (mark error %v)", err, markErr)
	}
	var status db.RunStatus
	var count int32
	var reservedRun pgtype.UUID
	var runtimeState db.RuntimeObservedState
	var desiredState db.RuntimeDesiredState
	if err := fixture.Pool.QueryRow(ctx, `
SELECT runs.status,
       runs.runtime_preparation_count,
       runtime_instances.reserved_run_id,
       runtime_instances.observed_state,
       runtime_instances.desired_state
  FROM runs
  JOIN runtime_instances ON runtime_instances.id = $2
 WHERE runs.id = $1`, work.RunID, runtimeID).Scan(
		&status, &count, &reservedRun, &runtimeState, &desiredState,
	); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusCancelled || count < 0 || count > 1 {
		t.Fatalf(
			"terminal race = run:%s count:%d reserved:%v runtime:%s mark:%v",
			status, count, reservedRun, runtimeState, markErr,
		)
	}
	if count == 1 && runtimeState != db.RuntimeObservedStateFailed {
		t.Fatalf("charged terminal race runtime = %s, want failed", runtimeState)
	}
	if count == 0 && (!reservedRun.Valid || desiredState != db.RuntimeDesiredStateClosed) {
		t.Fatalf("terminalization winner = reserved:%v desired:%s", reservedRun, desiredState)
	}
}
