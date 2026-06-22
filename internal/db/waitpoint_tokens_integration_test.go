package db_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testWorkspaceOperationMaxClaimAttempts = 3

func isDBUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func TestWaitpointTokenCompletionIdempotencyUsesData(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	tokenID := uuid.Must(uuid.NewV7())
	token, err := queries.CreateWaitpointToken(ctx, db.CreateWaitpointTokenParams{
		ID:                 pgvalue.UUID(tokenID),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		CallbackSecretHash: []byte("callback-hash-a"),
		TimeoutAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		Metadata:           []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"approved":true,"reason":"ok"}`)
	first, err := queries.CompleteWaitpointToken(ctx, db.CompleteWaitpointTokenParams{
		OrgID:          token.OrgID,
		ID:             token.ID,
		Data:           data,
		CompletionHash: pgvalue.Text(completionHash(t, data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != db.WaitpointTokenStatusCompleted {
		t.Fatalf("status = %s", first.Status)
	}
	second, err := queries.CompleteWaitpointToken(ctx, db.CompleteWaitpointTokenParams{
		OrgID:          token.OrgID,
		ID:             token.ID,
		Data:           []byte(`{"reason":"ok","approved":true}`),
		CompletionHash: pgvalue.Text(completionHash(t, []byte(`{"reason":"ok","approved":true}`))),
	})
	if err != nil {
		t.Fatalf("same canonical data should be idempotent: %v", err)
	}
	if string(second.Metadata) != `{}` {
		t.Fatalf("metadata = %s", second.Metadata)
	}
	_, err = queries.CompleteWaitpointToken(ctx, db.CompleteWaitpointTokenParams{
		OrgID:          token.OrgID,
		ID:             token.ID,
		Data:           []byte(`{"approved":false}`),
		CompletionHash: pgvalue.Text(completionHash(t, []byte(`{"approved":false}`))),
	})
	if err == nil {
		t.Fatal("different completion data should conflict")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("different data error = %v", err)
	}
}

func TestWorkspaceMaterializationStateMachine(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	workerID := seedMaterializationWorker(t, ctx, pool)
	queries := db.New(pool)
	rootfsDigest := deploymentSandboxRootfsDigest(t, ctx, pool, ids)

	requested, err := requestWorkspaceMaterializationForTest(ctx, queries, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      10,
		Request:       []byte(`{"test":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if requested.State != db.WorkspaceMaterializationStateRequested || !requested.BaseVersionID.Valid {
		t.Fatalf("requested materialization state=%s base=%v", requested.State, requested.BaseVersionID)
	}

	claimed, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(workerID),
		ReservationToken:        "reservation-token",
		ReservationExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		GuestdChannelTokenHash:  "channel-token-hash",
		RuntimeID:               "runtime-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != db.WorkspaceMaterializationStateMaterializing || claimed.WorkerInstanceID != pgvalue.UUID(workerID) || claimed.RuntimeID != "runtime-test" {
		t.Fatalf("claimed materialization = %+v", claimed)
	}
	capacity, err := queries.GetWorkerInstanceQueueCapacity(ctx, pgvalue.UUID(workerID))
	if err != nil {
		t.Fatal(err)
	}
	if capacity.AvailableMilliCpu != 1000 ||
		capacity.AvailableMemoryMib != 1024 ||
		capacity.AvailableDiskMib != 4096 ||
		capacity.AvailableExecutionSlots != 0 {
		t.Fatalf("materialization queue capacity should reserve live materialization resources: capacity=%+v claimed=%+v", capacity, claimed)
	}
	if _, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(workerID),
		ReservationToken:        "second-token",
		RuntimeID:               "runtime-test",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("concurrent claim err = %v, want no rows", err)
	}

	renewed, err := queries.RenewWorkspaceMaterialization(ctx, db.RenewWorkspaceMaterializationParams{
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(2 * time.Minute)),
		OrgID:                pgvalue.UUID(ids.orgID),
		ID:                   requested.ID,
		WorkerInstanceID:     pgvalue.UUID(workerID),
		ReservationToken:     "reservation-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.State != db.WorkspaceMaterializationStateMaterializing || !renewed.LastHeartbeatAt.Valid {
		t.Fatalf("renewed materialization = %+v", renewed)
	}
	if !renewed.ReservationExpiresAt.Valid || time.Until(renewed.ReservationExpiresAt.Time) < time.Minute {
		t.Fatalf("renew did not extend reservation: %+v", renewed.ReservationExpiresAt)
	}
	running, err := queries.MarkWorkspaceMaterializationRunning(ctx, db.MarkWorkspaceMaterializationRunningParams{
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(2 * time.Minute)),
		OrgID:                pgvalue.UUID(ids.orgID),
		ID:                   requested.ID,
		WorkerInstanceID:     pgvalue.UUID(workerID),
		ReservationToken:     "reservation-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if running.State != db.WorkspaceMaterializationStateRunning || !running.MaterializedAt.Valid {
		t.Fatalf("running materialization = %+v", running)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET reservation_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, requested.ID); err != nil {
		t.Fatal(err)
	}
	runCapacity, err := queries.GetWorkerInstanceRunDispatchCapacity(ctx, pgvalue.UUID(workerID))
	if err != nil {
		t.Fatal(err)
	}
	if runCapacity.AvailableMilliCpu != 2000 ||
		runCapacity.AvailableMemoryMib != 2048 ||
		runCapacity.AvailableDiskMib != 4096 ||
		runCapacity.AvailableExecutionSlots != 1 {
		t.Fatalf("run dispatch capacity should stay available for attached runs: capacity=%+v claimed=%+v", runCapacity, claimed)
	}
	operationExecID, operationLease := seedWorkspaceExecWithActiveWriteLease(t, ctx, pool, ids, running)
	operation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  requested.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         operationExecID,
		RequestFingerprint: "test-start-exec-dispatch",
		WriteLeaseID:       operationLease.ID,
		FencingToken:       operationLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := queries.StopWorkspaceMaterialization(ctx, db.StopWorkspaceMaterializationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ReservationToken: "reservation-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != db.WorkspaceMaterializationStateStopped {
		t.Fatalf("stopped state = %s", stopped.State)
	}
	if stopped.ReservedCpuMillis != 0 || stopped.ReservedMemoryMib != 0 || stopped.ReservedDiskMib != 0 || stopped.ReservedExecutionSlots != 0 {
		t.Fatalf("stopped materialization kept reservation cpu=%d memory=%d disk=%d slots=%d", stopped.ReservedCpuMillis, stopped.ReservedMemoryMib, stopped.ReservedDiskMib, stopped.ReservedExecutionSlots)
	}
	stoppedOperation, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stoppedOperation.State != db.WorkspaceMaterializationOperationStateCancelled {
		t.Fatalf("stopped operation state = %s", stoppedOperation.State)
	}
	capacity, err = queries.GetWorkerInstanceQueueCapacity(ctx, pgvalue.UUID(workerID))
	if err != nil {
		t.Fatal(err)
	}
	if capacity.AvailableMilliCpu != 2000 || capacity.AvailableMemoryMib != 2048 || capacity.AvailableDiskMib != 4096 || capacity.AvailableExecutionSlots != 1 {
		t.Fatalf("stopped materialization did not release capacity: %+v", capacity)
	}
	if _, err := queries.StopWorkspaceMaterialization(ctx, db.StopWorkspaceMaterializationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ReservationToken: "reservation-token",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("invalid stopped transition err = %v, want no rows", err)
	}
	var currentVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT current_version_id FROM workspaces WHERE id = $1`, ids.workspaceID).Scan(&currentVersionID); err != nil {
		t.Fatal(err)
	}
	if currentVersionID != pgvalue.MustUUIDValue(requested.BaseVersionID) {
		t.Fatalf("current_version_id = %s, want base version %s", currentVersionID, pgvalue.MustUUIDValue(requested.BaseVersionID))
	}
}

func TestWorkspaceMaterializationStopCapturePromotesSystemVersion(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	baseVersionID := pgvalue.MustUUIDValue(materialization.BaseVersionID)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET dirty_generation = 1
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, materialization.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET dirty_state = 'dirty'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	requestedStop, err := queries.RequestWorkspaceMaterializationStop(ctx, db.RequestWorkspaceMaterializationStopParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestedStop.State != db.WorkspaceMaterializationStateCapturing || requestedStop.DirtyGeneration != 1 {
		t.Fatalf("requested stop = %+v, want capturing dirty generation 1", requestedStop)
	}
	var desiredState, dirtyState string
	if err := pool.QueryRow(ctx, `
		SELECT desired_state::text, dirty_state::text
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&desiredState, &dirtyState); err != nil {
		t.Fatal(err)
	}
	if desiredState != "stopped" || dirtyState != "capturing" {
		t.Fatalf("workspace desired_state=%s dirty_state=%s, want stopped/capturing", desiredState, dirtyState)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET dirty_generation = 2
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, materialization.ID); err != nil {
		t.Fatal(err)
	}
	artifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	versionID := uuid.Must(uuid.NewV7())
	version, err := queries.PromoteWorkspaceMaterializationStopCapture(ctx, db.PromoteWorkspaceMaterializationStopCaptureParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		ID:                 materialization.ID,
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		WorkerInstanceID:   materialization.WorkerInstanceID,
		ReservationToken:   materialization.ReservationToken,
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		ArtifactID:         pgvalue.UUID(artifactID),
		SizeBytes:          10,
		ArtifactEncoding:   workspace.ArtifactEncoding,
		ContentDigest:      testDigest("stop-capture-content"),
		VersionID:          pgvalue.UUID(versionID),
		ArtifactEntryCount: 1,
		Message:            "capture before controlled stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if version.ID != pgvalue.UUID(versionID) || version.Kind != db.WorkspaceVersionKindSystem || version.State != db.WorkspaceVersionStateReady {
		t.Fatalf("promoted version = %+v", version)
	}
	if pgvalue.MustUUIDValue(version.ParentVersionID) != baseVersionID || pgvalue.MustUUIDValue(version.SourceMaterializationID) != pgvalue.MustUUIDValue(materialization.ID) {
		t.Fatalf("promoted version lineage parent=%v source=%v", version.ParentVersionID, version.SourceMaterializationID)
	}
	var promotedDirtyGeneration int64
	if err := pool.QueryRow(ctx, `
		SELECT dirty_generation
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, materialization.ID).Scan(&promotedDirtyGeneration); err != nil {
		t.Fatal(err)
	}
	if promotedDirtyGeneration != 0 {
		t.Fatalf("promoted materialization dirty_generation = %d, want 0", promotedDirtyGeneration)
	}
	stopped, err := queries.StopWorkspaceMaterialization(ctx, db.StopWorkspaceMaterializationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               materialization.ID,
		WorkerInstanceID: materialization.WorkerInstanceID,
		ReservationToken: materialization.ReservationToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != db.WorkspaceMaterializationStateStopped {
		t.Fatalf("stopped state = %s", stopped.State)
	}
	var currentVersionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT current_version_id, dirty_state::text
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&currentVersionID, &dirtyState); err != nil {
		t.Fatal(err)
	}
	if currentVersionID != versionID || dirtyState != "clean" {
		t.Fatalf("workspace current_version_id=%s dirty_state=%s, want %s/clean", currentVersionID, dirtyState, versionID)
	}
}

func TestWorkspaceMaterializationStopCaptureFailureRequiresRecovery(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	currentBefore := currentWorkspaceVersionID(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET dirty_generation = 1
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, materialization.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET dirty_state = 'dirty'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	requestedStop, err := queries.RequestWorkspaceMaterializationStop(ctx, db.RequestWorkspaceMaterializationStopParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestedStop.State != db.WorkspaceMaterializationStateCapturing {
		t.Fatalf("requested stop state = %s, want capturing", requestedStop.State)
	}
	failed, err := queries.FailWorkspaceMaterialization(ctx, db.FailWorkspaceMaterializationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               materialization.ID,
		WorkerInstanceID: materialization.WorkerInstanceID,
		ReservationToken: materialization.ReservationToken,
		Error:            []byte(`{"code":"workspace_materialization_recovery_required"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != db.WorkspaceMaterializationStateFailed {
		t.Fatalf("failed materialization state = %s", failed.State)
	}
	var workspaceState, dirtyState string
	var currentAfter uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT state::text, dirty_state::text, current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&workspaceState, &dirtyState, &currentAfter); err != nil {
		t.Fatal(err)
	}
	if workspaceState != "recovery_required" || dirtyState != "capture_failed" || currentAfter != currentBefore {
		t.Fatalf("workspace state=%s dirty_state=%s current=%s, want recovery_required/capture_failed/current %s", workspaceState, dirtyState, currentAfter, currentBefore)
	}
	var promotedCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_versions
		 WHERE org_id = $1
		   AND source_materialization_id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(materialization.ID)).Scan(&promotedCount); err != nil {
		t.Fatal(err)
	}
	if promotedCount != 0 {
		t.Fatalf("capture failure promoted %d workspace versions, want 0", promotedCount)
	}
}

