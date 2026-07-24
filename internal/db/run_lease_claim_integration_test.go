package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	runLeaseTestRegion      = "us-east-1"
	runLeaseTestWorkerGroup = "run-workers"
	runLeaseTestProtocol    = "helmr.worker.v0"
)

type runLeaseClaimFixture struct {
	pool                  *pgxpool.Pool
	queries               *Queries
	orgID                 uuid.UUID
	projectID             uuid.UUID
	environmentID         uuid.UUID
	deploymentID          uuid.UUID
	taskDefinitionID      uuid.UUID
	workspaceDefinitionID uuid.UUID
	workerID              uuid.UUID
	runtimeIdentityID     string
}

type runLeaseWork struct {
	leaseID uuid.UUID
	runID   uuid.UUID
}

type nestedHandoffChain struct {
	outerRunID          uuid.UUID
	parentRunID         uuid.UUID
	outerWaitID         uuid.UUID
	outerCheckpoint     uuid.UUID
	outerResumeID       uuid.UUID
	enclosingWaitID     uuid.UUID
	enclosingCheckpoint uuid.UUID
	enclosingResumeID   uuid.UUID
	runtimeID           uuid.UUID
	mountID             uuid.UUID
	versionID           uuid.UUID
}

func TestRunLeaseDiscoveryAndClaimFoundation(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	assigned := fixture.addWork(t, ctx, "assigned", time.Now().Add(-2*time.Minute))
	starting := fixture.addWork(t, ctx, "starting", time.Now().Add(-time.Minute))

	rows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerProtocolVersion: runLeaseTestProtocol,
		RowLimit: 8, WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || pgvalue.MustUUIDValue(rows[0].ID) != starting.leaseID ||
		pgvalue.MustUUIDValue(rows[1].ID) != assigned.leaseID {
		t.Fatalf("discovery = %+v, want starting then assigned", rows)
	}
	var state RunLeaseState
	var claimedAt pgtype.Timestamptz
	if err := fixture.pool.QueryRow(ctx,
		`SELECT state, claimed_at FROM run_leases WHERE id = $1`, assigned.leaseID,
	).Scan(&state, &claimedAt); err != nil {
		t.Fatal(err)
	}
	if state != RunLeaseStateAssigned || claimedAt.Valid {
		t.Fatalf("discovery mutated assigned lease to state=%s claimed_at=%v", state, claimedAt)
	}

	secretLocators, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(secretLocators.RunID) != assigned.runID ||
		secretLocators.EnvironmentID != pgvalue.UUID(fixture.environmentID) ||
		secretLocators.AttemptNumber != 1 {
		t.Fatalf("Secret delivery locators = %+v", secretLocators)
	}

	locators, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(locators.RunID) != assigned.runID {
		t.Fatalf("locator run = %s, want %s", pgvalue.UUIDString(locators.RunID), assigned.runID)
	}
	if locators.RunWaitID.Valid ||
		locators.SuspendCheckpointID.Valid ||
		locators.CheckpointPrivateWorkspaceVersionID.Valid {
		t.Fatalf("fresh locator exposed restore authority: %+v", locators)
	}
	source, err := fixture.queries.GetRunCheckpointSource(ctx, GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: locators.WorkspaceLeaseID,
		SourceRunLeaseID:       pgvalue.UUID(assigned.leaseID),
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if source.RunLease.ID != pgvalue.UUID(assigned.leaseID) ||
		source.WorkspaceLease.ID != locators.WorkspaceLeaseID ||
		source.WorkspaceLease.OwnerRunLeaseID != source.RunLease.ID ||
		source.RuntimeInstance.ID != locators.RuntimeInstanceID {
		t.Fatal("checkpoint source did not return one Run/Workspace Lease and Runtime receipt")
	}
	if _, err := fixture.queries.GetRunCheckpointSource(ctx, GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		SourceRunLeaseID:       pgvalue.UUID(assigned.leaseID),
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched source Workspace Lease error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockRunLeaseClaimWait(ctx, LockRunLeaseClaimWaitParams{
		ID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:     locators.EnvironmentID,
		RunID:             locators.RunID,
		AttemptNumber:     locators.AttemptNumber,
		WorkspaceID:       locators.WorkspaceID,
		CurrentRunLeaseID: pgvalue.UUID(assigned.leaseID),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing restore Wait error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockSameWorkspaceHandoffWait(ctx, LockSameWorkspaceHandoffWaitParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID:       locators.EnvironmentID,
		ParentRunID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ParentAttemptNumber: 1,
		WorkspaceID:         locators.WorkspaceID,
		ChildRunID:          locators.RunID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing handoff Wait error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockRestorableRunCheckpoint(ctx, LockRestorableRunCheckpointParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RunID:         locators.RunID,
		AttemptNumber: locators.AttemptNumber,
		RunWaitID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkspaceID:   locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing restore Checkpoint error = %v, want no rows", err)
	}
	if _, err := fixture.queries.LockReadyRunCheckpoint(ctx, LockReadyRunCheckpointParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Kind:          RunCheckpointKindHandoffResume,
		RunID:         locators.RunID,
		AttemptNumber: locators.AttemptNumber,
		RunWaitID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkspaceID:   locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing handoff Checkpoint error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunCheckpointSource(ctx, GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		SourceRunLeaseID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("missing source Runtime error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 2,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale sequence locator error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-worker locator error = %v, want no rows", err)
	}

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locked := New(tx)
	run, err := locked.LockRunLeaseClaimRun(ctx, LockRunLeaseClaimRunParams{
		ID: locators.RunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := locked.LockRunLeaseClaimWorkspace(ctx, LockRunLeaseClaimWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimAttempt(ctx, LockRunLeaseClaimAttemptParams{
		RunID: locators.RunID, Number: locators.AttemptNumber, WorkspaceID: locators.WorkspaceID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimWorkerGroup(ctx, LockRunLeaseClaimWorkerGroupParams{
		ID: runLeaseTestWorkerGroup, RegionID: locators.RegionID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimWorker(ctx, LockRunLeaseClaimWorkerParams{
		ID: pgvalue.UUID(fixture.workerID), WorkerGroupID: runLeaseTestWorkerGroup,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimNetworkSlot(ctx, LockRunLeaseClaimNetworkSlotParams{
		ID: locators.NetworkSlotID, WorkerGroupID: runLeaseTestWorkerGroup,
		WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
		Generation: locators.NetworkSlotGeneration, RuntimeInstanceID: locators.RuntimeInstanceID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimRuntime(ctx, LockRunLeaseClaimRuntimeParams{
		ID: locators.RuntimeInstanceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkspaceID: locators.WorkspaceID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := locked.LockRunLeaseClaimLease(ctx, LockRunLeaseClaimLeaseParams{
		ID: pgvalue.UUID(assigned.leaseID), RunID: locators.RunID,
		WorkspaceID: locators.WorkspaceID, AttemptNumber: locators.AttemptNumber,
		LeaseSequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	mount, err := locked.LockRunLeaseClaimMount(ctx, LockRunLeaseClaimMountParams{
		ID: locators.WorkspaceMountID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, RuntimeInstanceID: locators.RuntimeInstanceID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspaceLease, err := locked.LockRunLeaseClaimWorkspaceLease(ctx, LockRunLeaseClaimWorkspaceLeaseParams{
		ID: locators.WorkspaceLeaseID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, RuntimeInstanceID: locators.RuntimeInstanceID, WorkspaceID: locators.WorkspaceID,
		WorkspaceMountID: locators.WorkspaceMountID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.CurrentRunLeaseID != pgvalue.UUID(assigned.leaseID) ||
		workspaceLease.OwnerRunLeaseID != pgvalue.UUID(assigned.leaseID) ||
		workspaceLease.MountFencingGeneration != mount.FencingGeneration ||
		workspaceLease.OwnershipGeneration != workspace.OwnershipGeneration ||
		workspaceLease.WriterGeneration != workspace.WriterGeneration {
		t.Fatal("locked claim authority is not one exact attachment")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	claimed, err := fixture.queries.MarkRunLeaseStarting(ctx, MarkRunLeaseStartingParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.State != RunLeaseStateStarting || !claimed.ClaimedAt.Valid {
		t.Fatalf("claimed lease = state:%s claimed_at:%v", claimed.State, claimed.ClaimedAt)
	}
	firstClaimedAt := claimed.ClaimedAt.Time
	if _, err := fixture.queries.MarkRunLeaseStarting(ctx, MarkRunLeaseStartingParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim update error = %v, want no rows", err)
	}
	replayed, err := fixture.queries.GetRunLease(ctx, GetRunLeaseParams{
		RunID: pgvalue.UUID(assigned.runID), AttemptNumber: 1,
		WorkspaceID: locators.WorkspaceID, ID: pgvalue.UUID(assigned.leaseID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.ClaimedAt.Valid || !replayed.ClaimedAt.Time.Equal(firstClaimedAt) {
		t.Fatalf("claim replay timestamp = %v, want %s", replayed.ClaimedAt, firstClaimedAt)
	}
	unclaimed := fixture.addWork(t, ctx, "assigned", time.Now())

	if _, err := fixture.pool.Exec(ctx,
		`UPDATE worker_instances SET state = 'draining', draining_at = now() WHERE id = $1`,
		fixture.workerID,
	); err != nil {
		t.Fatal(err)
	}
	drainingRows, err := fixture.queries.DiscoverWorkerRunLeaseWork(ctx, DiscoverWorkerRunLeaseWorkParams{
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerProtocolVersion: runLeaseTestProtocol,
		RowLimit: 8, WorkerInstanceID: pgvalue.UUID(fixture.workerID), WorkerEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(drainingRows) != 2 {
		t.Fatalf("draining discovery returned %d rows, want two replayable starting leases", len(drainingRows))
	}
	for _, row := range drainingRows {
		if pgvalue.MustUUIDValue(row.ID) == unclaimed.leaseID {
			t.Fatalf("draining discovery returned unclaimed assigned lease %s", unclaimed.leaseID)
		}
		if pgvalue.MustUUIDValue(row.ID) != assigned.leaseID &&
			pgvalue.MustUUIDValue(row.ID) != starting.leaseID {
			t.Fatalf("draining discovery returned unrelated lease %s", pgvalue.UUIDString(row.ID))
		}
	}
	if _, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(unclaimed.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("draining assigned Secret locator error = %v, want no rows", err)
	}
	if _, err := fixture.queries.GetRunLeaseSecretDeliveryLocators(ctx, GetRunLeaseSecretDeliveryLocatorsParams{
		ID: pgvalue.UUID(assigned.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	}); err != nil {
		t.Fatalf("draining replay Secret locator: %v", err)
	}
}

func TestRunLeaseClaimLocatesNestedHandoffAuthority(t *testing.T) {
	ctx := context.Background()
	fixture := newRunLeaseClaimFixture(t, ctx)
	work := fixture.addWork(t, ctx, "assigned", time.Now().Add(-time.Minute))
	chain := fixture.addNestedHandoffChain(t, ctx, work)
	locatorArgs := []any{
		pgvalue.UUID(work.leaseID),
		int64(1),
		runLeaseTestWorkerGroup,
		pgvalue.UUID(fixture.workerID),
		int64(1),
		runLeaseTestProtocol,
	}
	var locatorCount int
	if err := fixture.pool.QueryRow(
		ctx,
		"SELECT count(*) FROM ("+getRunLeaseClaimLocators+") AS claim_locators",
		locatorArgs...,
	).Scan(&locatorCount); err != nil {
		t.Fatal(err)
	}
	if locatorCount != 1 {
		t.Fatalf("nested claim locator rows = %d, want exactly one", locatorCount)
	}

	locators, err := fixture.queries.GetRunLeaseClaimLocators(ctx, GetRunLeaseClaimLocatorsParams{
		ID: pgvalue.UUID(work.leaseID), LeaseSequence: 1,
		WorkerGroupID: runLeaseTestWorkerGroup, WorkerInstanceID: pgvalue.UUID(fixture.workerID),
		WorkerEpoch: 1, WorkerProtocolVersion: runLeaseTestProtocol,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pgvalue.MustUUIDValue(locators.ParentRunID) != chain.parentRunID ||
		!locators.ParentOwnsLifecycle.Valid ||
		!locators.ParentOwnsLifecycle.Bool ||
		locators.ParentAttemptNumber != 1 {
		t.Fatalf("parent locator = %+v, want parent %s attempt 1", locators, chain.parentRunID)
	}
	if pgvalue.MustUUIDValue(locators.EnclosingWaitID) != chain.enclosingWaitID ||
		pgvalue.MustUUIDValue(locators.EnclosingSuspendCheckpointID) != chain.enclosingCheckpoint ||
		pgvalue.MustUUIDValue(locators.EnclosingResumeAttachID) != chain.enclosingResumeID ||
		pgvalue.MustUUIDValue(locators.EnclosingRuntimeInstanceID) != chain.runtimeID ||
		pgvalue.MustUUIDValue(locators.EnclosingWorkspaceMountID) != chain.mountID ||
		pgvalue.MustUUIDValue(locators.EnclosingBaseWorkspaceVersionID) != chain.versionID ||
		locators.EnclosingMountGeneration.Int64 != 2 ||
		locators.EnclosingOwnershipGeneration.Int64 != 1 ||
		locators.EnclosingParentWriterGeneration.Int64 != 2 ||
		locators.EnclosingChildWriterGeneration.Int64 != 3 ||
		locators.EnclosingResumeWriterGeneration.Valid {
		t.Fatalf("enclosing locator = %+v, want B→C writer receipt 2→3", locators)
	}
	if pgvalue.MustUUIDValue(locators.ParentEnclosingWaitID) != chain.outerWaitID ||
		pgvalue.MustUUIDValue(locators.ParentEnclosingRunID) != chain.outerRunID ||
		locators.ParentEnclosingAttemptNumber != 1 {
		t.Fatalf("parent enclosing locator = %+v, want A→B Wait %s", locators, chain.outerWaitID)
	}
}

func newRunLeaseClaimFixture(t *testing.T, ctx context.Context) runLeaseClaimFixture {
	t.Helper()
	pool := newRunLeaseClaimDatabase(t, ctx)
	fixture := runLeaseClaimFixture{
		pool: pool, queries: New(pool),
		orgID: uuid.Must(uuid.NewV7()), projectID: uuid.Must(uuid.NewV7()),
		environmentID: uuid.Must(uuid.NewV7()), deploymentID: uuid.Must(uuid.NewV7()),
		taskDefinitionID: uuid.Must(uuid.NewV7()), workspaceDefinitionID: uuid.Must(uuid.NewV7()),
		workerID: uuid.Must(uuid.NewV7()), runtimeIdentityID: "run-lease-test-runtime",
	}
	sourceID, programID, imageID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7())
	sourceDigest, programDigest := runLeaseTestDigest("source"), runLeaseTestDigest("program")
	imageDigest := runLeaseTestDigest("image")
	programReceipt := dbtest.ProgramReceipt(dbtest.ProgramReceiptAuthority{
		Architecture:            "x86_64",
		ProgramArtifactID:       programID,
		ProgramDigest:           programDigest,
		ProgramSizeBytes:        1,
		RuntimeDigest:           "sha256:" + strings.Repeat("01", 32),
		SourceArtifactID:        sourceID,
		SourceDigest:            sourceDigest,
		SourceSizeBytes:         1,
		StandardToolchainDigest: "sha256:" + strings.Repeat("02", 32),
	})
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ($1, 'aws', $1, 'Run Lease Test')
	`, runLeaseTestRegion)
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO lookup_hmac_versions (version, key_fingerprint, is_current)
		VALUES (1, $1, true)
	`, runLeaseTestHash("lookup-hmac"))
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO worker_groups (
			id, region_id, name, enrollment_policy_fingerprint,
			allowed_attestation_fingerprints, protocol_version
		) VALUES ($1, $2, $1, 'test-policy', ARRAY['test-attestation'], $3)
	`, runLeaseTestWorkerGroup, runLeaseTestRegion, runLeaseTestProtocol)
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ($1, $2, 'Run Lease Test', $3)
	`, fixture.orgID, runLeasePublicID(t, publicid.Organization), "run-lease-"+shortRunLeaseID(fixture.orgID))
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, $4, $5, 'Run Lease Test')
	`, fixture.projectID, runLeasePublicID(t, publicid.Project), fixture.orgID,
		runLeaseTestRegion, "run-lease-"+shortRunLeaseID(fixture.projectID))
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, $5, 'Run Lease Test', '#3366ff')
	`, fixture.environmentID, runLeasePublicID(t, publicid.Environment), fixture.orgID,
		fixture.projectID, "run-lease-"+shortRunLeaseID(fixture.environmentID))
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES
			($1, $2, 1, 'application/vnd.helmr.deployment-source.v0+tar'),
			($1, $3, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($1, $4, 1, 'application/octet-stream')
	`, fixture.orgID, sourceDigest, programDigest, imageDigest)
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO artifacts (
			id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
		) VALUES
			($1, $4, $5, $6, $7, 'deployment_source', 1, 'application/vnd.helmr.deployment-source.v0+tar'),
			($2, $4, $5, $6, $8, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
			($3, $4, $5, $6, $9, 'workspace_image', 1, 'application/octet-stream')
	`, sourceID, programID, imageID, fixture.orgID, fixture.projectID,
		fixture.environmentID, sourceDigest, programDigest, imageDigest)
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO deployments (
			id, public_id, org_id, project_id, environment_id, build_region_id,
			build_architecture, build_runtime_digest, build_standard_toolchain_digest,
			build_manager_name, build_manager_version, build_manager_digest,
			build_contract_version, version, content_hash, deployment_source_artifact_id,
			program_artifact_id, program_runtime_digest, program_architecture,
			program_receipt, queue_config, status
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'x86_64',
			decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
			'bun', '1.2.3', decode(repeat('22', 32), 'hex'),
			'helmr.program-build.v0', 'run-lease-test', $7, $8, $9,
			decode(repeat('01', 32), 'hex'), 'x86_64', $10::jsonb, '{}'::jsonb, 'deployed'
		)
	`, fixture.deploymentID, runLeasePublicID(t, publicid.Deployment), fixture.orgID,
		fixture.projectID, fixture.environmentID, runLeaseTestRegion, sourceDigest,
		sourceID, programID, programReceipt)
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id,
			manifest_version, manifest, manifest_digest,
			workspace_architecture, artifact_id
		) VALUES (
			$1, $3, $4, 'task', 'test-task', 0, '{}'::jsonb,
			decode(repeat('03', 32), 'hex'), NULL, NULL
		), (
			$2, $3, $4, 'workspace', 'test-workspace', 0, '{}'::jsonb,
			decode(repeat('04', 32), 'hex'), 'x86_64', $5
		)
	`, fixture.taskDefinitionID, fixture.workspaceDefinitionID,
		fixture.environmentID, fixture.deploymentID, imageID)
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest,
			rootfs_digest, cni_profile
		) VALUES ($1, 'x86_64', 'test', 'kernel', 'initramfs', 'rootfs', 'default')
	`, fixture.runtimeIdentityID)
	mustRunLeaseExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, protocol_version, supervisor_version,
			supports_run, runtime_identity_id,
			substrate_format, substrate_builder_abi, substrate_layout_abi,
			certified_cpu_millis, certified_memory_bytes, certified_workload_disk_bytes,
			certified_scratch_bytes, per_vm_cpu_millis, per_vm_memory_bytes,
			per_vm_workload_disk_bytes, per_vm_scratch_bytes,
			max_vm_slots, max_run_consumers, max_runtime_starts,
			certification_profile, certification_fingerprint,
			epoch_started_at, certified_at, activated_at
		) VALUES (
			$1, $2, $3, 'test-attestation', 'active', 1, $4, $5, 'test',
			true, $6, 'squashfs', 'builder-v0', 'layout-v0',
			8000, 8589934592, 17179869184, 17179869184,
			1000, 1073741824, 2147483648, 2147483648,
			8, 8, 8, 'test', 'test-cert', now(), now(), now()
		)
	`, fixture.workerID, fixture.workerID.String(), runLeaseTestWorkerGroup,
		uuid.Must(uuid.NewV7()), runLeaseTestProtocol, fixture.runtimeIdentityID)
	return fixture
}

func (fixture runLeaseClaimFixture) addNestedHandoffChain(
	t *testing.T,
	ctx context.Context,
	work runLeaseWork,
) nestedHandoffChain {
	t.Helper()
	chain := nestedHandoffChain{
		outerRunID:          uuid.Must(uuid.NewV7()),
		parentRunID:         uuid.Must(uuid.NewV7()),
		outerWaitID:         uuid.Must(uuid.NewV7()),
		outerCheckpoint:     uuid.Must(uuid.NewV7()),
		outerResumeID:       uuid.Must(uuid.NewV7()),
		enclosingWaitID:     uuid.Must(uuid.NewV7()),
		enclosingCheckpoint: uuid.Must(uuid.NewV7()),
		enclosingResumeID:   uuid.Must(uuid.NewV7()),
	}
	outerClaimID, enclosingClaimID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	outerLeaseID, parentLeaseID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	outerWorkspaceLeaseID, parentWorkspaceLeaseID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	historicalWaitID := uuid.Must(uuid.NewV7())

	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT run_leases.runtime_instance_id,
		       workspace_leases.workspace_mount_id,
		       workspace_leases.base_version_id
		  FROM run_leases
		  JOIN workspace_leases
		    ON workspace_leases.owner_run_lease_id = run_leases.id
		 WHERE run_leases.id = $1
	`, work.leaseID).Scan(&chain.runtimeID, &chain.mountID, &chain.versionID); err != nil {
		t.Fatal(err)
	}
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'released',
		       writer_generation = 3,
		       released_at = now(),
		       terminal_at = now()
		 WHERE owner_run_lease_id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'checkpointed',
		       claimed_at = assigned_at,
		       started_at = assigned_at,
		       checkpointed_at = now(),
		       terminal_at = now(),
		       terminal_reason_code = 'test_handoff'
		 WHERE id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO idempotency_claims (
			id, environment_id, operation, scope_hash, key_hash,
			hash_key_version, generation, request_fingerprint, accepted_at
		) VALUES
			($1, $3, 'task.child.invoke', $4, $5, 1, 1, $6, now()),
			($2, $3, 'task.child.invoke', $7, $8, 1, 1, $9, now())
	`, outerClaimID, enclosingClaimID, fixture.environmentID,
		runLeaseTestHash("outer-scope"), runLeaseTestHash("outer-key"),
		runLeaseTestHash("outer-request"), runLeaseTestHash("inner-scope"),
		runLeaseTestHash("inner-key"), runLeaseTestHash("inner-request"))
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO runs (
			id, public_id, org_id, project_id, environment_id, deployment_id,
			deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
			cause_kind, parent_run_id, parent_owns_lifecycle, workspace_id,
			base_workspace_version_id, payload, queue_name, queue_origin_at,
			queue_score_at, max_active_duration_ms, retry_policy, trace_id,
			root_span_id, claim_id
		) VALUES (
			$1, $2, $5, $6, $7, $8, $9, 'task', 'test-task', 'api',
			NULL, NULL, $10, $11, '{}'::jsonb, 'default', now(), now(),
			300000, '{"enabled":false}'::jsonb,
			'33333333333333333333333333333333', '4444444444444444', NULL
		), (
			$3, $4, $5, $6, $7, $8, $9, 'task', 'test-task', 'child',
			$1, true, $10, $11, '{}'::jsonb, 'default', now(), now(),
			300000, '{"enabled":false}'::jsonb,
			'55555555555555555555555555555555', '6666666666666666', $12
		)
	`, chain.outerRunID, runLeasePublicID(t, publicid.Run),
		chain.parentRunID, runLeasePublicID(t, publicid.Run),
		fixture.orgID, fixture.projectID, fixture.environmentID,
		fixture.deploymentID, fixture.taskDefinitionID, fixture.workspaceID(t, ctx, tx, work.runID),
		chain.versionID, outerClaimID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE runs
		   SET cause_kind = 'child',
		       parent_run_id = $1,
		       parent_owns_lifecycle = true,
		       claim_id = $2
		 WHERE id = $3
	`, chain.parentRunID, enclosingClaimID, work.runID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_attempts (
			run_id, number, entrypoint_kind, workspace_id,
			entrypoint_entered_at, base_workspace_version_id
		) VALUES
			($1, 1, 'task', $3, now(), $4),
			($2, 1, 'task', $3, now(), $4)
	`, chain.outerRunID, chain.parentRunID, fixture.workspaceID(t, ctx, tx, work.runID), chain.versionID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE workspaces
		   SET owner_run_id = $1,
		       writer_generation = 3
		 WHERE id = (SELECT workspace_id FROM runs WHERE id = $2)
	`, chain.outerRunID, work.runID)

	fixture.parkNestedRun(t, ctx, tx, nestedRunPark{
		runID: chain.outerRunID, childRunID: chain.parentRunID,
		claimID: outerClaimID, leaseID: outerLeaseID,
		workspaceLeaseID: outerWorkspaceLeaseID, waitID: chain.outerWaitID,
		checkpointID: chain.outerCheckpoint, writerGeneration: 1,
		childWriterGeneration: 2, runtimeID: chain.runtimeID,
		mountID: chain.mountID, versionID: chain.versionID,
		resumeAttachID: chain.outerResumeID,
	})
	fixture.parkNestedRun(t, ctx, tx, nestedRunPark{
		runID: chain.parentRunID, childRunID: work.runID,
		claimID: enclosingClaimID, leaseID: parentLeaseID,
		workspaceLeaseID: parentWorkspaceLeaseID, waitID: chain.enclosingWaitID,
		checkpointID: chain.enclosingCheckpoint, writerGeneration: 2,
		childWriterGeneration: 3, runtimeID: chain.runtimeID,
		mountID: chain.mountID, versionID: chain.versionID,
		resumeAttachID: chain.enclosingResumeID,
	})
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, condition_state,
			child_run_id, child_parent_owned, child_target_declared_id,
			child_claim_id, child_request, suspension_state,
			expected_run_state_version, attempt_number, resume_attach_id,
			condition_error, condition_terminal_at, condition_reason_code,
			suspension_terminal_at, suspension_reason_code, suspension_error
		) VALUES (
			$1, $2, $3, $4, 'child', 'failed',
			$5, true, 'test-task', $6, '{}'::jsonb, 'failed',
			1, 1, $7, '{}'::jsonb, now(), 'test_history',
			now(), 'test_history', '{}'::jsonb
		)
	`, historicalWaitID, fixture.environmentID, chain.outerRunID,
		fixture.workspaceID(t, ctx, tx, work.runID), chain.parentRunID,
		outerClaimID, uuid.Must(uuid.NewV7()))
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'assigned',
		       claimed_at = NULL,
		       started_at = NULL,
		       checkpointed_at = NULL,
		       terminal_at = NULL,
		       terminal_reason_code = NULL
		 WHERE id = $1
	`, work.leaseID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'active',
		       released_at = NULL,
		       terminal_at = NULL
		 WHERE owner_run_lease_id = $1
	`, work.leaseID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return chain
}

type nestedRunPark struct {
	runID                 uuid.UUID
	childRunID            uuid.UUID
	claimID               uuid.UUID
	leaseID               uuid.UUID
	workspaceLeaseID      uuid.UUID
	waitID                uuid.UUID
	checkpointID          uuid.UUID
	writerGeneration      int64
	childWriterGeneration int64
	runtimeID             uuid.UUID
	mountID               uuid.UUID
	versionID             uuid.UUID
	resumeAttachID        uuid.UUID
}

func (fixture runLeaseClaimFixture) parkNestedRun(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	park nestedRunPark,
) {
	t.Helper()
	workspaceID := fixture.workspaceID(t, ctx, tx, park.runID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
			runtime_identity_id, worker_protocol_version, requested_cpu_millis,
			requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at,
			claimed_at, started_at, expires_at
		)
		SELECT $1, org_id, project_id, environment_id, $2, workspace_id, region_id,
		       1, 1, worker_group_id, worker_instance_id, worker_epoch,
		       runtime_instance_id, network_slot_id, network_slot_generation,
		       runtime_identity_id, worker_protocol_version, requested_cpu_millis,
		       requested_memory_bytes, requested_workload_disk_bytes,
		       requested_scratch_bytes, requested_execution_slots, 'running',
		       now() - interval '1 minute', now() + interval '5 minutes',
		       now() - interval '1 minute', now() - interval '1 minute',
		       now() + interval '10 minutes'
		  FROM run_leases
		 WHERE runtime_instance_id = $3
		 ORDER BY created_at
		 LIMIT 1
	`, park.leaseID, park.runID, park.runtimeID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
			workspace_mount_id, owner_run_lease_id, base_version_id,
			ownership_generation, writer_generation, mount_fencing_generation,
			fencing_key_fingerprint, fencing_token_hash, expires_at
		)
		SELECT $1, org_id, worker_group_id, project_id, environment_id, region_id,
		       worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
		       workspace_mount_id, $2, base_version_id, ownership_generation, $3,
		       mount_fencing_generation, fencing_key_fingerprint, fencing_token_hash,
		       now() + interval '10 minutes'
		  FROM workspace_leases
		 WHERE workspace_id = $4
		 ORDER BY acquired_at
		 LIMIT 1
	`, park.workspaceLeaseID, park.leaseID, park.writerGeneration, workspaceID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE runs
		   SET current_run_lease_id = $1,
		       status = 'running',
		       first_lease_at = now() - interval '1 minute',
		       started_at = now() - interval '1 minute'
		 WHERE id = $2
	`, park.leaseID, park.runID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_waits (
			id, environment_id, run_id, workspace_id, kind, condition_state,
			child_run_id, child_parent_owned, child_target_declared_id,
			child_claim_id, child_request, suspension_state,
			expected_run_state_version, attempt_number, current_run_lease_id,
			checkpoint_request_version, checkpoint_ack_version, resume_attach_id
		) VALUES (
			$1, $2, $3, $4, 'child', 'pending',
			$5, true, 'test-task', $6, '{}'::jsonb, 'hot',
			1, 1, $7, 1, 0, $8
		)
	`, park.waitID, fixture.environmentID, park.runID, workspaceID,
		park.childRunID, park.claimID, park.leaseID, park.resumeAttachID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE run_leases
		   SET state = 'checkpointed',
		       checkpointed_at = now(),
		       terminal_at = now(),
		       terminal_reason_code = 'test_handoff'
		 WHERE id = $1
	`, park.leaseID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE workspace_leases
		   SET state = 'released',
		       released_at = now(),
		       terminal_at = now()
		 WHERE id = $1
	`, park.workspaceLeaseID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_checkpoints (
			id, kind, run_id, attempt_number, run_wait_id,
			source_run_lease_id, source_workspace_lease_id, workspace_id,
			base_workspace_version_id, private_workspace_version_id,
			state, restore_manifest, ready_request_fingerprint, ready_at
		) VALUES (
			$1, 'suspend', $2, 1, $3, $4, $5, $6,
			$7, $7, 'ready', '{"test":true}'::jsonb, 'test-ready', now()
		)
	`, park.checkpointID, park.runID, park.waitID, park.leaseID,
		park.workspaceLeaseID, workspaceID, park.versionID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE run_waits
		   SET suspension_state = 'parked',
		       current_run_lease_id = NULL,
		       prior_run_lease_id = $1,
		       checkpoint_ack_version = 1,
		       suspend_checkpoint_id = $2,
		       base_workspace_version_id = $3,
		       base_workspace_content_digest = (
		           SELECT content_digest
		             FROM workspace_versions
		            WHERE id = $3
		       ),
		       handoff_runtime_instance_id = $4,
		       handoff_workspace_mount_id = $5,
		       handoff_mount_generation = 2,
		       ownership_generation = 1,
		       parent_writer_generation = $6,
		       child_writer_generation = $7
		 WHERE id = $8
	`, park.leaseID, park.checkpointID, park.versionID, park.runtimeID,
		park.mountID, park.writerGeneration, park.childWriterGeneration, park.waitID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE runs
		   SET status = 'waiting',
		       current_run_lease_id = NULL
		 WHERE id = $1
	`, park.runID)
}

func (fixture runLeaseClaimFixture) workspaceID(
	t *testing.T,
	ctx context.Context,
	tx pgx.Tx,
	runID uuid.UUID,
) uuid.UUID {
	t.Helper()
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT workspace_id FROM runs WHERE id = $1`, runID).Scan(&workspaceID); err != nil {
		t.Fatal(err)
	}
	return workspaceID
}

func (fixture runLeaseClaimFixture) addWork(
	t *testing.T,
	ctx context.Context,
	state string,
	assignedAt time.Time,
) runLeaseWork {
	t.Helper()
	workspaceID, versionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	runID, runtimeID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	slotID, mountID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	leaseID, workspaceLeaseID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO workspaces (
			id, public_id, org_id, project_id, environment_id, region_id,
			declaration_kind, workspace_declared_id, deployment_definition_id,
			owner_run_id, ownership_generation, writer_generation, head_version_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'workspace', 'test-workspace', $7,
			$8, 1, 1, $9
		)
	`, workspaceID, runLeasePublicID(t, publicid.Workspace), fixture.orgID,
		fixture.projectID, fixture.environmentID, runLeaseTestRegion,
		fixture.workspaceDefinitionID, runID, versionID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO workspace_versions (
			id, public_id, org_id, project_id, environment_id, workspace_id,
			kind, content_digest, state, ownership_generation, writer_generation, published_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, 'system',
			'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
			'committed', 0, 0, now()
		)
	`, versionID, runLeasePublicID(t, publicid.WorkspaceVersion), fixture.orgID,
		fixture.projectID, fixture.environmentID, workspaceID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO runs (
			id, public_id, org_id, project_id, environment_id, deployment_id,
			deployment_definition_id, entrypoint_kind, entrypoint_declared_id,
			cause_kind, workspace_id, base_workspace_version_id, payload,
			queue_name, queue_origin_at, queue_score_at, max_active_duration_ms,
			retry_policy, trace_id, root_span_id
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 'task', 'test-task', 'api',
			$8, $9, '{}'::jsonb, 'default', now(), now(), 300000,
			'{"enabled":false}'::jsonb,
			'11111111111111111111111111111111', '2222222222222222'
		)
	`, runID, runLeasePublicID(t, publicid.Run), fixture.orgID, fixture.projectID,
		fixture.environmentID, fixture.deploymentID, fixture.taskDefinitionID,
		workspaceID, versionID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_attempts (
			run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id
		) VALUES ($1, 1, 'task', $2, $3)
	`, runID, workspaceID, versionID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO runtime_instances (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, runtime_identity_id, deployment_definition_id,
			worker_epoch, network_policy, reserved_cpu_millis, reserved_memory_bytes,
			reserved_workload_disk_bytes, reserved_scratch_bytes, reserved_execution_slots,
			workspace_id, program_deployment_id, desired_reason, observed_state,
			observed_version, observed_desired_version, preparing_at, ready_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, 1, '{}'::jsonb,
			1000, 1073741824, 2147483648, 2147483648, 1,
			$10, $11, 'test', 'ready', 1, 1, now(), now()
		)
	`, runtimeID, fixture.orgID, runLeaseTestWorkerGroup, fixture.projectID,
		fixture.environmentID, runLeaseTestRegion, fixture.workerID,
		fixture.runtimeIdentityID, fixture.workspaceDefinitionID, workspaceID,
		fixture.deploymentID)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO worker_network_slots (
			id, worker_group_id, worker_instance_id, worker_epoch, slot_name,
			generation, state, runtime_instance_id, host_interface_name,
			guest_address, gateway_address, subnet, tap_name, netns_name,
			guest_mac, assigned_at
		) VALUES (
			$1, $2, $3, 1, $4, 1, 'bound', $5, $6,
			$9, '10.0.0.1', '10.0.0.0/8', $7, $8,
			'02:00:00:00:00:01', now()
		)
	`, slotID, runLeaseTestWorkerGroup, fixture.workerID, "slot-"+shortRunLeaseID(slotID),
		runtimeID, "veth-"+shortRunLeaseID(slotID), "tap-"+shortRunLeaseID(slotID),
		"netns-"+shortRunLeaseID(slotID),
		fmt.Sprintf("10.%d.%d.%d", slotID[13], slotID[14], slotID[15]))
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO workspace_mounts (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, workspace_id, materialized_version_id,
			runtime_instance_id, state, fencing_generation, mounted_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, $8, $9, $10, 'mounted', 2, now()
		)
	`, mountID, fixture.orgID, runLeaseTestWorkerGroup, fixture.projectID,
		fixture.environmentID, runLeaseTestRegion, fixture.workerID, workspaceID,
		versionID, runtimeID)
	var claimedAt any
	if state == "starting" {
		claimedAt = assignedAt.Add(time.Second)
	}
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO run_leases (
			id, org_id, project_id, environment_id, run_id, workspace_id, region_id,
			lease_sequence, attempt_number, worker_group_id, worker_instance_id,
			worker_epoch, runtime_instance_id, network_slot_id, network_slot_generation,
			runtime_identity_id, worker_protocol_version, requested_cpu_millis,
			requested_memory_bytes, requested_workload_disk_bytes, requested_scratch_bytes,
			requested_execution_slots, state, assigned_at, start_deadline_at,
			claimed_at, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, 1, $8, $9, 1, $10, $11, 1,
			$12, $13, 1000, 1073741824, 2147483648, 2147483648, 1,
			$14::run_lease_state, $15, now() + interval '5 minutes', $16,
			now() + interval '10 minutes'
		)
	`, leaseID, fixture.orgID, fixture.projectID, fixture.environmentID, runID,
		workspaceID, runLeaseTestRegion, runLeaseTestWorkerGroup, fixture.workerID,
		runtimeID, slotID, fixture.runtimeIdentityID, runLeaseTestProtocol,
		state, assignedAt, claimedAt)
	mustRunLeaseExec(t, ctx, tx, `
		INSERT INTO workspace_leases (
			id, org_id, worker_group_id, project_id, environment_id, region_id,
			worker_instance_id, worker_epoch, runtime_instance_id, workspace_id,
			workspace_mount_id, owner_run_lease_id, base_version_id,
			ownership_generation, writer_generation, mount_fencing_generation,
			fencing_key_fingerprint, fencing_token_hash, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, 1, $8, $9, $10, $11, $12,
			1, 1, 2, decode(repeat('00', 32), 'hex'), 'test-token-hash',
			now() + interval '10 minutes'
		)
	`, workspaceLeaseID, fixture.orgID, runLeaseTestWorkerGroup, fixture.projectID,
		fixture.environmentID, runLeaseTestRegion, fixture.workerID, runtimeID,
		workspaceID, mountID, leaseID, versionID)
	mustRunLeaseExec(t, ctx, tx, `
		UPDATE runs
		   SET current_run_lease_id = $1, first_lease_at = $2
		 WHERE id = $3
	`, leaseID, assignedAt, runID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return runLeaseWork{leaseID: leaseID, runID: runID}
}

func newRunLeaseClaimDatabase(t *testing.T, ctx context.Context) *pgxpool.Pool {
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
	if err := admin.QueryRow(ctx,
		`SELECT current_setting('server_version_num')::int`,
	).Scan(&serverVersion); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		admin.Close()
		t.Skipf("Postgres %d does not provide uuidv7()", serverVersion)
	}
	name := "helmr_run_lease_" + strings.ReplaceAll(uuid.NewString(), "-", "_")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(),
			"DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		admin.Close()
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	testDSN := parsed.String()
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

func mustRunLeaseExec(t *testing.T, ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

func runLeasePublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	value, err := publicid.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func runLeaseTestDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func runLeaseTestHash(seed string) []byte {
	sum := sha256.Sum256([]byte(seed))
	return sum[:]
}

func shortRunLeaseID(id uuid.UUID) string {
	return strings.ReplaceAll(id.String(), "-", "")[20:]
}