func TestWorkspaceStopWithoutActiveMaterializationMarksDesiredStopped(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	if _, err := queries.RequestWorkspaceMaterializationStop(ctx, db.RequestWorkspaceMaterializationStopParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("request stop without materialization err = %v, want no rows", err)
	}
	workspaceRow, err := queries.SetWorkspaceDesiredStopped(ctx, db.SetWorkspaceDesiredStoppedParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ID:            pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspaceRow.DesiredState != db.WorkspaceDesiredStateStopped || workspaceRow.State != db.WorkspaceStateActive {
		t.Fatalf("workspace state=%s desired=%s, want active/stopped", workspaceRow.State, workspaceRow.DesiredState)
	}
}

func TestEnsureWorkspaceOperationIdempotencyReplacesExpiredRow(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	firstID := uuid.Must(uuid.NewV7())
	first, err := queries.EnsureWorkspaceOperationIdempotency(ctx, db.EnsureWorkspaceOperationIdempotencyParams{
		ID:                   pgvalue.UUID(firstID),
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgtype.UUID{},
		OperationKind:        "workspace.create",
		IdempotencyKey:       "expired-key",
		RequestFingerprint:   "expired-fingerprint",
		ResponseResourceType: "",
		ResponseResourceID:   pgtype.UUID{},
		ResponseBody:         []byte(`{}`),
		ExpiresAt:            pgvalue.Timestamptz(time.Now().Add(-time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Inserted {
		t.Fatalf("first inserted = false")
	}
	second, err := queries.EnsureWorkspaceOperationIdempotency(ctx, db.EnsureWorkspaceOperationIdempotencyParams{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgtype.UUID{},
		OperationKind:        "workspace.create",
		IdempotencyKey:       "expired-key",
		RequestFingerprint:   "fresh-fingerprint",
		ResponseResourceType: "",
		ResponseResourceID:   pgtype.UUID{},
		ResponseBody:         []byte(`{}`),
		ExpiresAt:            pgvalue.Timestamptz(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Inserted {
		t.Fatalf("expired reuse inserted = false")
	}
	if second.ID != first.ID {
		t.Fatalf("expired reuse id = %s, want same authority row %s", pgvalue.MustUUIDValue(second.ID), pgvalue.MustUUIDValue(first.ID))
	}
	if second.RequestFingerprint != "fresh-fingerprint" || second.ResponseResourceID.Valid {
		t.Fatalf("expired reuse row = %+v, want fresh pending fingerprint", second)
	}
}

func TestWorkspaceStopPreventsLateExecStart(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	execID := seedWorkspaceExec(t, ctx, pool, ids, materialization)
	lease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgvalue.UUID(execID),
		OwnerPtySessionID: pgtype.UUID{},
		FencingToken:      "late-exec-fence",
		HeartbeatToken:    "late-exec-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BindWorkspaceExecMaterialization(ctx, db.BindWorkspaceExecMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      lease.ID,
		State:             db.WorkspaceExecStateQueued,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pgvalue.UUID(execID),
	}); err != nil {
		t.Fatal(err)
	}
	stopping, err := queries.RequestWorkspaceMaterializationStop(ctx, db.RequestWorkspaceMaterializationStopParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopping.State != db.WorkspaceMaterializationStateStopping {
		t.Fatalf("stop state = %s, want stopping for clean materialization", stopping.State)
	}
	if _, err := queries.MarkWorkspaceExecStarted(ctx, db.MarkWorkspaceExecStartedParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pgvalue.UUID(execID),
		MaterializationID: materialization.ID,
		ProcessID:         "late-exec-process",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late exec start err = %v, want no rows", err)
	}
	assertWorkspaceDirtyGeneration(t, ctx, pool, ids, materialization.ID, 0)
}

func TestWorkspaceStopPreventsLatePtyOpen(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	pty, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgtype.UUID{},
		OwnerPtySessionID: pty.ID,
		FencingToken:      "late-pty-fence",
		HeartbeatToken:    "late-pty-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	pty, err = queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      lease.ID,
		State:             db.WorkspacePtyStateCreating,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pty.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopping, err := queries.RequestWorkspaceMaterializationStop(ctx, db.RequestWorkspaceMaterializationStopParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stopping.State != db.WorkspaceMaterializationStateStopping {
		t.Fatalf("stop state = %s, want stopping for clean materialization", stopping.State)
	}
	if _, err := queries.MarkWorkspacePtyOpen(ctx, db.MarkWorkspacePtyOpenParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pty.ID,
		MaterializationID: materialization.ID,
		ProcessID:         "late-pty-process",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("late pty open err = %v, want no rows", err)
	}
	assertWorkspaceDirtyGeneration(t, ctx, pool, ids, materialization.ID, 0)
}

func TestWorkspaceExecStartedMarksWriteLeaseDirtyOnce(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	execID := seedWorkspaceExec(t, ctx, pool, ids, materialization)
	leaseID := uuid.Must(uuid.NewV7())
	lease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(leaseID),
		OwnerExecID:       pgvalue.UUID(execID),
		OwnerPtySessionID: pgtype.UUID{},
		FencingToken:      "exec-dirty-fence",
		HeartbeatToken:    "exec-dirty-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_execs
		   SET write_lease_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, lease.ID, ids.orgID, execID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i += 1 {
		if _, err := queries.MarkWorkspaceExecStarted(ctx, db.MarkWorkspaceExecStartedParams{
			OrgID:             pgvalue.UUID(ids.orgID),
			ProjectID:         pgvalue.UUID(ids.projectID),
			EnvironmentID:     pgvalue.UUID(ids.environmentID),
			WorkspaceID:       pgvalue.UUID(ids.workspaceID),
			ID:                pgvalue.UUID(execID),
			MaterializationID: materialization.ID,
			ProcessID:         "exec-dirty-process",
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertWorkspaceDirtyGeneration(t, ctx, pool, ids, materialization.ID, 1)
}

func TestWorkspacePtyOpenedMarksWriteLeaseDirtyOnce(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	ptyID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_pty_sessions (
			id, org_id, project_id, environment_id, workspace_id,
			materialization_id, cwd, cols, rows, state
		)
		VALUES ($1, $2, $3, $4, $5, $6, '/workspace', 80, 24, 'creating')
	`, ptyID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, pgvalue.MustUUIDValue(materialization.ID)); err != nil {
		t.Fatal(err)
	}
	leaseID := uuid.Must(uuid.NewV7())
	lease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(leaseID),
		OwnerExecID:       pgtype.UUID{},
		OwnerPtySessionID: pgvalue.UUID(ptyID),
		FencingToken:      "pty-dirty-fence",
		HeartbeatToken:    "pty-dirty-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_pty_sessions
		   SET write_lease_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, lease.ID, ids.orgID, ptyID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i += 1 {
		if _, err := queries.MarkWorkspacePtyOpen(ctx, db.MarkWorkspacePtyOpenParams{
			OrgID:             pgvalue.UUID(ids.orgID),
			ProjectID:         pgvalue.UUID(ids.projectID),
			EnvironmentID:     pgvalue.UUID(ids.environmentID),
			WorkspaceID:       pgvalue.UUID(ids.workspaceID),
			ID:                pgvalue.UUID(ptyID),
			MaterializationID: materialization.ID,
			ProcessID:         "pty-dirty-process",
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertWorkspaceDirtyGeneration(t, ctx, pool, ids, materialization.ID, 1)
}

func TestEnsureWorkspaceMaterializationRequestedCreatesOneActiveArtifactBackedRequest(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)

	first, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      7,
		Request:       []byte(`{"source":"task_start","run_id":"first"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != db.WorkspaceMaterializationStateRequested || !first.BaseVersionID.Valid {
		t.Fatalf("first materialization state=%s base=%v", first.State, first.BaseVersionID)
	}
	if !first.Inserted {
		t.Fatal("first ensure returned inserted=false, want true")
	}
	if !first.ImageArtifactID.Valid || first.ImageDigest == "" || first.RootfsDigest == "" {
		t.Fatalf("first materialization missing sandbox artifact authority: image_artifact=%v image_digest=%q rootfs_digest=%q", first.ImageArtifactID, first.ImageDigest, first.RootfsDigest)
	}
	if !first.WorkspaceArtifactID.Valid || first.WorkspaceArtifactDigest == "" || first.WorkspaceMountPath != "/workspace" {
		t.Fatalf("first materialization missing workspace artifact authority: artifact=%v digest=%q mount=%q", first.WorkspaceArtifactID, first.WorkspaceArtifactDigest, first.WorkspaceMountPath)
	}

	secondID := uuid.Must(uuid.NewV7())
	second, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(secondID),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      1,
		Request:       []byte(`{"source":"task_start","run_id":"second"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second ensure materialization id = %s, want existing %s", pgvalue.MustUUIDValue(second.ID), pgvalue.MustUUIDValue(first.ID))
	}
	if second.Inserted {
		t.Fatal("second ensure returned inserted=true for existing runnable materialization")
	}
	if second.ID == pgvalue.UUID(secondID) {
		t.Fatal("second ensure inserted a duplicate active materialization")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET desired_state = 'stopped'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	third, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      10,
		Request:       []byte(`{"source":"task_start","run_id":"third"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if third.ID != first.ID {
		t.Fatalf("third ensure materialization id = %s, want existing %s", pgvalue.MustUUIDValue(third.ID), pgvalue.MustUUIDValue(first.ID))
	}
	if third.Inserted {
		t.Fatal("third ensure returned inserted=true for priority bump")
	}
	if third.Priority != 10 {
		t.Fatalf("third ensure priority = %d, want 10", third.Priority)
	}
	assertWorkspaceDesiredState(t, ctx, pool, ids, "active")
	var storedPriority int32
	if err := pool.QueryRow(ctx, `
		SELECT priority
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(first.ID)).Scan(&storedPriority); err != nil {
		t.Fatal(err)
	}
	if storedPriority != 10 {
		t.Fatalf("stored materialization priority = %d, want 10", storedPriority)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET state = 'running'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(first.ID)); err != nil {
		t.Fatal(err)
	}
	running, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      20,
		Request:       []byte(`{"source":"task_start","run_id":"running"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if running.ID != first.ID {
		t.Fatalf("running ensure materialization id = %s, want existing %s", pgvalue.MustUUIDValue(running.ID), pgvalue.MustUUIDValue(first.ID))
	}
	if running.Priority != 10 {
		t.Fatalf("running ensure priority = %d, want unchanged 10", running.Priority)
	}
	var activeCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
	`, ids.orgID, ids.workspaceID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active materializations = %d, want 1", activeCount)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET state = 'stopped'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET desired_state = 'stopped'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	rematerialized, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      0,
		Request:       []byte(`{"source":"workspace.materialize","after_stop":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rematerialized.Inserted || rematerialized.ID == first.ID {
		t.Fatalf("rematerialized ensure = id %s inserted=%v, want new inserted materialization", pgvalue.MustUUIDValue(rematerialized.ID), rematerialized.Inserted)
	}
	assertWorkspaceDesiredState(t, ctx, pool, ids, "active")
}

func TestQueuedRunWorkspaceMaterializationDependencyIsExact(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	seedCurrentAttempt(t, ctx, pool, ids, db.RunAttemptStatusQueued)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'queued',
		       execution_status = 'queued'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	first, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      0,
		Request:       []byte(`{"source":"test","run":"first"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.SetQueuedRunWorkspaceMaterialization(ctx, db.SetQueuedRunWorkspaceMaterializationParams{
		OrgID:                      pgvalue.UUID(ids.orgID),
		RunID:                      pgvalue.UUID(ids.runID),
		WorkspaceID:                pgvalue.UUID(ids.workspaceID),
		WorkspaceMaterializationID: first.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET state = 'failed'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(first.ID)); err != nil {
		t.Fatal(err)
	}
	second, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      0,
		Request:       []byte(`{"source":"test","run":"second"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("second ensure reused failed materialization")
	}
	secondRunID := seedQueuedRunForWorkspaceMaterializationTest(t, ctx, pool, ids)
	if err := queries.SetQueuedRunWorkspaceMaterialization(ctx, db.SetQueuedRunWorkspaceMaterializationParams{
		OrgID:                      pgvalue.UUID(ids.orgID),
		RunID:                      pgvalue.UUID(secondRunID),
		WorkspaceID:                pgvalue.UUID(ids.workspaceID),
		WorkspaceMaterializationID: second.ID,
	}); err != nil {
		t.Fatal(err)
	}
	waiting, err := queries.ListQueuedRunsForWorkspaceMaterialization(ctx, db.ListQueuedRunsForWorkspaceMaterializationParams{
		OrgID:                      pgvalue.UUID(ids.orgID),
		WorkspaceID:                pgvalue.UUID(ids.workspaceID),
		WorkspaceMaterializationID: first.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting) != 1 || pgvalue.MustUUIDValue(waiting[0]) != ids.runID {
		t.Fatalf("first materialization queued runs = %v, want only %s", waiting, ids.runID)
	}
	if err := queries.FailQueuedRun(ctx, db.FailQueuedRunParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		RunID:        pgvalue.UUID(ids.runID),
		ErrorName:    "WorkspaceMaterializationFailed",
		ErrorMessage: "materialization failed",
		Reason:       []byte(`{"origin":"workspace_materialization"}`),
	}); err != nil {
		t.Fatal(err)
	}
	firstRun, err := queries.GetRun(ctx, db.GetRunParams{OrgID: pgvalue.UUID(ids.orgID), ID: pgvalue.UUID(ids.runID)})
	if err != nil {
		t.Fatal(err)
	}
	if firstRun.Status != db.RunStatusFailed {
		t.Fatalf("first run status = %s, want failed", firstRun.Status)
	}
	secondRun, err := queries.GetRun(ctx, db.GetRunParams{OrgID: pgvalue.UUID(ids.orgID), ID: pgvalue.UUID(secondRunID)})
	if err != nil {
		t.Fatal(err)
	}
	if secondRun.Status != db.RunStatusQueued {
		t.Fatalf("second run status = %s, want queued", secondRun.Status)
	}
}

func TestEnsureWorkspaceMaterializationRequestedSerializesConcurrentFirstRequests(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	const callers = 8
	start := make(chan struct{})
	results := make(chan db.EnsureWorkspaceMaterializationRequestedRow, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			row, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
				ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:         pgvalue.UUID(ids.orgID),
				ProjectID:     pgvalue.UUID(ids.projectID),
				EnvironmentID: pgvalue.UUID(ids.environmentID),
				WorkspaceID:   pgvalue.UUID(ids.workspaceID),
				Priority:      int32(i),
				Request:       []byte(fmt.Sprintf(`{"source":"task_start","call":%d}`, i)),
			})
			if err != nil {
				errs <- err
				return
			}
			results <- row
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ensure failed: %v", err)
	}
	var first pgtype.UUID
	count := 0
	insertedCount := 0
	for row := range results {
		if count == 0 {
			first = row.ID
		} else if row.ID != first {
			t.Fatalf("concurrent ensure returned id = %s, want %s", pgvalue.MustUUIDValue(row.ID), pgvalue.MustUUIDValue(first))
		}
		if row.Inserted {
			insertedCount++
		}
		count++
	}
	if count != callers {
		t.Fatalf("ensure results = %d, want %d", count, callers)
	}
	if insertedCount != 1 {
		t.Fatalf("inserted ensure results = %d, want 1", insertedCount)
	}
	var activeCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
		   AND state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
	`, ids.orgID, ids.workspaceID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active materializations = %d, want 1", activeCount)
	}
}

func TestEnsureWorkspaceMaterializationRequestedRejectsNonRunnableActiveMaterialization(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)

	requested, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      1,
		Request:       []byte(`{"source":"task_start","run_id":"first"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []db.WorkspaceMaterializationState{
		db.WorkspaceMaterializationStatePausing,
		db.WorkspaceMaterializationStatePaused,
		db.WorkspaceMaterializationStateCapturing,
		db.WorkspaceMaterializationStateStopping,
	} {
		if _, err := pool.Exec(ctx, `
			UPDATE workspace_materializations
			   SET state = $3
			 WHERE org_id = $1
			   AND id = $2
		`, ids.orgID, pgvalue.MustUUIDValue(requested.ID), state); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE workspaces
			   SET desired_state = 'stopped'
			 WHERE org_id = $1
			   AND id = $2
		`, ids.orgID, ids.workspaceID); err != nil {
			t.Fatal(err)
		}
		prerequisites, err := queries.GetWorkspaceMaterializationPrerequisites(ctx, db.GetWorkspaceMaterializationPrerequisitesParams{
			OrgID:         pgvalue.UUID(ids.orgID),
			ProjectID:     pgvalue.UUID(ids.projectID),
			EnvironmentID: pgvalue.UUID(ids.environmentID),
			WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !prerequisites.ActiveMaterializationState.Valid ||
			prerequisites.ActiveMaterializationState.WorkspaceMaterializationState != state {
			t.Fatalf("active materialization state = %+v, want %s", prerequisites.ActiveMaterializationState, state)
		}

		_, err = queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         pgvalue.UUID(ids.orgID),
			ProjectID:     pgvalue.UUID(ids.projectID),
			EnvironmentID: pgvalue.UUID(ids.environmentID),
			WorkspaceID:   pgvalue.UUID(ids.workspaceID),
			Priority:      1,
			Request:       []byte(`{"source":"task_start","run_id":"second"}`),
		})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("non-runnable active ensure for %s err = %v, want no rows", state, err)
		}
		assertWorkspaceDesiredState(t, ctx, pool, ids, "stopped")
	}
	var materializations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&materializations); err != nil {
		t.Fatal(err)
	}
	if materializations != 1 {
		t.Fatalf("materializations = %d, want existing non-runnable only", materializations)
	}
}

func TestEnsureWorkspaceMaterializationRequestedRejectsCorruptWorkspaceArtifact(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)

	if _, err := pool.Exec(ctx, `
		UPDATE artifacts
		   SET media_type = 'application/octet-stream'
		 WHERE id = (
		       SELECT workspace_versions.artifact_id
		         FROM workspaces
		         JOIN workspace_versions
		           ON workspace_versions.org_id = workspaces.org_id
		          AND workspace_versions.project_id = workspaces.project_id
		          AND workspace_versions.environment_id = workspaces.environment_id
		          AND workspace_versions.workspace_id = workspaces.id
		          AND workspace_versions.id = workspaces.current_version_id
		        WHERE workspaces.org_id = $1
		          AND workspaces.id = $2
		 )
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Priority:      1,
		Request:       []byte(`{"source":"task_start"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("corrupt workspace artifact ensure err = %v, want no rows", err)
	}
	var materializations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&materializations); err != nil {
		t.Fatal(err)
	}
	if materializations != 0 {
		t.Fatalf("corrupt artifact created materializations = %d", materializations)
	}
}

func TestDeploymentSandboxRequiresImageArtifactAuthority(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)

	_, err := pool.Exec(ctx, `
		INSERT INTO deployment_sandboxes (
			id, org_id, project_id, environment_id, deployment_id, sandbox_id,
			image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, filesystem_format,
			contract_version, fingerprint
		)
		VALUES ($1, $2, $3, $4, $5, 'missing-image-artifact',
			'oci-tar', 'sha256:rootfs', $6, 'oci-tar',
			'/workspace', 'test', 'guestd-test', 'adapter-test', 'tar', 1, 'missing-image-artifact')
	`, uuid.Must(uuid.NewV7()), ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, testDigest("missing-image"))
	if err == nil {
		t.Fatal("deployment_sandbox without image_artifact_id should fail")
	}
}

func TestSandboxImageArtifactDigestIntegrity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	imageArtifactID, _ := seedSandboxImageArtifact(t, ctx, pool, ids)
	mismatchedImageDigest := testDigest("mismatched-sandbox-image")

	_, err := pool.Exec(ctx, `
		INSERT INTO deployment_sandboxes (
			id, org_id, project_id, environment_id, deployment_id, sandbox_id,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, filesystem_format,
			contract_version, fingerprint
		)
		VALUES ($1, $2, $3, $4, $5, 'mismatched-image-artifact',
			$6, 'oci-tar', 'sha256:rootfs', $7, 'oci-tar',
			'/workspace', 'test', 'guestd-test', 'adapter-test', 'tar', 1, 'mismatched-image-artifact')
	`, uuid.Must(uuid.NewV7()), ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, imageArtifactID, mismatchedImageDigest)
	if err == nil {
		t.Fatal("deployment_sandbox image_artifact_id/image_digest mismatch should fail")
	}
}

func TestWorkspaceMaterializationClaimIncludesSandboxAndWorkspaceArtifacts(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	workerID := seedMaterializationWorker(t, ctx, pool)
	queries := db.New(pool)
	rootfsDigest := deploymentSandboxRootfsDigest(t, ctx, pool, ids)
	imageDigest := deploymentSandboxImageDigest(t, ctx, pool, ids)
	var taskSessionsBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_sessions WHERE org_id = $1`, ids.orgID).Scan(&taskSessionsBefore); err != nil {
		t.Fatal(err)
	}

	_, err := requestWorkspaceMaterializationForTest(ctx, queries, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(workerID),
		ReservationToken:        "artifact-claim-token",
		ReservationExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		GuestdChannelTokenHash:  "channel-token-hash",
		RuntimeID:               "runtime-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ImageArtifactDigest != imageDigest || claimed.ImageArtifactMediaType != api.SandboxImageArtifactMediaType || claimed.ImageArtifactFormat != "oci-tar" {
		t.Fatalf("sandbox image artifact = digest %q media %q format %q", claimed.ImageArtifactDigest, claimed.ImageArtifactMediaType, claimed.ImageArtifactFormat)
	}
	if claimed.WorkspaceMountPath != "/workspace" || claimed.WorkspaceArtifactMediaType != workspace.ArtifactMediaType || claimed.WorkspaceArtifactEncoding != workspace.ArtifactEncoding || !claimed.BaseVersionID.Valid {
		t.Fatalf("workspace artifact/mount = base %v digest %q media %q encoding %q mount %q", claimed.BaseVersionID, claimed.WorkspaceArtifactDigest, claimed.WorkspaceArtifactMediaType, claimed.WorkspaceArtifactEncoding, claimed.WorkspaceMountPath)
	}
	var taskSessionsAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_sessions WHERE org_id = $1`, ids.orgID).Scan(&taskSessionsAfter); err != nil {
		t.Fatal(err)
	}
	if taskSessionsAfter != taskSessionsBefore {
		t.Fatalf("direct materialization created task sessions: before=%d after=%d", taskSessionsBefore, taskSessionsAfter)
	}
}

func TestWorkspaceMaterializationRejectsCorruptWorkspaceArtifact(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	if _, err := pool.Exec(ctx, `
		UPDATE artifacts
		   SET media_type = 'application/octet-stream'
		 WHERE id = (
		       SELECT workspace_versions.artifact_id
		         FROM workspaces
		         JOIN workspace_versions
		           ON workspace_versions.org_id = workspaces.org_id
		          AND workspace_versions.project_id = workspaces.project_id
		          AND workspace_versions.environment_id = workspaces.environment_id
		          AND workspace_versions.workspace_id = workspaces.id
		          AND workspace_versions.id = workspaces.current_version_id
		        WHERE workspaces.org_id = $1
		          AND workspaces.id = $2
		 )
	`, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	_, err := requestWorkspaceMaterializationForTest(ctx, queries, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("corrupt workspace artifact request err = %v, want no rows", err)
	}
	var materializations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&materializations); err != nil {
		t.Fatal(err)
	}
	if materializations != 0 {
		t.Fatalf("corrupt artifact created materializations = %d", materializations)
	}
}

func TestWorkspaceMaterializationStaleHeartbeatMarksLost(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	workerID := seedMaterializationWorker(t, ctx, pool)
	queries := db.New(pool)
	rootfsDigest := deploymentSandboxRootfsDigest(t, ctx, pool, ids)
	requested, err := requestWorkspaceMaterializationForTest(ctx, queries, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(workerID),
		ReservationToken:        "stale-token",
		ReservationExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		GuestdChannelTokenHash:  "channel-token-hash",
		RuntimeID:               "runtime-test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWorkspaceMaterializationRunning(ctx, db.MarkWorkspaceMaterializationRunningParams{
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		OrgID:                pgvalue.UUID(ids.orgID),
		ID:                   requested.ID,
		WorkerInstanceID:     pgvalue.UUID(workerID),
		ReservationToken:     "stale-token",
	}); err != nil {
		t.Fatal(err)
	}
	operationExecID, operationLease := seedWorkspaceExecWithActiveWriteLease(t, ctx, pool, ids, requested)
	operation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  requested.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         operationExecID,
		RequestFingerprint: "test-start-exec-dispatch",
		WriteLeaseID:       operationLease.ID,
		FencingToken:       operationLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_materializations SET last_heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, requested.ID); err != nil {
		t.Fatal(err)
	}
	lost, err := queries.MarkStaleWorkspaceMaterializationsLost(ctx, pgvalue.Timestamptz(time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0].ID != requested.ID || lost[0].State != db.WorkspaceMaterializationStateLost {
		t.Fatalf("lost materializations = %+v", lost)
	}
	lostOperation, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lostOperation.State != db.WorkspaceMaterializationOperationStateLost {
		t.Fatalf("lost operation state = %s", lostOperation.State)
	}
}

func TestWorkspaceMaterializationTerminalTransitionsLoseLivePrimitivesAndReleaseLeases(t *testing.T) {
	for _, tc := range []struct {
		name          string
		wantExecState db.WorkspaceExecState
		wantPtyState  db.WorkspacePtyState
		transition    func(context.Context, *testing.T, *pgxpool.Pool, *db.Queries, waitpointTokenIntegrationIDs, db.WorkspaceMaterialization)
	}{
		{
			name:          "stop",
			wantExecState: db.WorkspaceExecStateTerminated,
			wantPtyState:  db.WorkspacePtyStateClosed,
			transition: func(ctx context.Context, t *testing.T, _ *pgxpool.Pool, queries *db.Queries, ids waitpointTokenIntegrationIDs, materialization db.WorkspaceMaterialization) {
				t.Helper()
				if _, err := queries.StopWorkspaceMaterialization(ctx, db.StopWorkspaceMaterializationParams{
					OrgID:            pgvalue.UUID(ids.orgID),
					ID:               materialization.ID,
					WorkerInstanceID: materialization.WorkerInstanceID,
					ReservationToken: materialization.ReservationToken,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:          "fail",
			wantExecState: db.WorkspaceExecStateLost,
			wantPtyState:  db.WorkspacePtyStateLost,
			transition: func(ctx context.Context, t *testing.T, _ *pgxpool.Pool, queries *db.Queries, ids waitpointTokenIntegrationIDs, materialization db.WorkspaceMaterialization) {
				t.Helper()
				if _, err := queries.FailWorkspaceMaterialization(ctx, db.FailWorkspaceMaterializationParams{
					OrgID:            pgvalue.UUID(ids.orgID),
					ID:               materialization.ID,
					WorkerInstanceID: materialization.WorkerInstanceID,
					ReservationToken: materialization.ReservationToken,
					Error:            []byte(`{"code":"test_failure"}`),
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:          "lost",
			wantExecState: db.WorkspaceExecStateLost,
			wantPtyState:  db.WorkspacePtyStateLost,
			transition: func(ctx context.Context, t *testing.T, pool *pgxpool.Pool, queries *db.Queries, ids waitpointTokenIntegrationIDs, materialization db.WorkspaceMaterialization) {
				t.Helper()
				if _, err := pool.Exec(ctx, `
					UPDATE workspace_materializations
					   SET last_heartbeat_at = now() - interval '10 minutes'
					 WHERE org_id = $1
					   AND id = $2
				`, ids.orgID, materialization.ID); err != nil {
					t.Fatal(err)
				}
				lost, err := queries.MarkStaleWorkspaceMaterializationsLost(ctx, pgvalue.Timestamptz(time.Now().Add(-time.Minute)))
				if err != nil {
					t.Fatal(err)
				}
				if len(lost) != 1 || lost[0].ID != materialization.ID {
					t.Fatalf("lost materializations = %+v", lost)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, initialLeaseState := range []db.WorkspaceLeaseState{db.WorkspaceLeaseStateActive, db.WorkspaceLeaseStateReleasing} {
				t.Run(string(initialLeaseState), func(t *testing.T) {
					ctx := context.Background()
					pool := newIntegrationDB(t, ctx)
					ids := seedWaitpointTokenIntegration(t, ctx, pool)
					queries := db.New(pool)
					materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
					execID, ptyID, portID, leaseID := seedLiveWorkspacePrimitiveRows(t, ctx, pool, ids, materialization)
					if initialLeaseState != db.WorkspaceLeaseStateActive {
						if _, err := pool.Exec(ctx, `
							UPDATE workspace_leases
							   SET state = $1
							 WHERE org_id = $2
							   AND id = $3
						`, initialLeaseState, ids.orgID, leaseID); err != nil {
							t.Fatal(err)
						}
					}

					tc.transition(ctx, t, pool, queries, ids, materialization)

					var execState db.WorkspaceExecState
					if err := pool.QueryRow(ctx, `
						SELECT state
						  FROM workspace_execs
						 WHERE org_id = $1
						   AND id = $2
					`, ids.orgID, execID).Scan(&execState); err != nil {
						t.Fatal(err)
					}
					if execState != tc.wantExecState {
						t.Fatalf("exec state = %s, want %s", execState, tc.wantExecState)
					}
					var ptyState db.WorkspacePtyState
					if err := pool.QueryRow(ctx, `
						SELECT state
						  FROM workspace_pty_sessions
						 WHERE org_id = $1
						   AND id = $2
					`, ids.orgID, ptyID).Scan(&ptyState); err != nil {
						t.Fatal(err)
					}
					if ptyState != tc.wantPtyState {
						t.Fatalf("pty state = %s, want %s", ptyState, tc.wantPtyState)
					}
					var portState db.WorkspacePortState
					if err := pool.QueryRow(ctx, `
						SELECT state
						  FROM workspace_ports
						 WHERE org_id = $1
						   AND id = $2
					`, ids.orgID, portID).Scan(&portState); err != nil {
						t.Fatal(err)
					}
					if portState != db.WorkspacePortStateClosed {
						t.Fatalf("port state = %s, want closed", portState)
					}
					var leaseState db.WorkspaceLeaseState
					if err := pool.QueryRow(ctx, `
						SELECT state
						  FROM workspace_leases
						 WHERE org_id = $1
						   AND id = $2
					`, ids.orgID, leaseID).Scan(&leaseState); err != nil {
						t.Fatal(err)
					}
					if leaseState != db.WorkspaceLeaseStateReleased {
						t.Fatalf("lease state = %s, want released", leaseState)
					}
				})
			}
		})
	}
}

func TestWorkspaceMaterializationStaleHeartbeatKeepsActiveRunMaterialization(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	attemptID := uuid.Must(uuid.NewV7())
	runLeaseID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (
			runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ('runtime-test', 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
		ON CONFLICT (runtime_id) DO NOTHING
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status, started_at)
		VALUES ($1, $2, $3, 1, 'running', now())
	`, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, run_id, attempt_id, worker_instance_id, worker_group_id,
			dispatch_message_id, dispatch_lease_id, dispatch_attempt, status,
			lease_expires_at, runtime_id, trace_id, span_id, parent_span_id, traceparent
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'dispatch-message', 'dispatch-lease', 1, 'running',
			now() + interval '1 hour', 'runtime-test',
			'11111111111111111111111111111111', '3333333333333333', '2222222222222222',
			'00-11111111111111111111111111111111-3333333333333333-01')
	`, runLeaseID, ids.orgID, ids.runID, attemptID, workerID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET workspace_materialization_id = $1,
		       current_attempt_id = $2,
		       current_run_lease_id = $3,
		       current_attempt_number = 1,
		       status = 'running',
		       execution_status = 'executing'
		 WHERE org_id = $4
		   AND id = $5
	`, pgvalue.MustUUIDValue(materialization.ID), attemptID, runLeaseID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET last_heartbeat_at = now() - interval '10 minutes'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, materialization.ID); err != nil {
		t.Fatal(err)
	}
	lost, err := queries.MarkStaleWorkspaceMaterializationsLost(ctx, pgvalue.Timestamptz(time.Now().Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 0 {
		t.Fatalf("active run materialization was reaped: %+v", lost)
	}
	var state db.WorkspaceMaterializationState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, materialization.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkspaceMaterializationStateRunning {
		t.Fatalf("materialization state = %s", state)
	}
}

func TestWorkspaceMaterializationExpiredClaimIsReclaimed(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	workerID := seedMaterializationWorker(t, ctx, pool)
	secondWorkerID := seedMaterializationWorker(t, ctx, pool)
	queries := db.New(pool)
	rootfsDigest := deploymentSandboxRootfsDigest(t, ctx, pool, ids)

	requested, err := requestWorkspaceMaterializationForTest(ctx, queries, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(workerID),
		ReservationToken:        "expired-materialization-token",
		ReservationExpiresAt:    pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		GuestdChannelTokenHash:  "expired-channel-token-hash",
		RuntimeID:               "runtime-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(secondWorkerID),
		ReservationToken:        "fresh-materialization-token",
		ReservationExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		GuestdChannelTokenHash:  "fresh-channel-token-hash",
		RuntimeID:               "runtime-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != requested.ID || second.ID != requested.ID || second.WorkerInstanceID != pgvalue.UUID(secondWorkerID) || second.ClaimAttempt != first.ClaimAttempt+1 {
		t.Fatalf("expired claim was not reclaimed first=%+v second=%+v requested=%+v", first, second, requested)
	}
	if _, err := queries.RenewWorkspaceMaterialization(ctx, db.RenewWorkspaceMaterializationParams{
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		OrgID:                pgvalue.UUID(ids.orgID),
		ID:                   requested.ID,
		WorkerInstanceID:     pgvalue.UUID(workerID),
		ReservationToken:     "expired-materialization-token",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired reservation renew err = %v, want no rows", err)
	}
}

func TestWorkspaceMaterializationExpiredClaimWithLiveWriteLeaseIsNotReclaimed(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	workerID := seedMaterializationWorker(t, ctx, pool)
	secondWorkerID := seedMaterializationWorker(t, ctx, pool)
	queries := db.New(pool)
	rootfsDigest := deploymentSandboxRootfsDigest(t, ctx, pool, ids)

	requested, err := requestWorkspaceMaterializationForTest(ctx, queries, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(workerID),
		ReservationToken:        "expired-materialization-token",
		ReservationExpiresAt:    pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		GuestdChannelTokenHash:  "expired-channel-token-hash",
		RuntimeID:               "runtime-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != requested.ID {
		t.Fatalf("claimed materialization id = %s, want %s", first.ID, requested.ID)
	}
	seedWorkspaceWriteLease(t, ctx, pool, ids, workerID)

	if _, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(secondWorkerID),
		ReservationToken:        "fresh-materialization-token",
		ReservationExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		GuestdChannelTokenHash:  "fresh-channel-token-hash",
		RuntimeID:               "runtime-test",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim with live write lease err = %v, want no rows", err)
	}
}

func TestWorkspaceMaterializationOperationClaimAndComplete(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)
	otherWorkerID := seedMaterializationWorker(t, ctx, pool)

	operationExecID, operationLease := seedWorkspaceExecWithActiveWriteLease(t, ctx, pool, ids, materialization)
	requested, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         operationExecID,
		RequestFingerprint: "test-start-exec-dispatch",
		WriteLeaseID:       operationLease.ID,
		FencingToken:       operationLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Priority:           10,
		Request:            []byte(`{"check":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if requested.State != db.WorkspaceMaterializationOperationStateQueued {
		t.Fatalf("queued state = %s", requested.State)
	}
	if requested.FencingGeneration != materialization.FencingGeneration {
		t.Fatalf("operation fencing_generation = %d, want materialization generation %d", requested.FencingGeneration, materialization.FencingGeneration)
	}

	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(otherWorkerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "wrong-worker-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong worker claim err = %v, want no rows", err)
	}
	claimed, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "claim-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != requested.ID || claimed.State != db.WorkspaceMaterializationOperationStateClaimed || claimed.ClaimedByWorkerInstanceID != pgvalue.UUID(workerID) {
		t.Fatalf("claimed operation = %+v", claimed)
	}
	running, err := queries.StartWorkspaceMaterializationOperation(ctx, db.StartWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "claim-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if running.State != db.WorkspaceMaterializationOperationStateRunning {
		t.Fatalf("running state = %s", running.State)
	}
	replayedStart, err := queries.StartWorkspaceMaterializationOperation(ctx, db.StartWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "claim-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayedStart.State != db.WorkspaceMaterializationOperationStateRunning || replayedStart.ID != requested.ID {
		t.Fatalf("replayed start = %+v", replayedStart)
	}
	if _, err := queries.CompleteWorkspaceMaterializationOperation(ctx, db.CompleteWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "stale-token",
		Result:           []byte(`{"ok":false}`),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale token complete err = %v, want no rows", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materialization_operations
		   SET claim_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, requested.ID); err != nil {
		t.Fatal(err)
	}
	completed, err := queries.CompleteWorkspaceMaterializationOperation(ctx, db.CompleteWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "claim-token",
		Result:           []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != db.WorkspaceMaterializationOperationStateCompleted || string(completed.Result) != `{"ok": true}` {
		t.Fatalf("completed operation = state:%s result:%s", completed.State, completed.Result)
	}
	replayed, err := queries.CompleteWorkspaceMaterializationOperation(ctx, db.CompleteWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "claim-token",
		Result:           []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State != db.WorkspaceMaterializationOperationStateCompleted || string(replayed.Result) != `{"ok": true}` {
		t.Fatalf("replayed completion = state:%s result:%s", replayed.State, replayed.Result)
	}
	if _, err := queries.CompleteWorkspaceMaterializationOperation(ctx, db.CompleteWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "claim-token",
		Result:           []byte(`{"ok":false}`),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("different replay completion err = %v, want no rows", err)
	}
}

func TestWorkspaceMaterializationOperationRequiresActiveWriteLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)
	leaseID := seedWorkspaceWriteLease(t, ctx, pool, ids, workerID)
	fencingToken := "fence-" + shortUUID(leaseID)

	requested, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RequestFingerprint: "test-write-lease-dispatch",
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		WriteLeaseID:       pgvalue.UUID(leaseID),
		FencingToken:       fencingToken,
		Request:            []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if requested.FencingGeneration != 1 || requested.WriteLeaseID != pgvalue.UUID(leaseID) || requested.FencingToken != fencingToken {
		t.Fatalf("operation lease fence = generation %d lease %v token %q", requested.FencingGeneration, requested.WriteLeaseID, requested.FencingToken)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_leases
		   SET state = 'released',
		       released_at = now(),
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, leaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "claim-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("claim after released write lease err = %v, want no rows", err)
	}
}

func TestWorkspaceMaterializationOperationExpiredClaimReclaimsForSameMaterialization(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)

	operationExecID, operationLease := seedWorkspaceExecWithActiveWriteLease(t, ctx, pool, ids, materialization)
	requested, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         operationExecID,
		RequestFingerprint: "test-start-exec-dispatch",
		WriteLeaseID:       operationLease.ID,
		FencingToken:       operationLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "expired-claim-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "fresh-claim-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != requested.ID || second.ID != requested.ID || second.ClaimToken != "fresh-claim-token" || second.ClaimAttempt != first.ClaimAttempt+1 {
		t.Fatalf("reclaimed operation first=%+v second=%+v requested=%+v", first, second, requested)
	}
	if _, err := queries.StartWorkspaceMaterializationOperation(ctx, db.StartWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "fresh-claim-token",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CompleteWorkspaceMaterializationOperation(ctx, db.CompleteWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "expired-claim-token",
		Result:           []byte(`{"ok":false}`),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired token complete err = %v, want no rows", err)
	}
	completed, err := queries.CompleteWorkspaceMaterializationOperation(ctx, db.CompleteWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       "fresh-claim-token",
		Result:           []byte(`{"ok":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != db.WorkspaceMaterializationOperationStateCompleted {
		t.Fatalf("completed state = %s", completed.State)
	}
}

func TestWorkspaceMaterializationOperationRunningIsTerminallyCleanedUp(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)

	operationExecID, operationLease := seedWorkspaceExecWithActiveWriteLease(t, ctx, pool, ids, materialization)
	requested, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         operationExecID,
		RequestFingerprint: "test-start-exec-dispatch",
		WriteLeaseID:       operationLease.ID,
		FencingToken:       operationLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "running-terminal-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartWorkspaceMaterializationOperation(ctx, db.StartWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       claimed.ClaimToken,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.FailWorkspaceMaterialization(ctx, db.FailWorkspaceMaterializationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               materialization.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ReservationToken: materialization.ReservationToken,
		Error:            []byte(`{"code":"test_failure"}`),
	}); err != nil {
		t.Fatal(err)
	}
	cleaned, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            requested.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.State != db.WorkspaceMaterializationOperationStateLost {
		t.Fatalf("running operation was not lost after materialization failure: %+v", cleaned)
	}
}

func TestWorkspaceMaterializationOperationRunningExpires(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)

	operationExecID, operationLease := seedWorkspaceExecWithActiveWriteLease(t, ctx, pool, ids, materialization)
	requested, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         operationExecID,
		RequestFingerprint: "test-start-exec-dispatch",
		WriteLeaseID:       operationLease.ID,
		FencingToken:       operationLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "running-expired-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartWorkspaceMaterializationOperation(ctx, db.StartWorkspaceMaterializationOperationParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ID:               requested.ID,
		WorkerInstanceID: pgvalue.UUID(workerID),
		ClaimToken:       claimed.ClaimToken,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materialization_operations
		   SET operation_expires_at = now() - interval '1 second'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, requested.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "after-expiry-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired running operation claim err = %v, want no rows", err)
	}
	expired, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            requested.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != db.WorkspaceMaterializationOperationStateExpired {
		t.Fatalf("running operation expiry state = %s", expired.State)
	}
}

func TestExpiredStartExecOperationFailsPendingExecAndReleasesWriteLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)
	execID := pgvalue.UUID(seedWorkspaceExec(t, ctx, pool, ids, materialization))
	execLease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       execID,
		OwnerPtySessionID: pgtype.UUID{},
		FencingToken:      "expired-exec-fence",
		HeartbeatToken:    "expired-exec-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	execRow, err := queries.BindWorkspaceExecMaterialization(ctx, db.BindWorkspaceExecMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      execLease.ID,
		State:             db.WorkspaceExecStateQueued,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                execID,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         execRow.ID,
		RequestFingerprint: "expired-start-exec",
		WriteLeaseID:       execLease.ID,
		FencingToken:       execLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		Request:            []byte(`{"exec_id":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "expired-start-exec-claim",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired start exec claim err = %v, want no rows", err)
	}
	expired, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != db.WorkspaceMaterializationOperationStateExpired {
		t.Fatalf("operation state = %s, want expired", expired.State)
	}
	var execState db.WorkspaceExecState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM workspace_execs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(execRow.ID)).Scan(&execState); err != nil {
		t.Fatal(err)
	}
	if execState != db.WorkspaceExecStateFailed {
		t.Fatalf("exec state = %s, want failed", execState)
	}
	var leaseState db.WorkspaceLeaseState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM workspace_leases
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(execLease.ID)).Scan(&leaseState); err != nil {
		t.Fatal(err)
	}
	if leaseState != db.WorkspaceLeaseStateReleased {
		t.Fatalf("lease state = %s, want released", leaseState)
	}
}

func TestExhaustedCreatePtyOperationFailsCreatingPtyAndReleasesWriteLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID := seedMaterializationWorker(t, ctx, pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)

	pty, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	ptyLease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgtype.UUID{},
		OwnerPtySessionID: pty.ID,
		FencingToken:      "exhausted-create-pty-fence",
		HeartbeatToken:    "exhausted-create-pty-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	pty, err = queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      ptyLease.ID,
		State:             db.WorkspacePtyStateCreating,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pty.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindCreatePty,
		ResourceKind:       db.WorkspaceResourceKindWorkspacePty,
		ResourceID:         pty.ID,
		RequestFingerprint: "exhausted-create-pty",
		WriteLeaseID:       ptyLease.ID,
		FencingToken:       ptyLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{"pty_id":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "exhausted-create-pty-claim",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		MaxClaimAttempts:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != operation.ID {
		t.Fatalf("claimed operation = %v, want %v", claimed.ID, operation.ID)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "exhausted-create-pty-second-claim",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exhausted create pty claim err = %v, want no rows", err)
	}
	exhausted, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.State != db.WorkspaceMaterializationOperationStateLost {
		t.Fatalf("operation state = %s, want lost", exhausted.State)
	}
	var ptyState db.WorkspacePtyState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM workspace_pty_sessions
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(pty.ID)).Scan(&ptyState); err != nil {
		t.Fatal(err)
	}
	if ptyState != db.WorkspacePtyStateFailed {
		t.Fatalf("pty state = %s, want failed", ptyState)
	}
	var leaseState db.WorkspaceLeaseState
	if err := pool.QueryRow(ctx, `
		SELECT state
		  FROM workspace_leases
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(ptyLease.ID)).Scan(&leaseState); err != nil {
		t.Fatal(err)
	}
	if leaseState != db.WorkspaceLeaseStateReleased {
		t.Fatalf("lease state = %s, want released", leaseState)
	}
}

func TestExpiredResizePtyOperationRollsBackOpenPtyAndKeepsWriteLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)
	ptyID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   ptyID,
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	ptyLease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgtype.UUID{},
		OwnerPtySessionID: ptyID,
		FencingToken:      "expired-resize-pty-fence",
		HeartbeatToken:    "expired-resize-pty-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      ptyLease.ID,
		State:             db.WorkspacePtyStateOpen,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                ptyID,
	}); err != nil {
		t.Fatal(err)
	}
	pty, err := queries.ResizeWorkspacePtySession(ctx, db.ResizeWorkspacePtySessionParams{
		Cols:          pgtype.Int4{Int32: 120, Valid: true},
		Rows:          pgtype.Int4{Int32: 40, Valid: true},
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pty.State != db.WorkspacePtyStateResizing || pty.Cols != 80 || pty.Rows != 24 || !pty.ResizeCols.Valid || pty.ResizeCols.Int32 != 120 || !pty.ResizeRows.Valid || pty.ResizeRows.Int32 != 40 {
		t.Fatalf("resize request pty = %+v", pty)
	}
	operation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindResizePty,
		ResourceKind:       db.WorkspaceResourceKindWorkspacePty,
		ResourceID:         pty.ID,
		RequestFingerprint: "expired-resize-pty",
		WriteLeaseID:       ptyLease.ID,
		FencingToken:       ptyLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		Request:            []byte(`{"pty_id":"test","cols":120,"rows":40}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "expired-resize-pty-claim",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired resize pty claim err = %v, want no rows", err)
	}
	expired, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != db.WorkspaceMaterializationOperationStateExpired {
		t.Fatalf("operation state = %s, want expired", expired.State)
	}
	pty, err = queries.GetWorkspacePtySession(ctx, db.GetWorkspacePtySessionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pty.State != db.WorkspacePtyStateOpen || pty.Cols != 80 || pty.Rows != 24 || pty.ResizeCols.Valid || pty.ResizeRows.Valid {
		t.Fatalf("rolled back pty = %+v", pty)
	}
	lease, err := queries.GetWorkspaceLease(ctx, db.GetWorkspaceLeaseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyLease.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.State != db.WorkspaceLeaseStateActive {
		t.Fatalf("lease state = %s, want active", lease.State)
	}
}

func TestExhaustedClosePtyOperationRollsBackOpenPtyAndKeepsWriteLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	workerID := seedMaterializationWorker(t, ctx, pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	ptyID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   ptyID,
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	ptyLease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgtype.UUID{},
		OwnerPtySessionID: ptyID,
		FencingToken:      "exhausted-close-pty-fence",
		HeartbeatToken:    "exhausted-close-pty-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      ptyLease.ID,
		State:             db.WorkspacePtyStateOpen,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                ptyID,
	}); err != nil {
		t.Fatal(err)
	}
	pty, err := queries.RequestWorkspacePtyClose(ctx, db.RequestWorkspacePtyCloseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pty.State != db.WorkspacePtyStateClosing {
		t.Fatalf("close request pty = %+v", pty)
	}
	operation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindClosePty,
		ResourceKind:       db.WorkspaceResourceKindWorkspacePty,
		ResourceID:         pty.ID,
		RequestFingerprint: "exhausted-close-pty",
		WriteLeaseID:       ptyLease.ID,
		FencingToken:       ptyLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{"pty_id":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "exhausted-close-pty-claim",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		MaxClaimAttempts:  1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "exhausted-close-pty-second-claim",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exhausted close pty claim err = %v, want no rows", err)
	}
	exhausted, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.State != db.WorkspaceMaterializationOperationStateLost {
		t.Fatalf("operation state = %s, want lost", exhausted.State)
	}
	pty, err = queries.GetWorkspacePtySession(ctx, db.GetWorkspacePtySessionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pty.State != db.WorkspacePtyStateOpen || pty.ResizeCols.Valid || pty.ResizeRows.Valid {
		t.Fatalf("rolled back pty = %+v", pty)
	}
	lease, err := queries.GetWorkspaceLease(ctx, db.GetWorkspaceLeaseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyLease.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.State != db.WorkspaceLeaseStateActive {
		t.Fatalf("lease state = %s, want active", lease.State)
	}
}

func TestWorkspaceMaterializationOperationsAllowCloseWhileResizeActive(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	ptyID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   ptyID,
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgtype.UUID{},
		OwnerPtySessionID: ptyID,
		FencingToken:      "serialize-pty-control-fence",
		HeartbeatToken:    "serialize-pty-control-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      lease.ID,
		State:             db.WorkspacePtyStateOpen,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                ptyID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResizeWorkspacePtySession(ctx, db.ResizeWorkspacePtySessionParams{
		Cols:          pgtype.Int4{Int32: 120, Valid: true},
		Rows:          pgtype.Int4{Int32: 40, Valid: true},
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	}); err != nil {
		t.Fatal(err)
	}
	resizeOperation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindResizePty,
		ResourceKind:       db.WorkspaceResourceKindWorkspacePty,
		ResourceID:         ptyID,
		RequestFingerprint: "serialize-resize-pty",
		WriteLeaseID:       lease.ID,
		FencingToken:       lease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{"pty_id":"test","cols":120,"rows":40}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resizeOperation.OperationKind != db.WorkspaceMaterializationOperationKindResizePty {
		t.Fatalf("resize operation kind = %s", resizeOperation.OperationKind)
	}
	closeOperation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindClosePty,
		ResourceKind:       db.WorkspaceResourceKindWorkspacePty,
		ResourceID:         ptyID,
		RequestFingerprint: "serialize-close-pty",
		WriteLeaseID:       lease.ID,
		FencingToken:       lease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{"pty_id":"test"}`),
	})
	if err != nil {
		t.Fatalf("close while resize active err = %v", err)
	}
	if closeOperation.OperationKind != db.WorkspaceMaterializationOperationKindClosePty {
		t.Fatalf("close operation kind = %s", closeOperation.OperationKind)
	}
	pty, err := queries.RequestWorkspacePtyClose(ctx, db.RequestWorkspacePtyCloseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pty.State != db.WorkspacePtyStateClosing || !pty.ResizeCols.Valid || pty.ResizeCols.Int32 != 120 || !pty.ResizeRows.Valid || pty.ResizeRows.Int32 != 40 {
		t.Fatalf("close should preserve pending resize target, got %+v", pty)
	}
	pty, err = queries.MarkWorkspacePtyResizeApplied(ctx, db.MarkWorkspacePtyResizeAppliedParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                ptyID,
		MaterializationID: materialization.ID,
		Cols:              pgtype.Int4{Int32: 120, Valid: true},
		Rows:              pgtype.Int4{Int32: 40, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pty.State != db.WorkspacePtyStateClosing || pty.Cols != 120 || pty.Rows != 40 || pty.ResizeCols.Valid || pty.ResizeRows.Valid {
		t.Fatalf("resize-applied during close = %+v", pty)
	}
	if _, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindClosePty,
		ResourceKind:       db.WorkspaceResourceKindWorkspacePty,
		ResourceID:         ptyID,
		RequestFingerprint: "serialize-close-pty-again",
		WriteLeaseID:       lease.ID,
		FencingToken:       lease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{"pty_id":"test"}`),
	}); !isDBUniqueViolation(err) {
		t.Fatalf("duplicate close pty operation err = %v, want unique violation", err)
	}
}

func TestExpiredResizePtyOperationDoesNotRollbackClosingPty(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)
	ptyID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   ptyID,
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	lease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgtype.UUID{},
		OwnerPtySessionID: ptyID,
		FencingToken:      "expired-resize-does-not-close-fence",
		HeartbeatToken:    "expired-resize-does-not-close-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      lease.ID,
		State:             db.WorkspacePtyStateOpen,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                ptyID,
	}); err != nil {
		t.Fatal(err)
	}
	pty, err := queries.ResizeWorkspacePtySession(ctx, db.ResizeWorkspacePtySessionParams{
		Cols:          pgtype.Int4{Int32: 120, Valid: true},
		Rows:          pgtype.Int4{Int32: 40, Valid: true},
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindResizePty,
		ResourceKind:       db.WorkspaceResourceKindWorkspacePty,
		ResourceID:         pty.ID,
		RequestFingerprint: "expired-resize-does-not-close",
		WriteLeaseID:       lease.ID,
		FencingToken:       lease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		Request:            []byte(`{"pty_id":"test","cols":120,"rows":40}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_pty_sessions
		   SET state = 'closing',
		       resize_cols = NULL,
		       resize_rows = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(ptyID)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "expired-resize-does-not-close-claim",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  testWorkspaceOperationMaxClaimAttempts,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired resize pty claim err = %v, want no rows", err)
	}
	expired, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            operation.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.State != db.WorkspaceMaterializationOperationStateExpired {
		t.Fatalf("operation state = %s, want expired", expired.State)
	}
	pty, err = queries.GetWorkspacePtySession(ctx, db.GetWorkspacePtySessionParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pty.State != db.WorkspacePtyStateClosing {
		t.Fatalf("pty state = %s, want closing", pty.State)
	}
}

func TestWorkspaceMaterializationOperationKindsAreConstrained(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)

	baseSQL := `
		INSERT INTO workspace_materialization_operations (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			operation_kind, resource_kind, resource_id, request_fingerprint,
			operation_expires_at, fencing_generation
		)
		VALUES ($1, $2, $3, $4, $5, $6, %s, %s, $7, 'constraint-test', now() + interval '1 minute', 1)
	`
	resourceID := uuid.Must(uuid.NewV7())
	cases := []struct {
		name         string
		operationSQL string
		resourceSQL  string
	}{
		{name: "unknown operation kind", operationSQL: "'unknown_kind'", resourceSQL: "'workspace_exec'"},
		{name: "operation resource mismatch", operationSQL: "'start_exec'", resourceSQL: "'workspace_pty'"},
		{name: "empty resource kind", operationSQL: "'start_exec'", resourceSQL: "''"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx,
				fmt.Sprintf(baseSQL, tc.operationSQL, tc.resourceSQL),
				uuid.Must(uuid.NewV7()),
				ids.orgID,
				ids.projectID,
				ids.environmentID,
				ids.workspaceID,
				pgvalue.MustUUIDValue(materialization.ID),
				resourceID,
			)
			if err == nil {
				t.Fatalf("insert with %s succeeded, want constraint error", tc.name)
			}
		})
	}
}

func TestWorkspaceStreamWakeupFailureRetriesTerminalAndDeletesExhaustedChunks(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	resourceID := pgvalue.UUID(uuid.Must(uuid.NewV7()))

	terminal, err := queries.CreateWorkspaceStreamWakeup(ctx, db.CreateWorkspaceStreamWakeupParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		WorkspaceID:      pgvalue.UUID(ids.workspaceID),
		ResourceKind:     db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:       resourceID,
		Stream:           "stdout",
		CursorOffset:     0,
		NotificationKind: db.WorkspaceStreamNotificationKindTerminal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_stream_wakeups SET attempts = 25 WHERE id = $1`, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if err := queries.MarkWorkspaceStreamWakeupFailed(ctx, db.MarkWorkspaceStreamWakeupFailedParams{
		ID:          terminal.ID,
		MaxAttempts: 25,
		RetryAfter:  pgtype.Interval{Microseconds: int64(time.Second / time.Microsecond), Valid: true},
		LastError:   "redis unavailable",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_stream_wakeups SET locked_until = NULL WHERE id = $1`, terminal.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := queries.ClaimWorkspaceStreamWakeups(ctx, db.ClaimWorkspaceStreamWakeupsParams{
		MaxAttempts:   25,
		RowLimit:      10,
		LeaseDuration: pgtype.Interval{Microseconds: int64(time.Second / time.Microsecond), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != terminal.ID {
		t.Fatalf("terminal wakeups = %+v, want retryable terminal", rows)
	}

	chunk, err := queries.CreateWorkspaceStreamWakeup(ctx, db.CreateWorkspaceStreamWakeupParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		ProjectID:        pgvalue.UUID(ids.projectID),
		EnvironmentID:    pgvalue.UUID(ids.environmentID),
		WorkspaceID:      pgvalue.UUID(ids.workspaceID),
		ResourceKind:     db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:       resourceID,
		Stream:           "stdout",
		CursorOffset:     0,
		NotificationKind: db.WorkspaceStreamNotificationKindChunk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_stream_wakeups SET attempts = 25 WHERE id = $1`, chunk.ID); err != nil {
		t.Fatal(err)
	}
	if err := queries.MarkWorkspaceStreamWakeupFailed(ctx, db.MarkWorkspaceStreamWakeupFailedParams{
		ID:          chunk.ID,
		MaxAttempts: 25,
		RetryAfter:  pgtype.Interval{Microseconds: int64(time.Second / time.Microsecond), Valid: true},
		LastError:   "redis unavailable",
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_stream_wakeups WHERE id = $1`, chunk.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("chunk wakeup count = %d, want deleted", count)
	}
}

func TestWorkspacePortsHaveNoPublicAccessTokenColumnBeforePortTokens(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = 'workspace_ports'
		   AND column_name = 'public_access_token_id'
	`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("workspace_ports.public_access_token_id column exists before public port token authority is implemented")
	}
}

func TestStaleDirtyWorkspaceMaterializationMarksWorkspaceRecoveryRequired(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)

	if _, err := pool.Exec(ctx, `
		UPDATE workspace_materializations
		   SET dirty_generation = 1,
		       last_heartbeat_at = now() - interval '10 minutes',
		       updated_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, materialization.ID); err != nil {
		t.Fatal(err)
	}
	lost, err := queries.MarkStaleWorkspaceMaterializationsLost(ctx, pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0].ID != materialization.ID {
		t.Fatalf("lost materializations = %+v, want %v", lost, materialization.ID)
	}
	var state db.WorkspaceState
	var dirtyState db.WorkspaceDirtyState
	if err := pool.QueryRow(ctx, `
		SELECT state, dirty_state
		  FROM workspaces
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND id = $4
	`, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID).Scan(&state, &dirtyState); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkspaceStateRecoveryRequired || dirtyState != db.WorkspaceDirtyStateDirtyStateLost {
		t.Fatalf("workspace state = %s dirty_state = %s, want recovery_required/dirty_state_lost", state, dirtyState)
	}
}

func TestWorkspaceMaterializationOperationExpiredClaimExhaustsAttempts(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	workerID := pgvalue.MustUUIDValue(materialization.WorkerInstanceID)

	operationExecID, operationLease := seedWorkspaceExecWithActiveWriteLease(t, ctx, pool, ids, materialization)
	requested, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         operationExecID,
		RequestFingerprint: "test-start-exec-dispatch",
		WriteLeaseID:       operationLease.ID,
		FencingToken:       operationLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "expired-claim-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(-time.Second)),
		MaxClaimAttempts:  1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterializationOperation(ctx, db.ClaimWorkspaceMaterializationOperationParams{
		WorkerInstanceID:  pgvalue.UUID(workerID),
		OrgID:             pgvalue.UUID(ids.orgID),
		MaterializationID: materialization.ID,
		ReservationToken:  materialization.ReservationToken,
		ClaimToken:        "second-claim-token",
		ClaimExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		MaxClaimAttempts:  1,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exhausted claim err = %v, want no rows", err)
	}
	exhausted, err := queries.GetWorkspaceMaterializationOperation(ctx, db.GetWorkspaceMaterializationOperationParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            requested.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exhausted.State != db.WorkspaceMaterializationOperationStateLost {
		t.Fatalf("exhausted state = %s", exhausted.State)
	}
}

func TestWorkspacePrimitiveOperationsRequireFencedWriteLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	execID := pgvalue.UUID(seedWorkspaceExec(t, ctx, pool, ids, materialization))

	if _, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         execID,
		RequestFingerprint: "start-exec-without-lease",
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{"exec_id":"test"}`),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("StartExec without write lease err = %v, want no rows", err)
	}
	execLease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       execID,
		OwnerPtySessionID: pgtype.UUID{},
		FencingToken:      "exec-fence",
		HeartbeatToken:    "exec-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	execRow, err := queries.BindWorkspaceExecMaterialization(ctx, db.BindWorkspaceExecMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      execLease.ID,
		State:             db.WorkspaceExecStateQueued,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                execID,
	})
	if err != nil {
		t.Fatal(err)
	}
	startExec, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindStartExec,
		ResourceKind:       db.WorkspaceResourceKindWorkspaceExec,
		ResourceID:         execID,
		RequestFingerprint: "start-exec-with-lease",
		WriteLeaseID:       execLease.ID,
		FencingToken:       execLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{"exec_id":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if startExec.WriteLeaseID != execLease.ID || startExec.FencingGeneration != execLease.AcquiredFencingGeneration {
		t.Fatalf("StartExec lease = %+v generation %d, want %+v generation %d", startExec.WriteLeaseID, startExec.FencingGeneration, execLease.ID, execLease.AcquiredFencingGeneration)
	}
	if _, err := queries.MarkWorkspaceExecStarted(ctx, db.MarkWorkspaceExecStartedParams{
		ProcessID:         "wrong-materialization-process",
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                execRow.ID,
		MaterializationID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("exec started from wrong materialization err = %v, want no rows", err)
	}
	if _, err := queries.MarkWorkspaceExecStarted(ctx, db.MarkWorkspaceExecStartedParams{
		ProcessID:         "exec-process",
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                execRow.ID,
		MaterializationID: materialization.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWorkspaceExecExited(ctx, db.MarkWorkspaceExecExitedParams{
		State:             db.WorkspaceExecStateExited,
		ExitCode:          pgtype.Int4{Int32: 0, Valid: true},
		Signal:            "",
		Error:             []byte(`{}`),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                execRow.ID,
		MaterializationID: materialization.ID,
	}); err != nil {
		t.Fatal(err)
	}
	execLeaseAfter, err := queries.GetWorkspaceLease(ctx, db.GetWorkspaceLeaseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            execLease.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if execLeaseAfter.State != db.WorkspaceLeaseStateReleased {
		t.Fatalf("exec lease state = %s, want released", execLeaseAfter.State)
	}

	pty, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	})
	if err != nil {
		t.Fatal(err)
	}
	ptyLease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgtype.UUID{},
		OwnerPtySessionID: pty.ID,
		FencingToken:      "pty-fence",
		HeartbeatToken:    "pty-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	pty, err = queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      ptyLease.ID,
		State:             db.WorkspacePtyStateCreating,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pty.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	createPty, err := queries.RequestWorkspaceMaterializationOperation(ctx, db.RequestWorkspaceMaterializationOperationParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		WorkspaceID:        pgvalue.UUID(ids.workspaceID),
		MaterializationID:  materialization.ID,
		OperationKind:      db.WorkspaceMaterializationOperationKindCreatePty,
		ResourceKind:       db.WorkspaceResourceKindWorkspacePty,
		ResourceID:         pty.ID,
		RequestFingerprint: "create-pty-with-lease",
		WriteLeaseID:       ptyLease.ID,
		FencingToken:       ptyLease.FencingToken,
		OperationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		Request:            []byte(`{"pty_id":"test"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if createPty.WriteLeaseID != ptyLease.ID || createPty.FencingGeneration != ptyLease.AcquiredFencingGeneration {
		t.Fatalf("CreatePty lease = %+v generation %d, want %+v generation %d", createPty.WriteLeaseID, createPty.FencingGeneration, ptyLease.ID, ptyLease.AcquiredFencingGeneration)
	}
	if _, err := queries.MarkWorkspacePtyOpen(ctx, db.MarkWorkspacePtyOpenParams{
		ProcessID:         "wrong-materialization-pty",
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pty.ID,
		MaterializationID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("pty opened from wrong materialization err = %v, want no rows", err)
	}
	if _, err := queries.MarkWorkspacePtyOpen(ctx, db.MarkWorkspacePtyOpenParams{
		ProcessID:         "pty-process",
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pty.ID,
		MaterializationID: materialization.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWorkspacePtyClosed(ctx, db.MarkWorkspacePtyClosedParams{
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                pty.ID,
		MaterializationID: materialization.ID,
	}); err != nil {
		t.Fatal(err)
	}
	ptyLeaseAfter, err := queries.GetWorkspaceLease(ctx, db.GetWorkspaceLeaseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyLease.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ptyLeaseAfter.State != db.WorkspaceLeaseStateReleased {
		t.Fatalf("pty lease state = %s, want released", ptyLeaseAfter.State)
	}
}

func TestWorkspaceInputDeliveredCursorRequiresExactAcceptedChunk(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	execID := pgvalue.UUID(seedWorkspaceExec(t, ctx, pool, ids, materialization))
	ptyID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   ptyID,
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      pgtype.UUID{},
		State:             db.WorkspacePtyStateOpen,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                ptyID,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.InsertWorkspaceExecStreamChunk(ctx, db.InsertWorkspaceExecStreamChunkParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ExecID:        execID,
		Stream:        db.WorkspaceExecStreamStdin,
		OffsetStart:   0,
		OffsetEnd:     5,
		Data:          []byte("hello"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AdvanceWorkspaceExecStreamCursor(ctx, db.AdvanceWorkspaceExecStreamCursorParams{
		Stream:        db.WorkspaceExecStreamStdin,
		OffsetEnd:     5,
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ExecID:        execID,
		OffsetStart:   0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AdvanceWorkspaceExecStdinDeliveredCursor(ctx, db.AdvanceWorkspaceExecStdinDeliveredCursorParams{
		OffsetEnd:     4,
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ExecID:        execID,
		OffsetStart:   0,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("partial exec input delivered err = %v, want no rows", err)
	}
	if _, err := queries.AdvanceWorkspaceExecStdinDeliveredCursor(ctx, db.AdvanceWorkspaceExecStdinDeliveredCursorParams{
		OffsetEnd:     5,
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ExecID:        execID,
		OffsetStart:   0,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.InsertWorkspacePtyStreamChunk(ctx, db.InsertWorkspacePtyStreamChunkParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		PtySessionID:  ptyID,
		Stream:        db.WorkspacePtyStreamInput,
		OffsetStart:   0,
		OffsetEnd:     5,
		Data:          []byte("hello"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AdvanceWorkspacePtyStreamCursor(ctx, db.AdvanceWorkspacePtyStreamCursorParams{
		Stream:        db.WorkspacePtyStreamInput,
		OffsetEnd:     5,
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		PtySessionID:  ptyID,
		OffsetStart:   0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AdvanceWorkspacePtyInputDeliveredCursor(ctx, db.AdvanceWorkspacePtyInputDeliveredCursorParams{
		OffsetEnd:     4,
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		PtySessionID:  ptyID,
		OffsetStart:   0,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("partial pty input delivered err = %v, want no rows", err)
	}
	if _, err := queries.AdvanceWorkspacePtyInputDeliveredCursor(ctx, db.AdvanceWorkspacePtyInputDeliveredCursorParams{
		OffsetEnd:     5,
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		PtySessionID:  ptyID,
		OffsetStart:   0,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspacePtyCreatingDoesNotAcceptResizeOrClose(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	ptyID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   ptyID,
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      pgtype.UUID{},
		State:             db.WorkspacePtyStateCreating,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                ptyID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ResizeWorkspacePtySession(ctx, db.ResizeWorkspacePtySessionParams{
		Cols:          pgtype.Int4{Int32: 120, Valid: true},
		Rows:          pgtype.Int4{Int32: 40, Valid: true},
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resize creating pty err = %v, want no rows", err)
	}
	if _, err := queries.RequestWorkspacePtyClose(ctx, db.RequestWorkspacePtyCloseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("close creating pty err = %v, want no rows", err)
	}
}

func TestWorkspacePtyCloseIsRetryableWhileClosing(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	ptyID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   ptyID,
		Cwd:                  "/workspace",
		Cols:                 80,
		Rows:                 24,
		FilesystemMode:       db.WorkspaceFilesystemModeWrite,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: "test",
		CreatedBySubjectID:   "test",
		OrgID:                pgvalue.UUID(ids.orgID),
		ProjectID:            pgvalue.UUID(ids.projectID),
		EnvironmentID:        pgvalue.UUID(ids.environmentID),
		WorkspaceID:          pgvalue.UUID(ids.workspaceID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      pgtype.UUID{},
		State:             db.WorkspacePtyStateOpen,
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		ID:                ptyID,
	}); err != nil {
		t.Fatal(err)
	}
	first, err := queries.RequestWorkspacePtyClose(ctx, db.RequestWorkspacePtyCloseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != db.WorkspacePtyStateClosing {
		t.Fatalf("first close state = %s, want closing", first.State)
	}
	if _, err := queries.ResizeWorkspacePtySession(ctx, db.ResizeWorkspacePtySessionParams{
		Cols:          pgtype.Int4{Int32: 120, Valid: true},
		Rows:          pgtype.Int4{Int32: 40, Valid: true},
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("resize closing pty err = %v, want no rows", err)
	}
	second, err := queries.RequestWorkspacePtyClose(ctx, db.RequestWorkspacePtyCloseParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		ID:            ptyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != db.WorkspacePtyStateClosing {
		t.Fatalf("retry close state = %s, want closing", second.State)
	}
}

func TestWorkspaceLeasesFenceDirtyAndCapturePromotion(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	materialization := seedRunningWorkspaceMaterialization(t, ctx, pool, queries, ids)
	firstExecID := seedWorkspaceExec(t, ctx, pool, ids, materialization)
	secondExecID := seedWorkspaceExec(t, ctx, pool, ids, materialization)
	writeExecID := seedWorkspaceExec(t, ctx, pool, ids, materialization)

	firstInstance, err := queries.AcquireWorkspaceInstanceLease(ctx, db.AcquireWorkspaceInstanceLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgvalue.UUID(firstExecID),
		FencingToken:      "instance-token-1",
		HeartbeatToken:    "instance-heartbeat-1",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondInstance, err := queries.AcquireWorkspaceInstanceLease(ctx, db.AcquireWorkspaceInstanceLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgvalue.UUID(secondExecID),
		FencingToken:      "instance-token-2",
		HeartbeatToken:    "instance-heartbeat-2",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if firstInstance.LeaseKind != db.WorkspaceLeaseKindInstance || secondInstance.LeaseKind != db.WorkspaceLeaseKindInstance {
		t.Fatalf("instance lease kinds = %s / %s", firstInstance.LeaseKind, secondInstance.LeaseKind)
	}

	writeLease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgvalue.UUID(writeExecID),
		FencingToken:      "write-token",
		HeartbeatToken:    "write-heartbeat",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		FencingToken:      "write-token-2",
		HeartbeatToken:    "write-heartbeat-2",
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	}); err == nil {
		t.Fatal("second active write lease should fail")
	}
	if _, err := queries.MarkWorkspaceWriteLeaseDirty(ctx, db.MarkWorkspaceWriteLeaseDirtyParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		WriteLeaseID: writeLease.ID,
		FencingToken: "stale-token",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale fencing dirty err = %v, want no rows", err)
	}
	dirty, err := queries.MarkWorkspaceWriteLeaseDirty(ctx, db.MarkWorkspaceWriteLeaseDirtyParams{
		OrgID:        pgvalue.UUID(ids.orgID),
		WriteLeaseID: writeLease.ID,
		FencingToken: "write-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dirty.DirtyGeneration != 1 {
		t.Fatalf("dirty generation = %d, want 1", dirty.DirtyGeneration)
	}
	artifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	if _, err := queries.PromoteWorkspaceCapture(ctx, db.PromoteWorkspaceCaptureParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		WriteLeaseID:       writeLease.ID,
		FencingToken:       "write-token",
		DirtyGeneration:    2,
		VersionID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:               db.WorkspaceVersionKindSystem,
		ArtifactID:         pgvalue.UUID(artifactID),
		ArtifactEncoding:   "tar.zst",
		ArtifactEntryCount: 1,
		ContentDigest:      "sha256:capture",
		SizeBytes:          10,
		Message:            "wrong dirty generation",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale capture dirty generation err = %v, want no rows", err)
	}
	if _, err := queries.PromoteWorkspaceCapture(ctx, db.PromoteWorkspaceCaptureParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		WriteLeaseID:       writeLease.ID,
		FencingToken:       "write-token",
		DirtyGeneration:    dirty.DirtyGeneration,
		VersionID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:               db.WorkspaceVersionKindSystem,
		ArtifactID:         pgvalue.UUID(artifactID),
		ArtifactEncoding:   "tar.zst",
		ArtifactEntryCount: 1,
		ContentDigest:      "sha256:capture",
		SizeBytes:          11,
		Message:            "bad artifact size",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CAS mismatch capture err = %v, want no rows", err)
	}
	var promotedAfterCASFailure bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM workspace_versions
			 WHERE org_id = $1
			   AND workspace_id = $2
			   AND artifact_id = $3
		)
	`, ids.orgID, ids.workspaceID, artifactID).Scan(&promotedAfterCASFailure); err != nil {
		t.Fatal(err)
	}
	if promotedAfterCASFailure {
		t.Fatal("CAS mismatch should not create a workspace version")
	}
	versionID := uuid.Must(uuid.NewV7())
	version, err := queries.PromoteWorkspaceCapture(ctx, db.PromoteWorkspaceCaptureParams{
		OrgID:              pgvalue.UUID(ids.orgID),
		WriteLeaseID:       writeLease.ID,
		FencingToken:       "write-token",
		DirtyGeneration:    dirty.DirtyGeneration,
		VersionID:          pgvalue.UUID(versionID),
		Kind:               db.WorkspaceVersionKindSystem,
		ArtifactID:         pgvalue.UUID(artifactID),
		ArtifactEncoding:   "tar.zst",
		ArtifactEntryCount: 1,
		ContentDigest:      "sha256:capture",
		SizeBytes:          10,
		Message:            "capture on stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if version.ID != pgvalue.UUID(versionID) || version.State != db.WorkspaceVersionStateReady || !version.PromotedAt.Valid {
		t.Fatalf("version = %+v", version)
	}
	var currentVersionID uuid.UUID
	var dirtyState string
	if err := pool.QueryRow(ctx, `SELECT current_version_id, dirty_state::text FROM workspaces WHERE id = $1`, ids.workspaceID).Scan(&currentVersionID, &dirtyState); err != nil {
		t.Fatal(err)
	}
	if currentVersionID != versionID || dirtyState != "clean" {
		t.Fatalf("workspace current_version_id=%s dirty_state=%s", currentVersionID, dirtyState)
	}
}

func TestGetWaitpointTokenForPublicCompletionRequiresEnvironmentBinding(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	tokenID := uuid.Must(uuid.NewV7())
	token, err := queries.CreateWaitpointToken(ctx, db.CreateWaitpointTokenParams{
		ID:                 pgvalue.UUID(tokenID),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		CallbackSecretHash: []byte("callback-hash-public-scope"),
		TimeoutAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		Metadata:           []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetWaitpointTokenForPublicCompletion(ctx, db.GetWaitpointTokenForPublicCompletionParams{
		OrgID:         token.OrgID,
		ProjectID:     token.ProjectID,
		EnvironmentID: token.EnvironmentID,
		ID:            token.ID,
	}); err != nil {
		t.Fatalf("same environment lookup failed: %v", err)
	}
	_, err = queries.GetWaitpointTokenForPublicCompletion(ctx, db.GetWaitpointTokenForPublicCompletionParams{
		OrgID:         token.OrgID,
		ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: token.EnvironmentID,
		ID:            token.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-project lookup error = %v", err)
	}
	_, err = queries.GetWaitpointTokenForPublicCompletion(ctx, db.GetWaitpointTokenForPublicCompletionParams{
		OrgID:         token.OrgID,
		ProjectID:     token.ProjectID,
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ID:            token.ID,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-environment lookup error = %v", err)
	}
}

func TestAttachCompletedWaitpointTokenResolvesWaitpoint(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	tokenID := uuid.Must(uuid.NewV7())
	waitpointID := uuid.Must(uuid.NewV7())
	token, err := queries.CreateWaitpointToken(ctx, db.CreateWaitpointTokenParams{
		ID:                 pgvalue.UUID(tokenID),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		CallbackSecretHash: []byte("callback-hash-b"),
		TimeoutAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		Metadata:           []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"approved":true}`)
	if _, err := queries.CompleteWaitpointToken(ctx, db.CompleteWaitpointTokenParams{
		OrgID:          token.OrgID,
		ID:             token.ID,
		Data:           data,
		CompletionHash: pgvalue.Text(completionHash(t, data)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO waitpoints (id, org_id, project_id, environment_id, run_id, kind, params)
		VALUES ($1, $2, $3, $4, $5, 'token', '{"token_id":"external"}')
	`, waitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID); err != nil {
		t.Fatal(err)
	}
	attached, err := queries.AttachWaitpointTokenToWaitpoint(ctx, db.AttachWaitpointTokenToWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(waitpointID),
		TokenID:     token.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !attached.ResolvedWaitpoint {
		t.Fatal("completed token did not resolve the pending waitpoint")
	}
	var status string
	var resolvedData []byte
	if err := pool.QueryRow(ctx, `SELECT status::text, data FROM waitpoints WHERE id = $1`, waitpointID).Scan(&status, &resolvedData); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || string(resolvedData) != `{"approved": true}` {
		t.Fatalf("waitpoint status=%s data=%s", status, resolvedData)
	}
}

func TestCompleteAttachedWaitpointTokenResolvesAndUnblocksRunSuspension(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	waitpointID := seedWaitingRunSuspensionForWaitpoint(t, ctx, pool, ids)
	tokenID := uuid.Must(uuid.NewV7())
	token, err := queries.CreateWaitpointToken(ctx, db.CreateWaitpointTokenParams{
		ID:                 pgvalue.UUID(tokenID),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		CallbackSecretHash: []byte("callback-hash-attached"),
		TimeoutAt:          pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		Metadata:           []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := queries.AttachWaitpointTokenToWaitpoint(ctx, db.AttachWaitpointTokenToWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(waitpointID),
		TokenID:     token.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if attached.ResolvedWaitpoint {
		t.Fatal("waiting token should attach without resolving the waitpoint")
	}

	data := []byte(`{"approved":true,"note":"ship"}`)
	completed, err := queries.CompleteWaitpointToken(ctx, db.CompleteWaitpointTokenParams{
		OrgID:          token.OrgID,
		ID:             token.ID,
		Data:           data,
		CompletionHash: pgvalue.Text(completionHash(t, data)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !completed.ResolvedWaitpoint || !completed.WaitpointID.Valid {
		t.Fatalf("complete did not resolve attached waitpoint: resolved=%v id=%v", completed.ResolvedWaitpoint, completed.WaitpointID)
	}

	resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(waitpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 {
		t.Fatalf("resumed rows = %d", len(resumed))
	}
	if resumed[0].Status != db.RunSuspensionStatusResuming {
		t.Fatalf("run suspension status = %s", resumed[0].Status)
	}
	if got := canonicalJSON(t, resumed[0].Resolution); got != `{"approved":true,"note":"ship"}` {
		t.Fatalf("run suspension resolution = %s", resumed[0].Resolution)
	}

	var runStatus string
	if err := pool.QueryRow(ctx, `SELECT status::text FROM runs WHERE id = $1`, ids.runID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "queued" {
		t.Fatalf("run status = %s", runStatus)
	}
}

func TestUnblockRunWaitpointsForWaitpointPublishesDirectCompletionData(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	waitpointID := seedWaitingRunSuspensionForWaitpoint(t, ctx, pool, ids)
	data := []byte(`{"approved":true}`)
	if _, err := pool.Exec(ctx, `
		UPDATE waitpoints
		   SET status = 'completed',
		       data = $2::jsonb,
		       resolved_at = now()
		 WHERE id = $1
	`, waitpointID, data); err != nil {
		t.Fatal(err)
	}
	resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(waitpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 {
		t.Fatalf("resumed rows = %d", len(resumed))
	}
	if got := canonicalJSON(t, resumed[0].Resolution); got != `{"approved":true}` {
		t.Fatalf("run suspension resolution = %s", resumed[0].Resolution)
	}
	var eventPayload []byte
	if err := pool.QueryRow(ctx, `
		SELECT payload
		  FROM events
		 WHERE org_id = $1
		   AND run_id = $2
		   AND kind = 'waitpoint.completed'
		 ORDER BY seq DESC
		 LIMIT 1
	`, ids.orgID, ids.runID).Scan(&eventPayload); err != nil {
		t.Fatal(err)
	}
	var event struct {
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(eventPayload, &event); err != nil {
		t.Fatal(err)
	}
	if got := canonicalJSON(t, event.Payload); got != `{"approved":true}` {
		t.Fatalf("event payload.payload = %s", event.Payload)
	}
}

func TestUnblockRunWaitpointsForWaitpointPublishesDirectTimeoutData(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	waitpointID := seedWaitingRunSuspensionForWaitpoint(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		UPDATE waitpoints
		   SET status = 'timed_out',
		       data = NULL,
		       error = '{"code":"timed_out"}'::jsonb,
		       resolved_at = now()
		 WHERE id = $1
	`, waitpointID); err != nil {
		t.Fatal(err)
	}
	resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(waitpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 {
		t.Fatalf("resumed rows = %d", len(resumed))
	}
	if !resumed[0].ResolutionKind.Valid || resumed[0].ResolutionKind.String != "timed_out" {
		t.Fatalf("resolution kind = %+v", resumed[0].ResolutionKind)
	}
	if got := canonicalJSON(t, resumed[0].Resolution); got != `null` {
		t.Fatalf("run suspension resolution = %s", resumed[0].Resolution)
	}
}

func TestResolveRunChannelWaitpointsWaitBeforeSend(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	waitpointID := seedChannelWaitpoint(t, ctx, pool, ids, "approval", 0)

	if _, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := queries.ResolveRunChannelWaitpointsForRun(ctx, db.ResolveRunChannelWaitpointsForRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != waitpointID {
		t.Fatalf("completed input waitpoints = %+v, want %s", completed, waitpointID)
	}
	resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(waitpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 {
		t.Fatalf("resumed waits = %d, want 1", len(resumed))
	}
	assertChannelWaitpointData(t, ctx, pool, waitpointID, `{"channel":"approval","correlation_id":"","data":{"approved":true},"sequence":1}`)
}

func TestResolveRunChannelWaitpointsSendBeforeWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	seedTaskSessionForRun(t, ctx, pool, ids)
	if _, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	waitpointID := seedChannelWaitpoint(t, ctx, pool, ids, "approval", 0)
	completed, err := queries.ResolveRunChannelWaitpointsForRun(ctx, db.ResolveRunChannelWaitpointsForRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != waitpointID {
		t.Fatalf("completed input waitpoints = %+v, want %s", completed, waitpointID)
	}
	assertChannelWaitpointData(t, ctx, pool, waitpointID, `{"channel":"approval","correlation_id":"","data":{"approved":false},"sequence":1}`)
}

func TestResolveRunChannelWaitpointsRespectsWaitCursors(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	firstWaitpointID := seedChannelWaitpoint(t, ctx, pool, ids, "approval", 0)
	secondWaitpointID := seedAdditionalChannelWaitpoint(t, ctx, pool, ids, "approval", 1)

	if _, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := queries.ResolveRunChannelWaitpointsForRun(ctx, db.ResolveRunChannelWaitpointsForRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != firstWaitpointID {
		t.Fatalf("completed input waitpoints = %+v, want first waitpoint %s only", completed, firstWaitpointID)
	}
	assertChannelWaitpointData(t, ctx, pool, firstWaitpointID, `{"channel":"approval","correlation_id":"","data":{"approved":true},"sequence":1}`)
	assertWaitpointStatus(t, ctx, pool, secondWaitpointID, "pending")

	if _, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err = queries.ResolveRunChannelWaitpointsForRun(ctx, db.ResolveRunChannelWaitpointsForRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != secondWaitpointID {
		t.Fatalf("completed input waitpoints = %+v, want second waitpoint %s", completed, secondWaitpointID)
	}
	assertChannelWaitpointData(t, ctx, pool, secondWaitpointID, `{"channel":"approval","correlation_id":"","data":{"approved":false},"sequence":2}`)
}

func TestChannelWaitCursorAdvancesUnfilteredWaitForCorrelatedRecord(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	taskSessionID := uuid.Must(uuid.NewV7())
	channelID := uuid.Must(uuid.NewV7())
	waitpointID := seedWaitingRunSuspensionForWaitpoint(t, ctx, pool, ids)
	var runSuspensionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT run_suspension_id
		  FROM run_suspension_waitpoints
		 WHERE org_id = $1
		   AND waitpoint_id = $2
	`, ids.orgID, waitpointID).Scan(&runSuspensionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (org_id, project_id, environment_id, task_id)
		VALUES ($1, $2, $3, 'approval-task')
		ON CONFLICT DO NOTHING
	`, ids.orgID, ids.projectID, ids.environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_sessions (
			id, org_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id, current_run_id
		)
		VALUES ($1, $2, $3, $4, 'approval-task', $5, $5, $6, $7)
	`, taskSessionID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.workspaceID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET task_session_id = $1,
		       workspace_id = $4
		 WHERE org_id = $2
		   AND id = $3
	`, taskSessionID, ids.orgID, ids.runID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO channels (id, org_id, project_id, environment_id, task_session_id, definition_id, name, direction)
		VALUES ($1, $2, $3, $4, $5, $6, 'approval', 'input')
	`, channelID, ids.orgID, ids.projectID, ids.environmentID, taskSessionID, seedChannelDefinition(t, ctx, pool, ids, "approval", "input")); err != nil {
		t.Fatal(err)
	}
	consumerKey := "runtime:input-wait"
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_wait_cursors (
			org_id, project_id, environment_id, task_session_id, channel_id, consumer_key, correlation_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, NULL)
	`, ids.orgID, ids.projectID, ids.environmentID, taskSessionID, channelID, consumerKey); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE waitpoints
		   SET kind = 'channel',
		       params = '{"channel":"approval","after_sequence":0}'::jsonb
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, waitpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_waits (
			waitpoint_id, org_id, project_id, environment_id, run_id, run_suspension_id, channel_id, after_sequence
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0)
	`, waitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, runSuspensionID, channelID); err != nil {
		t.Fatal(err)
	}
	record, err := queries.AppendChannelRecord(ctx, db.AppendChannelRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		ChannelID:              pgvalue.UUID(channelID),
		Direction:              db.ChannelDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		CorrelationID:          "thread-1",
		ContentType:            "application/json",
		IdempotencyFingerprint: "test-fingerprint",
		Actor:                  []byte(`{"type":"test"}`),
		Source:                 "test",
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := queries.ResolveChannelWaitpointsForChannel(ctx, db.ResolveChannelWaitpointsForChannelParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		ChannelID: pgvalue.UUID(channelID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != waitpointID {
		t.Fatalf("completed channel waitpoints = %+v, want %s", completed, waitpointID)
	}
	var lastDeliveredSequence int64
	var lastDeliveredRecordID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT last_delivered_sequence, last_delivered_record_id
		  FROM channel_wait_cursors
		 WHERE org_id = $1
		   AND task_session_id = $2
		   AND channel_id = $3
		   AND consumer_key = $4
		   AND correlation_id IS NULL
	`, ids.orgID, taskSessionID, channelID, consumerKey).Scan(&lastDeliveredSequence, &lastDeliveredRecordID); err != nil {
		t.Fatal(err)
	}
	if lastDeliveredSequence != 0 || lastDeliveredRecordID != uuid.Nil {
		t.Fatalf("pre-ack cursor = (%d, %s), want unconsumed; matched record = (%d, %s)", lastDeliveredSequence, lastDeliveredRecordID, record.Sequence, pgvalue.MustUUIDValue(record.ID))
	}
	assertChannelWaitpointData(t, ctx, pool, waitpointID, `{"channel":"approval","correlation_id":"thread-1","data":{"approved":true},"sequence":1}`)
}

func TestCreateRunSuspensionForWaitpointUsesTaskSessionChannel(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	taskSessionID, runLeaseID, workerID := seedRunningTaskSessionLease(t, ctx, pool, ids)
	channelID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO channels (id, org_id, project_id, environment_id, task_session_id, definition_id, name, direction)
		VALUES ($1, $2, $3, $4, $5, $6, 'approval', 'input')
	`, channelID, ids.orgID, ids.projectID, ids.environmentID, taskSessionID, seedChannelDefinition(t, ctx, pool, ids, "approval", "input")); err != nil {
		t.Fatal(err)
	}
	_, err := queries.AppendChannelRecord(ctx, db.AppendChannelRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		ChannelID:              pgvalue.UUID(channelID),
		Direction:              db.ChannelDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		CorrelationID:          "thread-1",
		ContentType:            "application/json",
		IdempotencyFingerprint: "test-fingerprint",
		Actor:                  []byte(`{"type":"test"}`),
		Source:                 "test",
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondRecord, err := queries.AppendChannelRecord(ctx, db.AppendChannelRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		ChannelID:              pgvalue.UUID(channelID),
		Direction:              db.ChannelDirectionInput,
		Data:                   []byte(`{"approved":false}`),
		CorrelationID:          "thread-1",
		ContentType:            "application/json",
		IdempotencyFingerprint: "test-fingerprint-2",
		Actor:                  []byte(`{"type":"test"}`),
		Source:                 "test",
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitpointID := uuid.Must(uuid.NewV7())
	waitpoint, err := queries.CreateRunSuspensionForWaitpoint(ctx, db.CreateRunSuspensionForWaitpointParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		CorrelationID:    "wait-1",
		CheckpointID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		CheckpointReason: "waitpoint",
		ID:               pgvalue.UUID(waitpointID),
		Kind:             db.WaitpointKindChannel,
		Params:           []byte(`{"channel":"approval","after_sequence":0,"correlation_id":"thread-1"}`),
		Metadata:         []byte(`{}`),
		Tags:             []string{},
		RunSuspensionID:  pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondWaitpointID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO waitpoints (id, org_id, project_id, environment_id, run_id, kind, status, params, data, resolved_at)
		VALUES (
			$1, $2, $3, $4, $5, 'channel', 'completed',
			'{"channel":"approval","after_sequence":1,"correlation_id":"thread-1"}'::jsonb,
			jsonb_build_object(
				'channel', 'approval',
				'correlation_id', 'thread-1',
				'sequence', $6::bigint,
				'data', $7::jsonb
			),
			now()
		)
	`, secondWaitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, secondRecord.Sequence, secondRecord.Data); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspension_waitpoints (
			org_id, run_id, project_id, environment_id, run_suspension_id, waitpoint_id, ordinal
		)
		VALUES ($1, $2, $3, $4, $5, $6, 1)
	`, ids.orgID, ids.runID, ids.projectID, ids.environmentID, pgvalue.MustUUIDValue(waitpoint.RunSuspensionID), secondWaitpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_waits (
			waitpoint_id, org_id, project_id, environment_id, run_id, run_suspension_id, channel_id, after_sequence, correlation_id, matched_record_id, matched_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 1, 'thread-1', $8, now())
	`, secondWaitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, pgvalue.MustUUIDValue(waitpoint.RunSuspensionID), channelID, pgvalue.MustUUIDValue(secondRecord.ID)); err != nil {
		t.Fatal(err)
	}
	var channelTaskSessionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT channels.task_session_id
		  FROM channel_waits
		  JOIN channels ON channels.org_id = channel_waits.org_id
		               AND channels.id = channel_waits.channel_id
		 WHERE channel_waits.org_id = $1
		   AND channel_waits.waitpoint_id = $2
	`, ids.orgID, waitpointID).Scan(&channelTaskSessionID); err != nil {
		t.Fatal(err)
	}
	if channelTaskSessionID != taskSessionID {
		t.Fatalf("channel task_session_id = %s, want %s", channelTaskSessionID, taskSessionID)
	}
	if waitpoint.Status != db.RunSuspensionStatusOpening {
		t.Fatalf("run suspension status = %s, want opening", waitpoint.Status)
	}
	var checkpointStatus db.CheckpointStatus
	if err := pool.QueryRow(ctx, `
		SELECT status
		  FROM checkpoints
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(waitpoint.CheckpointID)).Scan(&checkpointStatus); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusCreating {
		t.Fatalf("checkpoint status = %s, want creating", checkpointStatus)
	}
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		SELECT runtime_id
		  FROM run_leases
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	ready, err := queries.MarkRunSuspensionCheckpointReady(ctx, db.MarkRunSuspensionCheckpointReadyParams{
		OrgID:                      pgvalue.UUID(ids.orgID),
		RunID:                      pgvalue.UUID(ids.runID),
		RunLeaseID:                 pgvalue.UUID(runLeaseID),
		WorkerInstanceID:           pgvalue.UUID(workerID),
		RunSuspensionID:            waitpoint.RunSuspensionID,
		CheckpointID:               waitpoint.CheckpointID,
		WaitpointID:                waitpoint.ID,
		RuntimeBackend:             "firecracker",
		RuntimeID:                  runtimeID,
		RuntimeArch:                "arm64",
		RuntimeABI:                 "test",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "default",
		CheckpointArtifacts:        []byte(`[{"role":"runtime_config","ordinal":0,"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size_bytes":1,"media_type":"application/json"}]`),
		WorkspaceArtifactDigest:    pgvalue.Text("sha256:2222222222222222222222222222222222222222222222222222222222222222"),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 2, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		Manifest:                   []byte(`{"version":1}`),
		RuntimeVcpus:               pgtype.Int4{Int32: 1, Valid: true},
		RuntimeMemoryMib:           pgtype.Int4{Int32: 1024, Valid: true},
		RuntimeScratchDiskMib:      pgtype.Int4{Int32: 4096, Valid: true},
		ImageKey:                   pgvalue.Text("test-image"),
		WorkspaceArtifactEncoding:  pgvalue.Text("tar"),
		WorkspaceMountPath:         pgvalue.Text("/workspace"),
		ActiveDurationMs:           10,
		CheckpointPayload:          []byte(`{"waitpoint":"wait-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != db.RunSuspensionStatusWaiting {
		t.Fatalf("ready run suspension status = %s, want waiting", ready.Status)
	}
	completed, err := queries.ResolveRunChannelWaitpointsForRun(ctx, db.ResolveRunChannelWaitpointsForRunParams{
		OrgID: pgvalue.UUID(ids.orgID),
		RunID: pgvalue.UUID(ids.runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != waitpointID {
		t.Fatalf("completed waitpoints = %+v, want %s", completed, waitpointID)
	}
	resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(waitpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 {
		t.Fatalf("resumed rows = %d, want 1", len(resumed))
	}
	if !resumed[0].ResolutionKind.Valid || resumed[0].ResolutionKind.String != "waitpoints" {
		t.Fatalf("resolution kind = %+v, want waitpoints", resumed[0].ResolutionKind)
	}
	if got := canonicalJSON(t, resumed[0].Resolution); got != `{"waitpoints":[{"channel":"approval","correlation_id":"thread-1","data":{"approved":true},"sequence":1},{"channel":"approval","correlation_id":"thread-1","data":{"approved":false},"sequence":2}]}` {
		t.Fatalf("resolution = %s", got)
	}
	var lastDeliveredSequence int64
	var lastDeliveredRecordID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT last_delivered_sequence, last_delivered_record_id
		  FROM channel_wait_cursors
		 WHERE org_id = $1
		   AND task_session_id = $2
		   AND channel_id = $3
		   AND consumer_key = $4
		   AND correlation_id = 'thread-1'
	`, ids.orgID, taskSessionID, channelID, "runtime:input-wait").Scan(&lastDeliveredSequence, &lastDeliveredRecordID); err != nil {
		t.Fatal(err)
	}
	if lastDeliveredSequence != 0 || lastDeliveredRecordID.Valid {
		t.Fatalf("pre-ack cursor = (%d, %v), want unconsumed", lastDeliveredSequence, lastDeliveredRecordID)
	}
	restoreLeaseID := uuid.Must(uuid.NewV7())
	restoreDispatchMessageID := "restore-" + shortUUID(restoreLeaseID)
	restoreDispatchLeaseID := "restore-lease-" + shortUUID(restoreLeaseID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, run_id, attempt_id, worker_instance_id, worker_group_id, dispatch_message_id,
			dispatch_lease_id, dispatch_attempt, status, lease_expires_at, runtime_id, trace_id,
			span_id, parent_span_id, traceparent, restore_checkpoint_id
		)
		SELECT $1, runs.org_id, runs.id, runs.current_attempt_id, $2,
		       (SELECT worker_group_id FROM worker_instances WHERE id = $2), $3, $4, 2, 'running', now() + interval '1 hour', $5,
		       runs.trace_id, '4444444444444444', runs.root_span_id,
		       '00-' || runs.trace_id || '-4444444444444444-01', $6
		  FROM runs
		 WHERE runs.org_id = $7
		   AND runs.id = $8
	`, restoreLeaseID, workerID, restoreDispatchMessageID, restoreDispatchLeaseID, runtimeID, pgvalue.MustUUIDValue(waitpoint.CheckpointID), ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'running',
		       execution_status = 'executing',
		       current_run_lease_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, restoreLeaseID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	restored, err := queries.AcknowledgeRestore(ctx, db.AcknowledgeRestoreParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(restoreLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		RunSuspensionID:  waitpoint.RunSuspensionID,
		CheckpointID:     waitpoint.CheckpointID,
		WaitpointID:      waitpoint.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != db.RunSuspensionStatusRestored {
		t.Fatalf("restored run suspension status = %s, want restored", restored.Status)
	}
	if err := pool.QueryRow(ctx, `
		SELECT last_delivered_sequence, last_delivered_record_id
		  FROM channel_wait_cursors
		 WHERE org_id = $1
		   AND task_session_id = $2
		   AND channel_id = $3
		   AND consumer_key = $4
		   AND correlation_id = 'thread-1'
	`, ids.orgID, taskSessionID, channelID, "runtime:input-wait").Scan(&lastDeliveredSequence, &lastDeliveredRecordID); err != nil {
		t.Fatal(err)
	}
	if lastDeliveredSequence != 2 || !lastDeliveredRecordID.Valid || pgvalue.MustUUIDValue(lastDeliveredRecordID) != pgvalue.MustUUIDValue(secondRecord.ID) {
		t.Fatalf("post-ack cursor = (%d, %v), want (%d, %s)", lastDeliveredSequence, lastDeliveredRecordID, int64(2), pgvalue.MustUUIDValue(secondRecord.ID))
	}
}

func TestAcknowledgeRestoreDoesNotCommitMatchedChannelCursorForTimedOutAggregate(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	taskSessionID, runLeaseID, workerID := seedRunningTaskSessionLease(t, ctx, pool, ids)
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		SELECT runtime_id
		  FROM run_leases
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	channelWaitpointID := uuid.Must(uuid.NewV7())
	timerWaitpointID := uuid.Must(uuid.NewV7())
	runSuspensionID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	channelWaitpoint, err := queries.CreateRunSuspensionForWaitpoint(ctx, db.CreateRunSuspensionForWaitpointParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Kind:             db.WaitpointKindChannel,
		CorrelationID:    "aggregate-1",
		CheckpointID:     pgvalue.UUID(checkpointID),
		CheckpointReason: "waitpoint",
		ID:               pgvalue.UUID(channelWaitpointID),
		Params:           []byte(`{"channel":"approval","after_sequence":0}`),
		Metadata:         []byte(`{}`),
		Tags:             []string{},
		RunSuspensionID:  pgvalue.UUID(runSuspensionID),
		Ordinal:          0,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateRunSuspensionForWaitpoint(ctx, db.CreateRunSuspensionForWaitpointParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Kind:             db.WaitpointKindToken,
		CorrelationID:    "aggregate-1",
		CheckpointID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		CheckpointReason: "waitpoint",
		ID:               pgvalue.UUID(timerWaitpointID),
		Params:           []byte(`{"token_id":"token-1"}`),
		Metadata:         []byte(`{}`),
		Tags:             []string{},
		RunSuspensionID:  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		TimeoutSeconds:   pgtype.Int4{Int32: 1, Valid: true},
		Ordinal:          1,
	}); err != nil {
		t.Fatal(err)
	}
	ready, err := queries.MarkRunSuspensionCheckpointReady(ctx, db.MarkRunSuspensionCheckpointReadyParams{
		OrgID:                      pgvalue.UUID(ids.orgID),
		RunID:                      pgvalue.UUID(ids.runID),
		RunLeaseID:                 pgvalue.UUID(runLeaseID),
		WorkerInstanceID:           pgvalue.UUID(workerID),
		RunSuspensionID:            channelWaitpoint.RunSuspensionID,
		CheckpointID:               channelWaitpoint.CheckpointID,
		WaitpointID:                channelWaitpoint.ID,
		RuntimeBackend:             "firecracker",
		RuntimeID:                  runtimeID,
		RuntimeArch:                "arm64",
		RuntimeABI:                 "test",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "default",
		CheckpointArtifacts:        []byte(`[{"role":"runtime_config","ordinal":0,"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size_bytes":1,"media_type":"application/json"}]`),
		WorkspaceArtifactDigest:    pgvalue.Text("sha256:2222222222222222222222222222222222222222222222222222222222222222"),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 2, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		Manifest:                   []byte(`{"version":1}`),
		RuntimeVcpus:               pgtype.Int4{Int32: 1, Valid: true},
		RuntimeMemoryMib:           pgtype.Int4{Int32: 1024, Valid: true},
		RuntimeScratchDiskMib:      pgtype.Int4{Int32: 4096, Valid: true},
		ImageKey:                   pgvalue.Text("test-image"),
		WorkspaceArtifactEncoding:  pgvalue.Text("tar"),
		WorkspaceMountPath:         pgvalue.Text("/workspace"),
		ActiveDurationMs:           10,
		CheckpointPayload:          []byte(`{"waitpoint":"aggregate-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != db.RunSuspensionStatusWaiting {
		t.Fatalf("run suspension status = %s, want waiting", ready.Status)
	}
	var channelID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT channel_id
		  FROM channel_waits
		 WHERE org_id = $1
		   AND waitpoint_id = $2
	`, ids.orgID, channelWaitpointID).Scan(&channelID); err != nil {
		t.Fatal(err)
	}
	record, err := queries.AppendChannelRecord(ctx, db.AppendChannelRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		ChannelID:              pgvalue.UUID(channelID),
		Direction:              db.ChannelDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		CorrelationID:          "",
		ContentType:            "application/json",
		IdempotencyFingerprint: "aggregate-channel-match",
		Actor:                  []byte(`{"type":"test"}`),
		Source:                 "test",
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := queries.ResolveChannelWaitpointsForChannel(ctx, db.ResolveChannelWaitpointsForChannelParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		ChannelID: pgvalue.UUID(channelID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != channelWaitpointID {
		t.Fatalf("completed channel waitpoints = %+v, want %s", completed, channelWaitpointID)
	}
	resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(channelWaitpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 0 {
		t.Fatalf("partial aggregate resumed rows = %d, want 0", len(resumed))
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_suspensions
		   SET waiting_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(channelWaitpoint.RunSuspensionID)); err != nil {
		t.Fatal(err)
	}
	if err := queries.ExpireDuePendingWaitpoints(ctx, pgvalue.UUID(ids.orgID)); err != nil {
		t.Fatal(err)
	}
	restoreLeaseID := uuid.Must(uuid.NewV7())
	restoreDispatchMessageID := "restore-" + shortUUID(restoreLeaseID)
	restoreDispatchLeaseID := "restore-lease-" + shortUUID(restoreLeaseID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, run_id, attempt_id, worker_instance_id, worker_group_id, dispatch_message_id,
			dispatch_lease_id, dispatch_attempt, status, lease_expires_at, runtime_id, trace_id,
			span_id, parent_span_id, traceparent, restore_checkpoint_id
		)
		SELECT $1, runs.org_id, runs.id, runs.current_attempt_id, $2,
		       (SELECT worker_group_id FROM worker_instances WHERE id = $2), $3, $4, 2, 'running', now() + interval '1 hour', $5,
		       runs.trace_id, '4444444444444444', runs.root_span_id,
		       '00-' || runs.trace_id || '-4444444444444444-01', $6
		  FROM runs
		 WHERE runs.org_id = $7
		   AND runs.id = $8
	`, restoreLeaseID, workerID, restoreDispatchMessageID, restoreDispatchLeaseID, runtimeID, checkpointID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET status = 'running',
		       execution_status = 'executing',
		       current_run_lease_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, restoreLeaseID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	restored, err := queries.AcknowledgeRestore(ctx, db.AcknowledgeRestoreParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(restoreLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		RunSuspensionID:  channelWaitpoint.RunSuspensionID,
		CheckpointID:     channelWaitpoint.CheckpointID,
		WaitpointID:      channelWaitpoint.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != db.RunSuspensionStatusRestored {
		t.Fatalf("restored run suspension status = %s", restored.Status)
	}
	if !restored.ResolutionKind.Valid || restored.ResolutionKind.String != "timed_out" {
		t.Fatalf("resolution kind = %+v, want timed_out", restored.ResolutionKind)
	}
	var lastDeliveredSequence int64
	var lastDeliveredRecordID pgtype.UUID
	if err := pool.QueryRow(ctx, `
		SELECT last_delivered_sequence, last_delivered_record_id
		  FROM channel_wait_cursors
		 WHERE org_id = $1
		   AND task_session_id = $2
		   AND channel_id = $3
		   AND consumer_key = 'runtime:input-wait'
		   AND correlation_id IS NULL
	`, ids.orgID, taskSessionID, channelID).Scan(&lastDeliveredSequence, &lastDeliveredRecordID); err != nil {
		t.Fatal(err)
	}
	if lastDeliveredSequence != 0 || lastDeliveredRecordID.Valid {
		t.Fatalf("cursor committed failed aggregate record: sequence=%d record=%v, matched record=%s", lastDeliveredSequence, lastDeliveredRecordID, pgvalue.MustUUIDValue(record.ID))
	}
	var releasedRecordID pgtype.UUID
	var releasedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT matched_record_id, matched_at
		  FROM channel_waits
		 WHERE org_id = $1
		   AND waitpoint_id = $2
	`, ids.orgID, channelWaitpointID).Scan(&releasedRecordID, &releasedAt); err != nil {
		t.Fatal(err)
	}
	if releasedRecordID.Valid || releasedAt.Valid {
		t.Fatalf("failed aggregate kept channel record reservation: matched_record_id=%v matched_at=%v", releasedRecordID, releasedAt)
	}
}

func TestTimerWaitpointDoesNotTimeoutAggregateWaitAll(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	channelWaitpointID := seedChannelWaitpoint(t, ctx, pool, ids, "approval", 0)
	var runSuspensionID uuid.UUID
	var channelID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT run_suspension_waitpoints.run_suspension_id, channel_waits.channel_id
		  FROM run_suspension_waitpoints
		  JOIN channel_waits ON channel_waits.org_id = run_suspension_waitpoints.org_id
		                    AND channel_waits.waitpoint_id = run_suspension_waitpoints.waitpoint_id
		 WHERE run_suspension_waitpoints.org_id = $1
		   AND run_suspension_waitpoints.waitpoint_id = $2
	`, ids.orgID, channelWaitpointID).Scan(&runSuspensionID, &channelID); err != nil {
		t.Fatal(err)
	}
	timerWaitpointID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO waitpoints (id, org_id, project_id, environment_id, run_id, kind, params)
		VALUES ($1, $2, $3, $4, $5, 'timer', '{"seconds":1}'::jsonb)
	`, timerWaitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspension_waitpoints (
			org_id, run_id, project_id, environment_id, run_suspension_id, waitpoint_id, ordinal, timeout_seconds
		)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 1)
	`, ids.orgID, ids.runID, ids.projectID, ids.environmentID, runSuspensionID, timerWaitpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE run_suspensions
		   SET waiting_at = now() - interval '2 seconds'
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runSuspensionID); err != nil {
		t.Fatal(err)
	}
	if err := queries.ExpireDuePendingWaitpoints(ctx, pgvalue.UUID(ids.orgID)); err != nil {
		t.Fatal(err)
	}
	var runSuspensionStatus string
	var timerStatus string
	if err := pool.QueryRow(ctx, `
		SELECT run_suspensions.status::text, waitpoints.status::text
		  FROM run_suspensions
		  JOIN waitpoints ON waitpoints.org_id = run_suspensions.org_id
		                 AND waitpoints.id = $3
		 WHERE run_suspensions.org_id = $1
		   AND run_suspensions.id = $2
	`, ids.orgID, runSuspensionID, timerWaitpointID).Scan(&runSuspensionStatus, &timerStatus); err != nil {
		t.Fatal(err)
	}
	if runSuspensionStatus != "waiting" || timerStatus != "timed_out" {
		t.Fatalf("after timer expiry suspension=%s timer=%s, want waiting/timed_out", runSuspensionStatus, timerStatus)
	}
	if _, err := queries.AppendChannelRecord(ctx, db.AppendChannelRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		ChannelID:              pgvalue.UUID(channelID),
		Direction:              db.ChannelDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		CorrelationID:          "",
		ContentType:            "application/json",
		IdempotencyFingerprint: "timer-aggregate-channel",
		Actor:                  []byte(`{"type":"test"}`),
		Source:                 "test",
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := queries.ResolveChannelWaitpointsForChannel(ctx, db.ResolveChannelWaitpointsForChannelParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		ChannelID: pgvalue.UUID(channelID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != channelWaitpointID {
		t.Fatalf("completed channel waitpoints = %+v, want %s", completed, channelWaitpointID)
	}
	resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(channelWaitpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 {
		t.Fatalf("resumed rows = %d, want 1", len(resumed))
	}
	if !resumed[0].ResolutionKind.Valid || resumed[0].ResolutionKind.String != "waitpoints" {
		t.Fatalf("resolution kind = %+v, want waitpoints", resumed[0].ResolutionKind)
	}
	if got := canonicalJSON(t, resumed[0].Resolution); got != `{"waitpoints":[{"channel":"approval","correlation_id":"","data":{"approved":true},"sequence":1},null]}` {
		t.Fatalf("resolution = %s", got)
	}
}

func TestAggregateWaitResumesWhenNonTimerDependencyTimesOut(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	channelWaitpointID := seedChannelWaitpoint(t, ctx, pool, ids, "approval", 0)
	var runSuspensionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT run_suspension_id
		  FROM run_suspension_waitpoints
		 WHERE org_id = $1
		   AND waitpoint_id = $2
	`, ids.orgID, channelWaitpointID).Scan(&runSuspensionID); err != nil {
		t.Fatal(err)
	}
	tokenWaitpointID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO waitpoints (id, org_id, project_id, environment_id, run_id, kind, status, params, error, resolved_at)
		VALUES ($1, $2, $3, $4, $5, 'token', 'timed_out', '{"token_id":"token-1"}', '{"code":"timed_out"}'::jsonb, now())
	`, tokenWaitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspension_waitpoints (
			org_id, run_id, project_id, environment_id, run_suspension_id, waitpoint_id, ordinal, timeout_seconds
		)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 1)
	`, ids.orgID, ids.runID, ids.projectID, ids.environmentID, runSuspensionID, tokenWaitpointID); err != nil {
		t.Fatal(err)
	}
	resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
		OrgID:       pgvalue.UUID(ids.orgID),
		WaitpointID: pgvalue.UUID(tokenWaitpointID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed) != 1 {
		t.Fatalf("resumed rows = %d, want 1", len(resumed))
	}
	if !resumed[0].ResolutionKind.Valid || resumed[0].ResolutionKind.String != "timed_out" {
		t.Fatalf("resolution kind = %+v, want timed_out", resumed[0].ResolutionKind)
	}
	if got := canonicalJSON(t, resumed[0].Resolution); got != `null` {
		t.Fatalf("resolution = %s, want null", got)
	}
}

func TestReleaseRunLeaseReleasesUnconsumedChannelMatch(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	_, runLeaseID, workerID := seedRunningTaskSessionLease(t, ctx, pool, ids)

	channelID := seedInputChannel(t, ctx, pool, ids, "approval")
	record, err := queries.AppendChannelRecord(ctx, db.AppendChannelRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		ChannelID:              pgvalue.UUID(channelID),
		Direction:              db.ChannelDirectionInput,
		Data:                   []byte(`{"approved":true}`),
		CorrelationID:          "",
		ContentType:            "application/json",
		IdempotencyFingerprint: "release-unconsumed-match",
		Actor:                  []byte(`{"type":"test"}`),
		Source:                 "test",
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}

	checkpointID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO checkpoints (id, org_id, run_id, project_id, environment_id, run_lease_id, status, reason, manifest, ready_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'ready', 'waitpoint', '{}'::jsonb, now())
	`, checkpointID, ids.orgID, ids.runID, ids.projectID, ids.environmentID, runLeaseID); err != nil {
		t.Fatal(err)
	}
	waitpointID := uuid.Must(uuid.NewV7())
	runSuspensionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO waitpoints (id, org_id, project_id, environment_id, run_id, kind, status, params, data)
		VALUES ($1, $2, $3, $4, $5, 'channel', 'completed', '{"channel":"approval","after_sequence":0}'::jsonb, '{"approved":true}'::jsonb)
	`, waitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspensions (
			id, org_id, run_id, project_id, environment_id, run_lease_id, checkpoint_id,
			correlation_id, status, waiting_at, resolution_kind, resolution
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'aggregate-1', 'resuming', now(), 'waitpoints', '{"waitpoints":[{"approved":true}]}'::jsonb)
	`, runSuspensionID, ids.orgID, ids.runID, ids.projectID, ids.environmentID, runLeaseID, checkpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspension_waitpoints (org_id, run_id, project_id, environment_id, run_suspension_id, waitpoint_id, ordinal)
		VALUES ($1, $2, $3, $4, $5, $6, 0)
	`, ids.orgID, ids.runID, ids.projectID, ids.environmentID, runSuspensionID, waitpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_waits (
			waitpoint_id, org_id, project_id, environment_id, run_id, run_suspension_id, channel_id,
			after_sequence, matched_record_id, matched_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0, $8, now())
	`, waitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, runSuspensionID, channelID, pgvalue.MustUUIDValue(record.ID)); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		RunLeaseID:              pgvalue.UUID(runLeaseID),
		WorkerInstanceID:        pgvalue.UUID(workerID),
		DispatchMessageID:       "dispatch-" + runLeaseID.String()[:8],
		DispatchLeaseID:         "lease-" + runLeaseID.String()[:8],
		OrgID:                   pgvalue.UUID(ids.orgID),
		RunID:                   pgvalue.UUID(ids.runID),
		RunStatus:               db.RunStatusFailed,
		AttemptStatus:           db.RunAttemptStatusFailed,
		ExitCode:                pgtype.Int4{Int32: 1, Valid: true},
		Output:                  []byte(`null`),
		ErrorMessage:            pgvalue.Text("schema validation failed"),
		TerminalEventKind:       "run.failed",
		TerminalEventPayload:    []byte(`{"failure_kind":"task_failed"}`),
		ReleaseActiveDurationMs: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var matchedRecordID pgtype.UUID
	var matchedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT matched_record_id, matched_at
		  FROM channel_waits
		 WHERE org_id = $1
		   AND waitpoint_id = $2
	`, ids.orgID, waitpointID).Scan(&matchedRecordID, &matchedAt); err != nil {
		t.Fatal(err)
	}
	if matchedRecordID.Valid || matchedAt.Valid {
		t.Fatalf("released failed lease kept channel match: matched_record_id=%v matched_at=%v", matchedRecordID, matchedAt)
	}
}

func TestResolveChannelWaitpointsAssignsOneRecordPerWait(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	firstWaitpointID := seedChannelWaitpoint(t, ctx, pool, ids, "approval", 0)
	var runSuspensionID uuid.UUID
	var channelID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT run_suspension_waitpoints.run_suspension_id, channel_waits.channel_id
		  FROM run_suspension_waitpoints
		  JOIN channel_waits ON channel_waits.org_id = run_suspension_waitpoints.org_id
		                    AND channel_waits.waitpoint_id = run_suspension_waitpoints.waitpoint_id
		 WHERE run_suspension_waitpoints.org_id = $1
		   AND run_suspension_waitpoints.waitpoint_id = $2
	`, ids.orgID, firstWaitpointID).Scan(&runSuspensionID, &channelID); err != nil {
		t.Fatal(err)
	}
	secondWaitpointID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO waitpoints (id, org_id, project_id, environment_id, run_id, kind, params)
		VALUES ($1, $2, $3, $4, $5, 'channel', '{"channel":"approval","after_sequence":0}'::jsonb)
	`, secondWaitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspension_waitpoints (
			org_id, run_id, project_id, environment_id, run_suspension_id, waitpoint_id, ordinal
		)
		VALUES ($1, $2, $3, $4, $5, $6, 1)
	`, ids.orgID, ids.runID, ids.projectID, ids.environmentID, runSuspensionID, secondWaitpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_waits (
			waitpoint_id, org_id, project_id, environment_id, run_id, run_suspension_id, channel_id, after_sequence, correlation_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 0, 'thread-1')
	`, secondWaitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, runSuspensionID, channelID); err != nil {
		t.Fatal(err)
	}
	firstRecord, err := queries.AppendChannelRecord(ctx, db.AppendChannelRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		ChannelID:              pgvalue.UUID(channelID),
		Direction:              db.ChannelDirectionInput,
		Data:                   []byte(`{"step":1}`),
		CorrelationID:          "thread-1",
		ContentType:            "application/json",
		IdempotencyFingerprint: "multi-channel-1",
		Actor:                  []byte(`{"type":"test"}`),
		Source:                 "test",
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondRecord, err := queries.AppendChannelRecord(ctx, db.AppendChannelRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		ProjectID:              pgvalue.UUID(ids.projectID),
		EnvironmentID:          pgvalue.UUID(ids.environmentID),
		ChannelID:              pgvalue.UUID(channelID),
		Direction:              db.ChannelDirectionInput,
		Data:                   []byte(`{"step":2}`),
		CorrelationID:          "thread-1",
		ContentType:            "application/json",
		IdempotencyFingerprint: "multi-channel-2",
		Actor:                  []byte(`{"type":"test"}`),
		Source:                 "test",
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := queries.ResolveChannelWaitpointsForChannel(ctx, db.ResolveChannelWaitpointsForChannelParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		ChannelID: pgvalue.UUID(channelID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != firstWaitpointID {
		t.Fatalf("first resolve completed = %+v, want %s", completed, firstWaitpointID)
	}
	completed, err = queries.ResolveChannelWaitpointsForChannel(ctx, db.ResolveChannelWaitpointsForChannelParams{
		OrgID:     pgvalue.UUID(ids.orgID),
		ChannelID: pgvalue.UUID(channelID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || pgvalue.MustUUIDValue(completed[0].ID) != secondWaitpointID {
		t.Fatalf("second resolve completed = %+v, want %s", completed, secondWaitpointID)
	}
	var firstMatched uuid.UUID
	var secondMatched uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT first_wait.matched_record_id, second_wait.matched_record_id
		  FROM channel_waits first_wait
		  JOIN channel_waits second_wait ON second_wait.org_id = first_wait.org_id
		                               AND second_wait.waitpoint_id = $3
		 WHERE first_wait.org_id = $1
		   AND first_wait.waitpoint_id = $2
	`, ids.orgID, firstWaitpointID, secondWaitpointID).Scan(&firstMatched, &secondMatched); err != nil {
		t.Fatal(err)
	}
	if firstMatched != pgvalue.MustUUIDValue(firstRecord.ID) || secondMatched != pgvalue.MustUUIDValue(secondRecord.ID) {
		t.Fatalf("matched records = (%s, %s), want (%s, %s)", firstMatched, secondMatched, pgvalue.MustUUIDValue(firstRecord.ID), pgvalue.MustUUIDValue(secondRecord.ID))
	}
}

func TestMarkRunSuspensionCheckpointReadyReleasesWorkspaceWriteLease(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	_, runLeaseID, workerID := seedRunningTaskSessionLease(t, ctx, pool, ids)
	var runtimeID string
	if err := pool.QueryRow(ctx, `
		SELECT runtime_id
		  FROM run_leases
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, runLeaseID).Scan(&runtimeID); err != nil {
		t.Fatal(err)
	}
	workspaceLeaseID := seedWorkspaceWriteLease(t, ctx, pool, ids, workerID)
	waitpoint, err := queries.CreateRunSuspensionForWaitpoint(ctx, db.CreateRunSuspensionForWaitpointParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Kind:             db.WaitpointKindToken,
		CorrelationID:    "wait-lease-release",
		CheckpointID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		CheckpointReason: "waitpoint",
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Params:           []byte(`{"token_id":"token-1"}`),
		Metadata:         []byte(`{}`),
		Tags:             []string{},
		RunSuspensionID:  pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := queries.MarkRunSuspensionCheckpointReady(ctx, db.MarkRunSuspensionCheckpointReadyParams{
		OrgID:                      pgvalue.UUID(ids.orgID),
		RunID:                      pgvalue.UUID(ids.runID),
		RunLeaseID:                 pgvalue.UUID(runLeaseID),
		WorkerInstanceID:           pgvalue.UUID(workerID),
		RunSuspensionID:            waitpoint.RunSuspensionID,
		CheckpointID:               waitpoint.CheckpointID,
		WaitpointID:                waitpoint.ID,
		RuntimeBackend:             "firecracker",
		RuntimeID:                  runtimeID,
		RuntimeArch:                "arm64",
		RuntimeABI:                 "test",
		KernelDigest:               "sha256:kernel",
		InitramfsDigest:            "sha256:initramfs",
		RootfsDigest:               "sha256:rootfs",
		CniProfile:                 "default",
		CheckpointArtifacts:        []byte(`[{"role":"runtime_config","ordinal":0,"digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111","size_bytes":1,"media_type":"application/json"}]`),
		WorkspaceArtifactDigest:    pgvalue.Text("sha256:2222222222222222222222222222222222222222222222222222222222222222"),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: 2, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		Manifest:                   []byte(`{"version":1}`),
		RuntimeVcpus:               pgtype.Int4{Int32: 1, Valid: true},
		RuntimeMemoryMib:           pgtype.Int4{Int32: 1024, Valid: true},
		RuntimeScratchDiskMib:      pgtype.Int4{Int32: 4096, Valid: true},
		ImageKey:                   pgvalue.Text("test-image"),
		WorkspaceArtifactEncoding:  pgvalue.Text("tar"),
		WorkspaceMountPath:         pgvalue.Text("/workspace"),
		ActiveDurationMs:           10,
		CheckpointPayload:          []byte(`{"waitpoint":"token-1"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != db.RunSuspensionStatusWaiting {
		t.Fatalf("run suspension status = %s, want waiting", ready.Status)
	}
	var releasedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT released_at
		  FROM workspace_leases
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, workspaceLeaseID).Scan(&releasedAt); err != nil {
		t.Fatal(err)
	}
	if !releasedAt.Valid {
		t.Fatal("workspace write lease released_at is null after checkpoint ready")
	}
	_ = seedWorkspaceWriteLease(t, ctx, pool, ids, workerID)
}

func TestRunTaskSessionIDCannotBeCleared(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET task_session_id = NULL
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID); err != nil {
		return
	}
	t.Fatal("clearing runs.task_session_id succeeded")
}

func TestAppendExecutionChannelRecordUsesTaskSessionOutputChannel(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	taskSessionID, runLeaseID, workerID := seedRunningTaskSessionLease(t, ctx, pool, ids)
	record, err := queries.AppendExecutionChannelRecord(ctx, db.AppendExecutionChannelRecordParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Channel:          "status",
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Payload:          []byte(`{"ok":true}`),
		ContentType:      "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}
	var channelTaskSessionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT task_session_id
		  FROM channels
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(record.ChannelID)).Scan(&channelTaskSessionID); err != nil {
		t.Fatal(err)
	}
	if channelTaskSessionID != taskSessionID {
		t.Fatalf("output channel task_session_id = %s, want %s", channelTaskSessionID, taskSessionID)
	}
	channelRecords, err := queries.ListChannelRecords(ctx, db.ListChannelRecordsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ChannelID:     record.ChannelID,
		Direction:     db.ChannelDirectionOutput,
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(channelRecords) != 1 || pgvalue.MustUUIDValue(channelRecords[0].ID) != pgvalue.MustUUIDValue(record.ID) {
		t.Fatalf("public channel records = %+v, want %s", channelRecords, pgvalue.MustUUIDValue(record.ID))
	}
}

func TestAppendExecutionChannelRecordRejectsNonCurrentSessionRun(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	taskSessionID, runLeaseID, workerID := seedRunningTaskSessionLease(t, ctx, pool, ids)
	nextRunID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			task_session_id, status, execution_status, payload, queue_name, max_duration_seconds, trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approval-task', $8, 'queued', 'queued', '{}', 'default', 300,
			'00000000000000000000000000000002', '0000000000000002')
	`, nextRunID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE task_sessions
		   SET current_run_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, nextRunID, ids.orgID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	_, err := queries.AppendExecutionChannelRecord(ctx, db.AppendExecutionChannelRecordParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Channel:          "status",
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Payload:          []byte(`{"stale":true}`),
		ContentType:      "application/json",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("append stale session run error = %v, want pgx.ErrNoRows", err)
	}
}

func TestReleaseRunLeaseFailsTaskSessionWhenResultTooLarge(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	taskSessionID, runLeaseID, workerID := seedRunningTaskSessionLease(t, ctx, pool, ids)
	seedTaskSessionRun(t, ctx, pool, ids, taskSessionID)
	workspaceLeaseID := seedWorkspaceWriteLease(t, ctx, pool, ids, workerID)
	baseVersionID := currentWorkspaceVersionID(t, ctx, pool, ids)
	largeOutput := []byte(`"` + strings.Repeat("x", 1048577) + `"`)

	_, err := queries.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		RunLeaseID:                  pgvalue.UUID(runLeaseID),
		WorkerInstanceID:            pgvalue.UUID(workerID),
		DispatchMessageID:           "dispatch-" + runLeaseID.String()[:8],
		DispatchLeaseID:             "lease-" + runLeaseID.String()[:8],
		OrgID:                       pgvalue.UUID(ids.orgID),
		RunID:                       pgvalue.UUID(ids.runID),
		RunStatus:                   db.RunStatusSucceeded,
		WorkspaceLeaseID:            pgvalue.UUID(workspaceLeaseID),
		WorkspaceBaseVersionID:      pgvalue.UUID(baseVersionID),
		WorkspaceFencingToken:       pgtype.Text{String: "fence-" + shortUUID(workspaceLeaseID), Valid: true},
		WorkspaceArtifactDigest:     pgvalue.Text("sha256:" + strings.Repeat("c", 64)),
		WorkspaceArtifactSizeBytes:  pgtype.Int8{Int64: 123, Valid: true},
		WorkspaceArtifactMediaType:  pgvalue.Text("application/vnd.helmr.workspace.v0.tar"),
		WorkspaceArtifactEncoding:   pgvalue.Text("tar"),
		WorkspaceArtifactEntryCount: pgtype.Int4{Int32: 2, Valid: true},
		WorkspaceMountPath:          pgvalue.Text("/workspace"),
		AttemptStatus:               db.RunAttemptStatusSucceeded,
		ExitCode:                    pgtype.Int4{Int32: 0, Valid: true},
		Output:                      largeOutput,
		TerminalEventKind:           "run.completed",
		TerminalEventPayload:        []byte(`{}`),
		ReleaseActiveDurationMs:     1,
	})
	if err != nil {
		t.Fatal(err)
	}

	var status db.TaskSessionStatus
	var completedAt pgtype.Timestamptz
	var failedAt pgtype.Timestamptz
	var result []byte
	var terminalReason []byte
	if err := pool.QueryRow(ctx, `
		SELECT status, completed_at, failed_at, result, terminal_reason
		  FROM task_sessions
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, taskSessionID).Scan(&status, &completedAt, &failedAt, &result, &terminalReason); err != nil {
		t.Fatal(err)
	}
	if status != db.TaskSessionStatusFailed {
		t.Fatalf("task session status = %s, want failed", status)
	}
	if completedAt.Valid {
		t.Fatalf("completed_at should be null for oversized result, got %+v", completedAt)
	}
	if !failedAt.Valid {
		t.Fatal("failed_at should be set for oversized result")
	}
	if !strings.Contains(string(result), `"ResultTooLarge"`) {
		t.Fatalf("result = %s, want ResultTooLarge", string(result))
	}
	if !strings.Contains(string(terminalReason), `"ResultTooLarge"`) {
		t.Fatalf("terminal_reason = %s, want ResultTooLarge", string(terminalReason))
	}
	var currentVersionID pgtype.UUID
	var workspaceVersionCount int
	if err := pool.QueryRow(ctx, `
		SELECT current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&currentVersionID); err != nil {
		t.Fatal(err)
	}
	if !currentVersionID.Valid || pgvalue.MustUUIDValue(currentVersionID) != baseVersionID {
		t.Fatalf("workspace current_version_id = %+v, want base version %s for oversized session result", currentVersionID, baseVersionID)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM workspace_versions
		 WHERE org_id = $1
		   AND workspace_id = $2
	`, ids.orgID, ids.workspaceID).Scan(&workspaceVersionCount); err != nil {
		t.Fatal(err)
	}
	if workspaceVersionCount != 1 {
		t.Fatalf("workspace versions = %d, want only base version for oversized session result", workspaceVersionCount)
	}
}

func TestAppendExecutionChannelRecordUsesSessionOutputChannel(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	taskSessionID := seedTaskSessionForRun(t, ctx, pool, ids)
	runLeaseID, workerID := seedRunningRunLease(t, ctx, pool, ids)
	record, err := queries.AppendExecutionChannelRecord(ctx, db.AppendExecutionChannelRecordParams{
		OrgID:            pgvalue.UUID(ids.orgID),
		RunID:            pgvalue.UUID(ids.runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(workerID),
		Channel:          "status",
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Payload:          []byte(`{"ok":true}`),
		ContentType:      "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}
	var channelTaskSessionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT task_session_id
		  FROM channels
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, pgvalue.MustUUIDValue(record.ChannelID)).Scan(&channelTaskSessionID); err != nil {
		t.Fatal(err)
	}
	if channelTaskSessionID != taskSessionID {
		t.Fatalf("output channel task_session_id = %s, want %s", channelTaskSessionID, taskSessionID)
	}
	channelRecords, err := queries.ListChannelRecords(ctx, db.ListChannelRecordsParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ChannelID:     record.ChannelID,
		Direction:     db.ChannelDirectionOutput,
		LimitCount:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(channelRecords) != 1 || pgvalue.MustUUIDValue(channelRecords[0].ID) != pgvalue.MustUUIDValue(record.ID) {
		t.Fatalf("public channel records = %+v, want %s", channelRecords, pgvalue.MustUUIDValue(record.ID))
	}
}

func TestLockTaskSessionForChannelAppendReturnsTerminalSession(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	taskSessionID, _, _ := seedRunningTaskSessionLease(t, ctx, pool, ids)
	channelID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO channels (id, org_id, project_id, environment_id, task_session_id, definition_id, name, direction)
		VALUES ($1, $2, $3, $4, $5, $6, 'approval', 'input')
	`, channelID, ids.orgID, ids.projectID, ids.environmentID, taskSessionID, seedChannelDefinition(t, ctx, pool, ids, "approval", "input")); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.LockTaskSessionForChannelAppend(ctx, db.LockTaskSessionForChannelAppendParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ChannelID:     pgvalue.UUID(channelID),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE task_sessions
		   SET status = 'closed',
		       closed_at = now()
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	lockedSession, err := queries.LockTaskSessionForChannelAppend(ctx, db.LockTaskSessionForChannelAppendParams{
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		ChannelID:     pgvalue.UUID(channelID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lockedSession.Status != db.TaskSessionStatusClosed {
		t.Fatalf("terminal session status = %s, want closed", lockedSession.Status)
	}
}

func TestAppendSessionChannelInputIdempotencyReturnsExistingInput(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	seedTaskSessionForRun(t, ctx, pool, ids)

	first, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("duplicate idempotency key returned different input: %s != %s", pgvalue.MustUUIDValue(first.ID), pgvalue.MustUUIDValue(second.ID))
	}
	if got := canonicalJSON(t, second.Data); got != `{"approved":true}` {
		t.Fatalf("duplicate idempotency key rewrote data = %s", got)
	}
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM channel_records
		  JOIN channels ON channels.org_id = channel_records.org_id
		               AND channels.id = channel_records.channel_id
		  JOIN runs ON runs.org_id = channels.org_id
		           AND runs.task_session_id = channels.task_session_id
		 WHERE channel_records.org_id = $1
		   AND runs.id = $2
		   AND channels.name = 'approval'
		   AND channels.direction = 'input'
		   AND channel_records.idempotency_key = 'external-action-1'
	`, ids.orgID, ids.runID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("input count for idempotency key = %d, want 1", count)
	}
}

func TestAppendSessionChannelInputIdempotencyFingerprintMismatchReturnsNoRows(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	seedTaskSessionForRun(t, ctx, pool, ids)

	if _, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "different-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("fingerprint mismatch error = %v, want pgx.ErrNoRows", err)
	}
	reason, err := queries.GetSessionChannelInputAppendConflictReason(ctx, db.GetSessionChannelInputAppendConflictReasonParams{
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "different-fingerprint",
		MaxInputsPerChannel:    1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reason != "idempotency_conflict" {
		t.Fatalf("append conflict reason = %q, want idempotency_conflict", reason)
	}
}

func TestAppendSessionChannelInputExternalEventIDReturnsExistingInput(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	seedTaskSessionForRun(t, ctx, pool, ids)

	first, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		ExternalEventID:        "external-event-1",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		ExternalEventID:        "external-event-1",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.Inserted {
		t.Fatalf("external event duplicate returned row id=%s inserted=%v, want existing %s", pgvalue.MustUUIDValue(second.ID), second.Inserted, pgvalue.MustUUIDValue(first.ID))
	}
	if got := canonicalJSON(t, second.Data); got != `{"approved":true}` {
		t.Fatalf("duplicate external event rewrote data = %s", got)
	}

	_, err = queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "different-fingerprint",
		ExternalEventID:        "external-event-1",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("external event fingerprint mismatch error = %v, want pgx.ErrNoRows", err)
	}
	reason, err := queries.GetSessionChannelInputAppendConflictReason(ctx, db.GetSessionChannelInputAppendConflictReasonParams{
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "different-fingerprint",
		ExternalEventID:        "external-event-1",
		MaxInputsPerChannel:    1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reason != "idempotency_conflict" {
		t.Fatalf("append conflict reason = %q, want idempotency_conflict", reason)
	}
}

func TestAppendSessionChannelInputRejectsAmbiguousIdempotencyIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	seedTaskSessionForRun(t, ctx, pool, ids)

	if _, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"source":"idempotency"}`),
		ContentType:            "application/json",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "same-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"source":"event"}`),
		ContentType:            "application/json",
		IdempotencyFingerprint: "same-fingerprint",
		ExternalEventID:        "external-event-1",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"source":"both"}`),
		ContentType:            "application/json",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "same-fingerprint",
		ExternalEventID:        "external-event-1",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ambiguous duplicate append error = %v, want pgx.ErrNoRows", err)
	}
	reason, err := queries.GetSessionChannelInputAppendConflictReason(ctx, db.GetSessionChannelInputAppendConflictReasonParams{
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		IdempotencyKey:         "external-action-1",
		IdempotencyFingerprint: "same-fingerprint",
		ExternalEventID:        "external-event-1",
		MaxInputsPerChannel:    1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reason != "idempotency_conflict" {
		t.Fatalf("append conflict reason = %q, want idempotency_conflict", reason)
	}
}

func TestAppendSessionChannelInputRejectsStaleCurrentRun(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	sessionID := seedTaskSessionForRun(t, ctx, pool, ids)
	replacementRunID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id,
			task_id, task_session_id, status, execution_status, payload, metadata, locked_retry_policy,
			queue_name, max_duration_seconds, trace_id, root_span_id, created_at, updated_at
		)
		SELECT $1, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id,
		       task_id, task_session_id, 'queued', 'queued', '{}', '{}', '{}',
		       queue_name, max_duration_seconds, '00000000000000000000000000000001', '0000000000000001', now(), now()
		  FROM runs
		 WHERE id = $2
	`, replacementRunID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE task_sessions
		   SET current_run_id = $1,
		       current_run_version = current_run_version + 1
		 WHERE id = $2
	`, replacementRunID, sessionID); err != nil {
		t.Fatal(err)
	}

	_, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale current run append error = %v, want pgx.ErrNoRows", err)
	}
	reason, err := queries.GetSessionChannelInputAppendConflictReason(ctx, db.GetSessionChannelInputAppendConflictReasonParams{
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reason != "run_not_accepting" {
		t.Fatalf("append conflict reason = %q, want run_not_accepting", reason)
	}
}

func TestSessionChannelInputAppendConflictReasonReturnsInputLimit(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	seedTaskSessionForRun(t, ctx, pool, ids)

	if _, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "first-fingerprint",
		MaxInputsPerChannel:    1,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "second-fingerprint",
		MaxInputsPerChannel:    1,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("input limit error = %v, want pgx.ErrNoRows", err)
	}
	var nextSequence int64
	if err := pool.QueryRow(ctx, `
		SELECT channels.next_sequence
		  FROM channels
		  JOIN task_sessions ON task_sessions.id = channels.task_session_id
		 WHERE task_sessions.current_run_id = $1
		   AND channels.name = 'approval'
		   AND channels.direction = 'input'
	`, ids.runID).Scan(&nextSequence); err != nil {
		t.Fatal(err)
	}
	if nextSequence != 2 {
		t.Fatalf("next_sequence = %d, want 2 after rejected second append", nextSequence)
	}
	reason, err := queries.GetSessionChannelInputAppendConflictReason(ctx, db.GetSessionChannelInputAppendConflictReasonParams{
		OrgID:                  pgvalue.UUID(ids.orgID),
		RunID:                  pgvalue.UUID(ids.runID),
		Channel:                "approval",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "second-fingerprint",
		MaxInputsPerChannel:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reason != "input_limit_exceeded" {
		t.Fatalf("append conflict reason = %q, want input_limit_exceeded", reason)
	}
}

func TestTaskStartIdempotencyExpiredKeyCanBeReused(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	sessionID := seedTaskSessionForRun(t, ctx, pool, ids)

	params := db.CreateTaskStartIdempotencyParams{
		ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:              pgvalue.UUID(ids.orgID),
		ProjectID:          pgvalue.UUID(ids.projectID),
		EnvironmentID:      pgvalue.UUID(ids.environmentID),
		TaskID:             "approval-task",
		IdempotencyKey:     "start-key-1",
		RequestFingerprint: "fingerprint-1",
		TaskSessionID:      pgvalue.UUID(sessionID),
		FirstRunID:         pgvalue.UUID(ids.runID),
		ExpiresAt:          pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
	}
	if _, err := queries.CreateTaskStartIdempotency(ctx, params); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	txQueries := db.New(tx)
	params.ID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	params.RequestFingerprint = "fingerprint-conflict"
	params.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if _, err := txQueries.CreateTaskStartIdempotency(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("conflicting insert err = %v, want no rows without aborting transaction", err)
	}
	if err := txQueries.DeleteExpiredTaskStartIdempotency(ctx, db.DeleteExpiredTaskStartIdempotencyParams{
		OrgID:          params.OrgID,
		ProjectID:      params.ProjectID,
		EnvironmentID:  params.EnvironmentID,
		TaskID:         params.TaskID,
		IdempotencyKey: params.IdempotencyKey,
	}); err != nil {
		t.Fatal(err)
	}
	params.ID = pgvalue.UUID(uuid.Must(uuid.NewV7()))
	params.RequestFingerprint = "fingerprint-2"
	params.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if _, err := txQueries.CreateTaskStartIdempotency(ctx, params); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx = nil
	row, err := queries.GetTaskStartIdempotency(ctx, db.GetTaskStartIdempotencyParams{
		OrgID:          params.OrgID,
		ProjectID:      params.ProjectID,
		EnvironmentID:  params.EnvironmentID,
		TaskID:         params.TaskID,
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if row.RequestFingerprint != "fingerprint-2" {
		t.Fatalf("request fingerprint = %q, want fingerprint-2", row.RequestFingerprint)
	}
}

func TestAppendSessionChannelInputSequenceIsScopedToRunAndChannel(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	firstRun := seedWaitpointTokenIntegration(t, ctx, pool)
	secondRun := seedWaitpointTokenIntegration(t, ctx, pool)
	seedTaskSessionForRun(t, ctx, pool, firstRun)
	seedTaskSessionForRun(t, ctx, pool, secondRun)

	firstApproval, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(firstRun.orgID),
		RunID:                  pgvalue.UUID(firstRun.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondApproval, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(firstRun.orgID),
		RunID:                  pgvalue.UUID(firstRun.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":false}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstMessage, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(firstRun.orgID),
		RunID:                  pgvalue.UUID(firstRun.runID),
		Channel:                "message",
		Data:                   []byte(`{"text":"first"}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherRunApproval, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  pgvalue.UUID(secondRun.orgID),
		RunID:                  pgvalue.UUID(secondRun.runID),
		Channel:                "approval",
		Data:                   []byte(`{"approved":true}`),
		ContentType:            "application/json",
		IdempotencyKey:         "",
		IdempotencyFingerprint: "test-fingerprint",
		MaxInputsPerChannel:    1024,
		AuthSubjectType:        db.ChannelRecordAuthSubjectTypeSystem,
	})
	if err != nil {
		t.Fatal(err)
	}

	if firstApproval.Sequence != 1 || secondApproval.Sequence != 2 {
		t.Fatalf("first run approval sequences = %d, %d; want 1, 2", firstApproval.Sequence, secondApproval.Sequence)
	}
	if firstMessage.Sequence != 1 {
		t.Fatalf("first run message sequence = %d, want 1", firstMessage.Sequence)
	}
	if otherRunApproval.Sequence != 1 {
		t.Fatalf("second run approval sequence = %d, want 1", otherRunApproval.Sequence)
	}
}

func TestExpireDuePendingWaitpointsPublishesNullTimeoutData(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	ids := seedWaitpointTokenIntegration(t, ctx, pool)
	queries := db.New(pool)
	waitpointID := seedWaitingRunSuspensionForWaitpoint(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		WITH target_dependency AS (
		    UPDATE run_suspension_waitpoints
		       SET timeout_seconds = 1
		     WHERE org_id = $1
		       AND waitpoint_id = $2
		    RETURNING run_suspension_id
		)
		UPDATE run_suspensions
		   SET waiting_at = now() - interval '2 seconds'
		  FROM target_dependency
		 WHERE run_suspensions.org_id = $1
		   AND run_suspensions.id = target_dependency.run_suspension_id
	`, ids.orgID, waitpointID); err != nil {
		t.Fatal(err)
	}
	if err := queries.ExpireDuePendingWaitpoints(ctx, pgvalue.UUID(ids.orgID)); err != nil {
		t.Fatal(err)
	}
	var resolutionKind string
	var resolution []byte
	if err := pool.QueryRow(ctx, `
		SELECT run_suspensions.resolution_kind, run_suspensions.resolution
		  FROM run_suspensions
		  JOIN run_suspension_waitpoints ON run_suspension_waitpoints.org_id = run_suspensions.org_id
		                            AND run_suspension_waitpoints.run_suspension_id = run_suspensions.id
		 WHERE run_suspensions.org_id = $1
		   AND run_suspension_waitpoints.waitpoint_id = $2
	`, ids.orgID, waitpointID).Scan(&resolutionKind, &resolution); err != nil {
		t.Fatal(err)
	}
	if resolutionKind != "timed_out" {
		t.Fatalf("resolution kind = %s", resolutionKind)
	}
	if got := canonicalJSON(t, resolution); got != `null` {
		t.Fatalf("run suspension resolution = %s", resolution)
	}
}

type waitpointTokenIntegrationIDs struct {
	orgID               uuid.UUID
	projectID           uuid.UUID
	environmentID       uuid.UUID
	deploymentID        uuid.UUID
	deploymentSandboxID uuid.UUID
	workspaceID         uuid.UUID
	taskID              uuid.UUID
	runID               uuid.UUID
}

func shortUUID(id uuid.UUID) string {
	compact := strings.ReplaceAll(id.String(), "-", "")
	return compact[len(compact)-12:]
}

func seedWorkspaceExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, materialization db.WorkspaceMaterialization) uuid.UUID {
	t.Helper()
	execID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_execs (
			id, org_id, project_id, environment_id, workspace_id,
			materialization_id, command, state
		)
		VALUES ($1, $2, $3, $4, $5, $6, '{"cmd":["true"]}'::jsonb, 'queued')
	`, execID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, pgvalue.MustUUIDValue(materialization.ID)); err != nil {
		t.Fatal(err)
	}
	return execID
}

func seedWorkspaceExecWithActiveWriteLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, materialization db.WorkspaceMaterialization) (pgtype.UUID, db.WorkspaceLease) {
	t.Helper()
	execID := pgvalue.UUID(seedWorkspaceExec(t, ctx, pool, ids, materialization))
	queries := db.New(pool)
	lease, err := queries.AcquireWorkspaceWriteLease(ctx, db.AcquireWorkspaceWriteLeaseParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OwnerExecID:       execID,
		OwnerPtySessionID: pgtype.UUID{},
		FencingToken:      "exec-fence-" + shortUUID(pgvalue.MustUUIDValue(execID)),
		HeartbeatToken:    "exec-heartbeat-" + shortUUID(pgvalue.MustUUIDValue(execID)),
		ExpiresAt:         pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		OrgID:             pgvalue.UUID(ids.orgID),
		ProjectID:         pgvalue.UUID(ids.projectID),
		EnvironmentID:     pgvalue.UUID(ids.environmentID),
		WorkspaceID:       pgvalue.UUID(ids.workspaceID),
		MaterializationID: materialization.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_execs
		   SET write_lease_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, pgvalue.MustUUIDValue(lease.ID), ids.orgID, pgvalue.MustUUIDValue(execID)); err != nil {
		t.Fatal(err)
	}
	return execID, lease
}

func seedLiveWorkspacePrimitiveRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, materialization db.WorkspaceMaterialization) (uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	execID := uuid.Must(uuid.NewV7())
	ptyID := uuid.Must(uuid.NewV7())
	portID := uuid.Must(uuid.NewV7())
	leaseID := uuid.Must(uuid.NewV7())
	baseVersionID := currentWorkspaceVersionID(t, ctx, pool, ids)
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_execs (
			id, org_id, project_id, environment_id, workspace_id,
			materialization_id, command, state, process_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, '{"cmd":["sleep","60"]}'::jsonb, 'running', 'exec-process')
	`, execID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, pgvalue.MustUUIDValue(materialization.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_pty_sessions (
			id, org_id, project_id, environment_id, workspace_id,
			materialization_id, cwd, cols, rows, state, process_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, '/workspace', 80, 24, 'open', 'pty-process')
	`, ptyID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, pgvalue.MustUUIDValue(materialization.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_ports (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			owner_exec_id, port, state, url
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 3000, 'open', 'https://example.test')
	`, portID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, pgvalue.MustUUIDValue(materialization.ID), execID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			lease_kind, state, owner_exec_id, base_version_id, acquired_version_id,
			acquired_fencing_generation, fencing_token, heartbeat_token, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'write', 'active', $7, $8, $8, 1, $9, $10, now() + interval '1 hour')
	`, leaseID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, pgvalue.MustUUIDValue(materialization.ID), execID, baseVersionID, "fence-"+shortUUID(leaseID), "heartbeat-"+shortUUID(leaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspace_execs
		   SET write_lease_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, leaseID, ids.orgID, execID); err != nil {
		t.Fatal(err)
	}
	return execID, ptyID, portID, leaseID
}

func currentWorkspaceVersionID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) uuid.UUID {
	t.Helper()
	var versionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT current_version_id
		  FROM workspaces
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND id = $4
	`, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID).Scan(&versionID); err != nil {
		t.Fatal(err)
	}
	return versionID
}

func seedWorkspaceWriteLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, workerID uuid.UUID) uuid.UUID {
	t.Helper()
	materializationID := uuid.Must(uuid.NewV7())
	leaseID := uuid.Must(uuid.NewV7())
	baseVersionID := currentWorkspaceVersionID(t, ctx, pool, ids)
	err := pool.QueryRow(ctx, `
		SELECT id
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND workspace_id = $4
		   AND worker_instance_id = $5
		   AND state IN ('requested', 'materializing', 'restoring', 'running', 'pausing', 'paused', 'capturing', 'stopping')
		 LIMIT 1
	`, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, workerID).Scan(&materializationID)
	if errors.Is(err, pgx.ErrNoRows) {
		var imageArtifactID uuid.UUID
		var rootfsDigest string
		var imageDigest string
		var workspaceVersionID uuid.UUID
		var workspaceArtifactID uuid.UUID
		var workspaceDigest string
		var workspaceSize int64
		var workspaceMediaType string
		if err := pool.QueryRow(ctx, `
			SELECT deployment_sandboxes.image_artifact_id,
			       deployment_sandboxes.rootfs_digest,
			       deployment_sandboxes.image_digest,
			       workspace_versions.id,
			       workspace_versions.artifact_id,
			       artifacts.digest,
			       artifacts.size_bytes,
			       artifacts.media_type
			  FROM workspaces
			  JOIN deployment_sandboxes
			    ON deployment_sandboxes.org_id = workspaces.org_id
			   AND deployment_sandboxes.project_id = workspaces.project_id
			   AND deployment_sandboxes.environment_id = workspaces.environment_id
			   AND deployment_sandboxes.id = workspaces.deployment_sandbox_id
			  JOIN workspace_versions
			    ON workspace_versions.org_id = workspaces.org_id
			   AND workspace_versions.project_id = workspaces.project_id
			   AND workspace_versions.environment_id = workspaces.environment_id
			   AND workspace_versions.workspace_id = workspaces.id
			   AND workspace_versions.id = workspaces.current_version_id
			  JOIN artifacts
			    ON artifacts.org_id = workspace_versions.org_id
			   AND artifacts.project_id = workspace_versions.project_id
			   AND artifacts.environment_id = workspace_versions.environment_id
			   AND artifacts.id = workspace_versions.artifact_id
			 WHERE workspaces.org_id = $1
			   AND workspaces.project_id = $2
			   AND workspaces.environment_id = $3
			   AND workspaces.id = $4
		`, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID).Scan(&imageArtifactID, &rootfsDigest, &imageDigest, &workspaceVersionID, &workspaceArtifactID, &workspaceDigest, &workspaceSize, &workspaceMediaType); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_materializations (
			id, org_id, project_id, environment_id, workspace_id, deployment_sandbox_id,
			base_version_id,
			sandbox_fingerprint, worker_instance_id, requested_cpu_millis, requested_memory_mib,
			requested_disk_mib, runtime_id,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_artifact_id, workspace_artifact_encoding, workspace_artifact_entry_count,
			workspace_artifact_digest, workspace_artifact_size_bytes, workspace_artifact_media_type, workspace_mount_path,
			runtime_abi, guestd_abi, adapter_abi,
			state, materialized_at, last_heartbeat_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'sandbox-fingerprint', $8, 1000, 1024, 4096,
			'test-runtime',
			$9, 'oci-tar', $10, $11, 'oci-tar',
			$12, $13, 0, $14, $15, $16, '/workspace',
			'test', 'guestd-test', 'adapter-test',
			'running', now(), now())
	`, materializationID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, ids.deploymentSandboxID, workspaceVersionID, workerID, imageArtifactID, rootfsDigest, imageDigest, workspaceArtifactID, workspace.ArtifactEncoding, workspaceDigest, workspaceSize, workspaceMediaType); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_leases (
			id, org_id, project_id, environment_id, workspace_id, materialization_id,
			lease_kind, state, owner_run_id, base_version_id, acquired_version_id, acquired_fencing_generation, fencing_token,
			heartbeat_token, expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'write', 'active', $7, $8, $8, 1, $9, $10, now() + interval '1 hour')
	`, leaseID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, materializationID, ids.runID, baseVersionID, "fence-"+shortUUID(leaseID), "heartbeat-"+shortUUID(leaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET workspace_materialization_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, materializationID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	return leaseID
}

func seedMaterializationWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	workerID := uuid.Must(uuid.NewV7())
	workerResourceID := "materialization-worker-" + shortUUID(workerID)
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, worker_group_id, resource_id, total_milli_cpu, total_memory_mib, total_disk_mib,
			total_execution_slots, available_milli_cpu, available_memory_mib, available_disk_mib,
			available_execution_slots, runtime_id, runtime_arch, runtime_abi, rootfs_digest
		)
		VALUES ($1, $2, $3, 2000, 2048, 4096, 1, 2000, 2048, 4096, 1,
			'runtime-test', 'arm64', 'test', 'sha256:rootfs')
	`, workerID, workerGroupID, workerResourceID); err != nil {
		t.Fatal(err)
	}
	return workerID
}

func requestWorkspaceMaterializationForTest(ctx context.Context, queries *db.Queries, arg db.EnsureWorkspaceMaterializationRequestedParams) (db.WorkspaceMaterialization, error) {
	row, err := queries.EnsureWorkspaceMaterializationRequested(ctx, arg)
	if err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	return db.WorkspaceMaterialization{
		ID:                          row.ID,
		OrgID:                       row.OrgID,
		ProjectID:                   row.ProjectID,
		EnvironmentID:               row.EnvironmentID,
		WorkspaceID:                 row.WorkspaceID,
		DeploymentSandboxID:         row.DeploymentSandboxID,
		SandboxFingerprint:          row.SandboxFingerprint,
		BaseVersionID:               row.BaseVersionID,
		WorkerInstanceID:            row.WorkerInstanceID,
		ReservationToken:            row.ReservationToken,
		ReservationExpiresAt:        row.ReservationExpiresAt,
		ClaimAttempt:                row.ClaimAttempt,
		DeadLetteredAt:              row.DeadLetteredAt,
		Priority:                    row.Priority,
		RequestedCpuMillis:          row.RequestedCpuMillis,
		RequestedMemoryMib:          row.RequestedMemoryMib,
		RequestedDiskMib:            row.RequestedDiskMib,
		RequestedExecutionSlots:     row.RequestedExecutionSlots,
		ReservedCpuMillis:           row.ReservedCpuMillis,
		ReservedMemoryMib:           row.ReservedMemoryMib,
		ReservedDiskMib:             row.ReservedDiskMib,
		ReservedExecutionSlots:      row.ReservedExecutionSlots,
		CapacityReservationID:       row.CapacityReservationID,
		GuestdChannelTokenHash:      row.GuestdChannelTokenHash,
		GuestdChannelTokenExpiresAt: row.GuestdChannelTokenExpiresAt,
		RuntimeID:                   row.RuntimeID,
		State:                       row.State,
		Request:                     row.Request,
		LeaseGeneration:             row.LeaseGeneration,
		DirtyGeneration:             row.DirtyGeneration,
		FencingGeneration:           row.FencingGeneration,
		NetworkNamespace:            row.NetworkNamespace,
		PortNamespace:               row.PortNamespace,
		ImageArtifactID:             row.ImageArtifactID,
		ImageArtifactFormat:         row.ImageArtifactFormat,
		RootfsDigest:                row.RootfsDigest,
		ImageDigest:                 row.ImageDigest,
		ImageFormat:                 row.ImageFormat,
		WorkspaceArtifactID:         row.WorkspaceArtifactID,
		WorkspaceArtifactEncoding:   row.WorkspaceArtifactEncoding,
		WorkspaceArtifactEntryCount: row.WorkspaceArtifactEntryCount,
		WorkspaceArtifactDigest:     row.WorkspaceArtifactDigest,
		WorkspaceArtifactSizeBytes:  row.WorkspaceArtifactSizeBytes,
		WorkspaceArtifactMediaType:  row.WorkspaceArtifactMediaType,
		WorkspaceMountPath:          row.WorkspaceMountPath,
		RuntimeABI:                  row.RuntimeABI,
		GuestdAbi:                   row.GuestdAbi,
		AdapterAbi:                  row.AdapterAbi,
		LastHeartbeatAt:             row.LastHeartbeatAt,
		RequestedAt:                 row.RequestedAt,
		MaterializedAt:              row.MaterializedAt,
		StoppedAt:                   row.StoppedAt,
		LostAt:                      row.LostAt,
		FailedAt:                    row.FailedAt,
		Error:                       row.Error,
		CreatedAt:                   row.CreatedAt,
		UpdatedAt:                   row.UpdatedAt,
	}, nil
}

func seedRunningWorkspaceMaterialization(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queries *db.Queries, ids waitpointTokenIntegrationIDs) db.WorkspaceMaterialization {
	t.Helper()
	workerID := seedMaterializationWorker(t, ctx, pool)
	requested, err := requestWorkspaceMaterializationForTest(ctx, queries, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(ids.orgID),
		ProjectID:     pgvalue.UUID(ids.projectID),
		EnvironmentID: pgvalue.UUID(ids.environmentID),
		WorkspaceID:   pgvalue.UUID(ids.workspaceID),
		Request:       []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var rootfsDigest string
	if err := pool.QueryRow(ctx, `
		SELECT rootfs_digest
		  FROM deployment_sandboxes
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND id = $4
	`, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentSandboxID).Scan(&rootfsDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ClaimWorkspaceMaterialization(ctx, db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      2000,
		AvailableMemoryMib:      2048,
		AvailableDiskMib:        4096,
		AvailableExecutionSlots: 1,
		RootfsDigest:            rootfsDigest,
		RuntimeABI:              "test",
		GuestdAbi:               "guestd-test",
		AdapterAbi:              "adapter-test",
		WorkerInstanceID:        pgvalue.UUID(workerID),
		ReservationToken:        "lease-test-token",
		ReservationExpiresAt:    pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		GuestdChannelTokenHash:  "channel-token-hash",
		RuntimeID:               "runtime-test",
	}); err != nil {
		t.Fatal(err)
	}
	running, err := queries.MarkWorkspaceMaterializationRunning(ctx, db.MarkWorkspaceMaterializationRunningParams{
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		OrgID:                pgvalue.UUID(ids.orgID),
		ID:                   requested.ID,
		WorkerInstanceID:     pgvalue.UUID(workerID),
		ReservationToken:     "lease-test-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return running
}

func seedWorkspaceVersionArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) uuid.UUID {
	t.Helper()
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("workspace-version")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (digest, size_bytes, media_type)
		VALUES ($1, 10, $2)
	`, digest, workspace.ArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'workspace_version', 10, $6)
	`, artifactID, ids.orgID, ids.projectID, ids.environmentID, digest, workspace.ArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	return artifactID
}

func assertWorkspaceDirtyGeneration(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, materializationID pgtype.UUID, want int64) {
	t.Helper()
	var dirtyGeneration int64
	if err := pool.QueryRow(ctx, `
		SELECT dirty_generation
		  FROM workspace_materializations
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, materializationID).Scan(&dirtyGeneration); err != nil {
		t.Fatal(err)
	}
	if dirtyGeneration != want {
		t.Fatalf("dirty_generation = %d, want %d", dirtyGeneration, want)
	}
	var dirtyState string
	if err := pool.QueryRow(ctx, `
		SELECT dirty_state::text
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&dirtyState); err != nil {
		t.Fatal(err)
	}
	if dirtyState != "dirty" {
		t.Fatalf("workspace dirty_state = %s, want dirty", dirtyState)
	}
}

func assertWorkspaceDesiredState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, want string) {
	t.Helper()
	var got string
	if err := pool.QueryRow(ctx, `
		SELECT desired_state::text
		  FROM workspaces
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.workspaceID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("workspace desired_state = %s, want %s", got, want)
	}
}

func seedSandboxImageArtifact(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) (uuid.UUID, string) {
	t.Helper()
	artifactID := uuid.Must(uuid.NewV7())
	digest := testDigest("sandbox-image")
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (digest, size_bytes, media_type)
		VALUES ($1, 6, $2)
	`, digest, api.SandboxImageArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'sandbox_image', 6, $6)
	`, artifactID, ids.orgID, ids.projectID, ids.environmentID, digest, api.SandboxImageArtifactMediaType); err != nil {
		t.Fatal(err)
	}
	return artifactID, digest
}

func deploymentSandboxRootfsDigest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) string {
	t.Helper()
	var digest string
	if err := pool.QueryRow(ctx, `
		SELECT rootfs_digest
		  FROM deployment_sandboxes
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND id = $4
	`, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentSandboxID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	return digest
}

func deploymentSandboxImageDigest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) string {
	t.Helper()
	var digest string
	if err := pool.QueryRow(ctx, `
		SELECT image_digest
		  FROM deployment_sandboxes
		 WHERE org_id = $1
		   AND project_id = $2
		   AND environment_id = $3
		   AND id = $4
	`, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentSandboxID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	return digest
}

func testDigest(label string) string {
	sum := sha256.Sum256([]byte(label + ":" + uuid.NewString()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func seedWaitpointTokenIntegration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) waitpointTokenIntegrationIDs {
	t.Helper()
	ids := waitpointTokenIntegrationIDs{
		orgID:               dbtest.DefaultOrgID,
		projectID:           uuid.Must(uuid.NewV7()),
		environmentID:       uuid.Must(uuid.NewV7()),
		deploymentID:        uuid.Must(uuid.NewV7()),
		deploymentSandboxID: uuid.Must(uuid.NewV7()),
		workspaceID:         uuid.Must(uuid.NewV7()),
		taskID:              uuid.Must(uuid.NewV7()),
		runID:               uuid.Must(uuid.NewV7()),
	}
	artifactID := uuid.Must(uuid.NewV7())
	digest := "sha256:" + strings.ReplaceAll(uuid.NewString(), "-", "")
	rootfsDigest := "sha256:rootfs"
	projectSlug := "project-" + shortUUID(ids.projectID)
	environmentSlug := "env-" + shortUUID(ids.environmentID)
	var workerGroupID uuid.UUID
	if _, err := pool.Exec(ctx, `
		INSERT INTO organizations (id, name, slug) VALUES ($1, 'Default', 'default')
		ON CONFLICT (id) DO NOTHING
	`, ids.orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO projects (id, org_id, slug, name) VALUES ($1, $2, $3, 'Project')
	`, ids.projectID, ids.orgID, projectSlug); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Env', '#3366ff')
	`, ids.environmentID, ids.orgID, ids.projectID, environmentSlug); err != nil {
		t.Fatal(err)
	}
	imageArtifactID, imageDigest := seedSandboxImageArtifact(t, ctx, pool, ids)
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO cas_objects (digest, size_bytes, media_type)
		VALUES ($1, 1, 'application/json')
	`, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $2, $3, $4, $5, 'task_bundle', 1, 'application/json')
	`, artifactID, ids.orgID, ids.projectID, ids.environmentID, digest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployments (id, org_id, project_id, environment_id, worker_group_id, version, content_hash, deployment_source_artifact_id, status)
		VALUES ($1, $2, $3, $4, $5, 'v1', $6, $7, 'deployed')
	`, ids.deploymentID, ids.orgID, ids.projectID, ids.environmentID, workerGroupID, digest, artifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_sandboxes (
			id, org_id, project_id, environment_id, deployment_id, sandbox_id,
			image_artifact_id, image_artifact_format, rootfs_digest, image_digest, image_format,
			workspace_mount_path, runtime_abi, guestd_abi, adapter_abi, filesystem_format,
			contract_version, fingerprint
		)
		VALUES ($1, $2, $3, $4, $5, 'default', $6, 'oci-tar', $7, $8, 'oci-tar', '/workspace',
			'test', 'guestd-test', 'adapter-test', 'tar', 1, 'sandbox-fingerprint')
	`, ids.deploymentSandboxID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, imageArtifactID, rootfsDigest, imageDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (org_id, project_id, environment_id, task_id)
		VALUES ($1, $2, $3, 'approval-task')
		ON CONFLICT DO NOTHING
	`, ids.orgID, ids.projectID, ids.environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO deployment_tasks (
			id, org_id, project_id, environment_id, deployment_id, deployment_sandbox_id, task_id, bundle_artifact_id,
			queue_name, max_duration_seconds
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'approval-task', $7, 'default', 300)
	`, ids.taskID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.deploymentSandboxID, artifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspaces (
			id, org_id, project_id, environment_id, deployment_sandbox_id, sandbox_id, sandbox_fingerprint
		)
		VALUES ($1, $2, $3, $4, $5, 'default', 'sandbox-fingerprint')
	`, ids.workspaceID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentSandboxID); err != nil {
		t.Fatal(err)
	}
	initialWorkspaceArtifactID := seedWorkspaceVersionArtifact(t, ctx, pool, ids)
	initialVersionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO workspace_versions (
			id, org_id, project_id, environment_id, workspace_id, kind, state,
			artifact_id, artifact_encoding, artifact_entry_count, content_digest, size_bytes, promoted_at
		)
		SELECT $1, $2, $3, $4, $5, 'system', 'ready',
		       artifacts.id, $6, 0, artifacts.digest, artifacts.size_bytes, now()
		  FROM artifacts
		 WHERE artifacts.org_id = $2
		   AND artifacts.project_id = $3
		   AND artifacts.environment_id = $4
		   AND artifacts.id = $7
	`, initialVersionID, ids.orgID, ids.projectID, ids.environmentID, ids.workspaceID, workspace.ArtifactEncoding, initialWorkspaceArtifactID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE workspaces
		   SET current_version_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, initialVersionID, ids.orgID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	taskSessionID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_sessions (
			id, org_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id
		)
		VALUES ($1, $2, $3, $4, 'approval-task', $5, $5, $6)
	`, taskSessionID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			task_session_id, status, execution_status, payload, queue_name, max_duration_seconds, trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approval-task', $8, 'waiting', 'waiting', '{}', 'default', 300,
			'11111111111111111111111111111111', '2222222222222222')
	`, ids.runID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE task_sessions
		   SET current_run_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, ids.runID, ids.orgID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	return ids
}

func seedTaskSessionForRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) uuid.UUID {
	t.Helper()
	var taskSessionID uuid.UUID
	err := pool.QueryRow(ctx, `
		SELECT task_session_id
		  FROM runs
		 WHERE org_id = $1
		   AND id = $2
	`, ids.orgID, ids.runID).Scan(&taskSessionID)
	if err == nil {
		return taskSessionID
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	taskSessionID = uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (org_id, project_id, environment_id, task_id)
		VALUES ($1, $2, $3, 'approval-task')
		ON CONFLICT DO NOTHING
	`, ids.orgID, ids.projectID, ids.environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_sessions (
			id, org_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id, current_run_id
		)
		VALUES ($1, $2, $3, $4, 'approval-task', $5, $5, $6, $7)
	`, taskSessionID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.workspaceID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET task_session_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, taskSessionID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	return taskSessionID
}

func seedQueuedRunForWorkspaceMaterializationTest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) uuid.UUID {
	t.Helper()
	runID := uuid.Must(uuid.NewV7())
	taskSessionID := uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_sessions (
			id, org_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id
		)
		VALUES ($1, $2, $3, $4, 'approval-task', $5, $5, $6)
	`, taskSessionID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runs (
			id, org_id, project_id, environment_id, deployment_id, deployment_task_id, workspace_id, task_id,
			task_session_id, status, execution_status, payload, queue_name, max_duration_seconds,
			trace_id, root_span_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approval-task', $8, 'queued', 'queued', '{}', 'default', 300,
			'33333333333333333333333333333333', '4444444444444444')
	`, runID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.taskID, ids.workspaceID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, 'queued')
	`, attemptID, ids.orgID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET current_attempt_id = $1,
		       current_attempt_number = 1
		 WHERE org_id = $2
		   AND id = $3
	`, attemptID, ids.orgID, runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE task_sessions
		   SET current_run_id = $1
		 WHERE org_id = $2
		   AND id = $3
	`, runID, ids.orgID, taskSessionID); err != nil {
		t.Fatal(err)
	}
	return runID
}

func seedRunningTaskSessionLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) (taskSessionID uuid.UUID, runLeaseID uuid.UUID, workerID uuid.UUID) {
	t.Helper()
	taskSessionID = uuid.Must(uuid.NewV7())
	runLeaseID = uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	workerID = uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	workerResourceID := "worker-" + shortUUID(workerID)
	dispatchMessageID := "dispatch-" + runLeaseID.String()[:8]
	dispatchLeaseID := "lease-" + runLeaseID.String()[:8]
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO tasks (org_id, project_id, environment_id, task_id)
		VALUES ($1, $2, $3, 'approval-task')
		ON CONFLICT DO NOTHING
	`, ids.orgID, ids.projectID, ids.environmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_sessions (
			id, org_id, project_id, environment_id, task_id,
			initial_deployment_id, active_deployment_id, workspace_id, current_run_id
		)
		VALUES ($1, $2, $3, $4, 'approval-task', $5, $5, $6, $7)
	`, taskSessionID, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, ids.workspaceID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, resource_id, total_milli_cpu, total_memory_mib, total_disk_mib,
			worker_group_id,
			total_execution_slots, available_milli_cpu, available_memory_mib, available_disk_mib,
			available_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, 1000, 1024, 4096, $3, 1, 1000, 1024, 4096, 1,
			$4, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, workerResourceID, workerGroupID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, 'running')
	`, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, run_id, attempt_id, worker_instance_id, worker_group_id, dispatch_message_id,
			dispatch_lease_id, dispatch_attempt, status, lease_expires_at, runtime_id, trace_id,
			span_id, parent_span_id, traceparent
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			1, 'running', now() + interval '1 hour', $9,
			'11111111111111111111111111111111', '3333333333333333', '2222222222222222',
			'00-11111111111111111111111111111111-3333333333333333-01')
	`, runLeaseID, ids.orgID, ids.runID, attemptID, workerID, workerGroupID, dispatchMessageID, dispatchLeaseID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET task_session_id = $1,
		       workspace_id = $6,
		       current_run_lease_id = $2,
		       current_attempt_id = $3,
		       current_attempt_number = 1,
		       status = 'running',
		       execution_status = 'executing'
		 WHERE org_id = $4
		   AND id = $5
	`, taskSessionID, runLeaseID, attemptID, ids.orgID, ids.runID, ids.workspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, 1000, 1024, 4096, 1, $3, 'arm64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:rootfs', 'default', $4)
	`, ids.runID, ids.orgID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (
			run_id, org_id, status, queue_name, dispatch_message_id,
			reserved_by_worker_instance_id, reservation_expires_at
		)
		VALUES ($1, $2, 'reserved', 'default', $3, $4, now() + interval '1 hour')
	`, ids.runID, ids.orgID, dispatchMessageID, workerID); err != nil {
		t.Fatal(err)
	}
	return taskSessionID, runLeaseID, workerID
}

func seedRunningRunLease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) (runLeaseID uuid.UUID, workerID uuid.UUID) {
	t.Helper()
	runLeaseID = uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	workerID = uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	workerResourceID := "worker-" + shortUUID(workerID)
	dispatchMessageID := "dispatch-" + runLeaseID.String()[:8]
	dispatchLeaseID := "lease-" + runLeaseID.String()[:8]
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, resource_id, total_milli_cpu, total_memory_mib, total_disk_mib,
			worker_group_id,
			total_execution_slots, available_milli_cpu, available_memory_mib, available_disk_mib,
			available_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, 1000, 1024, 4096, $3, 1, 1000, 1024, 4096, 1,
			$4, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, workerResourceID, workerGroupID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, 'running')
	`, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, run_id, attempt_id, worker_instance_id, worker_group_id, dispatch_message_id,
			dispatch_lease_id, dispatch_attempt, status, lease_expires_at, runtime_id, trace_id,
			span_id, parent_span_id, traceparent
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8,
			1, 'running', now() + interval '1 hour', $9,
			'11111111111111111111111111111111', '3333333333333333', '2222222222222222',
			'00-11111111111111111111111111111111-3333333333333333-01')
	`, runLeaseID, ids.orgID, ids.runID, attemptID, workerID, workerGroupID, dispatchMessageID, dispatchLeaseID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET current_run_lease_id = $1,
		       current_attempt_id = $2,
		       current_attempt_number = 1,
		       status = 'running',
		       execution_status = 'executing'
		 WHERE org_id = $3
		   AND id = $4
	`, runLeaseID, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, 1000, 1024, 4096, 1, $3, 'arm64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:rootfs', 'default', $4)
	`, ids.runID, ids.orgID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	return runLeaseID, workerID
}

func seedWaitingRunSuspensionForWaitpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs) uuid.UUID {
	t.Helper()
	waitpointID := uuid.Must(uuid.NewV7())
	runSuspensionID := uuid.Must(uuid.NewV7())
	checkpointID := uuid.Must(uuid.NewV7())
	attemptID := uuid.Must(uuid.NewV7())
	sessionID := uuid.Must(uuid.NewV7())
	workerID := uuid.Must(uuid.NewV7())
	runtimeID := "runtime-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	workerResourceID := "worker-" + shortUUID(workerID)
	var workerGroupID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM worker_groups WHERE name = 'default'`).Scan(&workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime_releases (runtime_id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile)
		VALUES ($1, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (
			id, resource_id, total_milli_cpu, total_memory_mib, total_disk_mib,
			worker_group_id,
			total_execution_slots, available_milli_cpu, available_memory_mib, available_disk_mib,
			available_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile
		)
		VALUES ($1, $2, 1000, 1024, 4096, $3, 1, 1000, 1024, 4096, 1,
			$4, 'arm64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`, workerID, workerResourceID, workerGroupID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_attempts (id, org_id, run_id, attempt_number, status)
		VALUES ($1, $2, $3, 1, 'waiting')
	`, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_leases (
			id, org_id, run_id, attempt_id, worker_instance_id, worker_group_id, dispatch_message_id,
			dispatch_lease_id, dispatch_attempt, status, lease_expires_at, runtime_id, trace_id,
			span_id, parent_span_id, traceparent
		)
		VALUES ($1, $2, $3, $4, $5, $6, 'dispatch-1', 'lease-1', 1, 'released', now() + interval '1 hour',
			$7, '11111111111111111111111111111111', '3333333333333333', '2222222222222222',
			'00-11111111111111111111111111111111-3333333333333333-01')
	`, sessionID, ids.orgID, ids.runID, attemptID, workerID, workerGroupID, runtimeID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE runs
		   SET current_attempt_id = $1,
		       current_attempt_number = 1,
		       status = 'waiting',
		       execution_status = 'waiting'
		 WHERE org_id = $2
		   AND id = $3
	`, attemptID, ids.orgID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_runtime_requirements (
			run_id, org_id, requested_milli_cpu, requested_memory_mib, requested_disk_mib,
			requested_execution_slots, runtime_id, runtime_arch, runtime_abi, kernel_digest,
			initramfs_digest, rootfs_digest, cni_profile, worker_group_id
		)
		VALUES ($1, $2, 1000, 1024, 4096, 1, $3, 'arm64', 'test', 'sha256:kernel',
			'sha256:initramfs', 'sha256:rootfs', 'default', $4)
	`, ids.runID, ids.orgID, runtimeID, workerGroupID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_queue_items (run_id, org_id, status, queue_name)
		VALUES ($1, $2, 'parked', 'default')
	`, ids.runID, ids.orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO checkpoints (id, org_id, run_id, project_id, environment_id, run_lease_id, status, reason, manifest, ready_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'ready', 'waitpoint', '{}'::jsonb, now())
	`, checkpointID, ids.orgID, ids.runID, ids.projectID, ids.environmentID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO waitpoints (id, org_id, project_id, environment_id, run_id, kind, params)
		VALUES ($1, $2, $3, $4, $5, 'token', '{"token_id":"token-1"}')
	`, waitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspensions (
			id, org_id, run_id, project_id, environment_id, run_lease_id, checkpoint_id,
			correlation_id, status, waiting_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, '1', 'waiting', now())
	`, runSuspensionID, ids.orgID, ids.runID, ids.projectID, ids.environmentID, sessionID, checkpointID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspension_waitpoints (org_id, run_id, project_id, environment_id, run_suspension_id, waitpoint_id, ordinal)
		VALUES ($1, $2, $3, $4, $5, $6, 0)
	`, ids.orgID, ids.runID, ids.projectID, ids.environmentID, runSuspensionID, waitpointID); err != nil {
		t.Fatal(err)
	}
	return waitpointID
}

func seedChannelWaitpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, stream string, afterSequence int64) uuid.UUID {
	t.Helper()
	waitpointID := seedWaitingRunSuspensionForWaitpoint(t, ctx, pool, ids)
	var runSuspensionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT run_suspension_id
		  FROM run_suspension_waitpoints
		 WHERE org_id = $1
		   AND waitpoint_id = $2
	`, ids.orgID, waitpointID).Scan(&runSuspensionID); err != nil {
		t.Fatal(err)
	}
	params, err := json.Marshal(map[string]any{
		"channel":        stream,
		"after_sequence": afterSequence,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE waitpoints
		   SET kind = 'channel',
		       params = $1
		 WHERE org_id = $2
		   AND id = $3
	`, params, ids.orgID, waitpointID); err != nil {
		t.Fatal(err)
	}
	channelID := seedInputChannel(t, ctx, pool, ids, stream)
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_waits (
			waitpoint_id, org_id, project_id, environment_id, run_id, run_suspension_id, channel_id, after_sequence
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, waitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, runSuspensionID, channelID, afterSequence); err != nil {
		t.Fatal(err)
	}
	return waitpointID
}

func seedAdditionalChannelWaitpoint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, stream string, afterSequence int64) uuid.UUID {
	t.Helper()
	waitpointID := uuid.Must(uuid.NewV7())
	var runSuspensionID uuid.UUID
	var nextOrdinal int
	if err := pool.QueryRow(ctx, `
		SELECT run_suspensions.id,
		       COALESCE(max(run_suspension_waitpoints.ordinal), -1) + 1
		  FROM run_suspensions
		  JOIN run_suspension_waitpoints
		    ON run_suspension_waitpoints.org_id = run_suspensions.org_id
		   AND run_suspension_waitpoints.run_suspension_id = run_suspensions.id
		 WHERE run_suspensions.org_id = $1
		   AND run_suspensions.run_id = $2
		   AND run_suspensions.status = 'waiting'
		 GROUP BY run_suspensions.id
		 ORDER BY run_suspensions.waiting_at ASC, run_suspensions.id ASC
		 LIMIT 1
	`, ids.orgID, ids.runID).Scan(&runSuspensionID, &nextOrdinal); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO waitpoints (id, org_id, project_id, environment_id, run_id, kind, params)
		VALUES ($1, $2, $3, $4, $5, 'channel', $6)
	`, waitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, json.RawMessage(fmt.Sprintf(`{"channel":%q,"after_sequence":%d}`, stream, afterSequence))); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO run_suspension_waitpoints (org_id, run_id, project_id, environment_id, run_suspension_id, waitpoint_id, ordinal)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, ids.orgID, ids.runID, ids.projectID, ids.environmentID, runSuspensionID, waitpointID, nextOrdinal); err != nil {
		t.Fatal(err)
	}
	channelID := seedInputChannel(t, ctx, pool, ids, stream)
	if _, err := pool.Exec(ctx, `
		INSERT INTO channel_waits (
			waitpoint_id, org_id, project_id, environment_id, run_id, run_suspension_id, channel_id, after_sequence
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, waitpointID, ids.orgID, ids.projectID, ids.environmentID, ids.runID, runSuspensionID, channelID, afterSequence); err != nil {
		t.Fatal(err)
	}
	return waitpointID
}

func seedInputChannel(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, name string) uuid.UUID {
	t.Helper()
	taskSessionID := seedTaskSessionForRun(t, ctx, pool, ids)
	definitionID := seedChannelDefinition(t, ctx, pool, ids, name, "input")
	var channelID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO channels (org_id, project_id, environment_id, task_session_id, definition_id, name, direction)
		VALUES ($1, $2, $3, $4, $5, $6, 'input')
		ON CONFLICT (org_id, task_session_id, name, direction)
		DO UPDATE SET next_sequence = channels.next_sequence
		RETURNING id
	`, ids.orgID, ids.projectID, ids.environmentID, taskSessionID, definitionID, name).Scan(&channelID); err != nil {
		t.Fatal(err)
	}
	return channelID
}

func seedChannelDefinition(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ids waitpointTokenIntegrationIDs, name string, direction string) uuid.UUID {
	t.Helper()
	var definitionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO channel_definitions (
			org_id, project_id, environment_id, deployment_id, task_id, name, direction
		)
		VALUES ($1, $2, $3, $4, 'approval-task', $5, $6)
		ON CONFLICT (org_id, deployment_id, task_id, name, direction)
		DO UPDATE SET name = EXCLUDED.name
		RETURNING id
	`, ids.orgID, ids.projectID, ids.environmentID, ids.deploymentID, name, direction).Scan(&definitionID); err != nil {
		t.Fatal(err)
	}
	return definitionID
}

func assertChannelWaitpointData(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waitpointID uuid.UUID, want string) {
	t.Helper()
	var status string
	var data []byte
	var deliveredRecordID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT waitpoints.status::text, waitpoints.data, channel_waits.matched_record_id
		  FROM waitpoints
		  JOIN channel_waits ON channel_waits.org_id = waitpoints.org_id
		                    AND channel_waits.waitpoint_id = waitpoints.id
		 WHERE waitpoints.id = $1
	`, waitpointID).Scan(&status, &data, &deliveredRecordID); err != nil {
		t.Fatal(err)
	}
	if status != "completed" {
		t.Fatalf("waitpoint status = %s", status)
	}
	if deliveredRecordID == uuid.Nil {
		t.Fatal("matched_record_id is empty")
	}
	if got := canonicalJSON(t, data); got != want {
		t.Fatalf("input waitpoint data = %s, want %s", got, want)
	}
}

func assertWaitpointStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, waitpointID uuid.UUID, want string) {
	t.Helper()
	var status string
	if err := pool.QueryRow(ctx, `
		SELECT status::text
		  FROM waitpoints
		 WHERE id = $1
	`, waitpointID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != want {
		t.Fatalf("waitpoint %s status = %s, want %s", waitpointID, status, want)
	}
}

func newIntegrationDB(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("HELMR_TEST_DATABASE_URL is not set")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	var serverVersion int
	if err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		admin.Close()
		t.Skipf("Postgres %d does not provide uuidv7(); skipping waitpoint token integration test", serverVersion)
	}
	name := "helmr_waitpoint_tokens_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	testDSN := databaseDSN(t, dsn, name)
	if err := schema.Up(ctx, testDSN); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func databaseDSN(t *testing.T, dsn string, database string) string {
	t.Helper()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func completionHash(t *testing.T, data []byte) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func canonicalJSON(t *testing.T, data []byte) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(canonical)
}
