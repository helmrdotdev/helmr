package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRegisterFinalizedDeploymentBundlePostgresRollsBackEveryAuthorityRow(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	prepared := fixture.prepared
	prepared.definitions = []finalizedDeploymentDefinition{{
		kind: "invalid", declaredID: "invalid", manifest: []byte(`{}`), manifestDigest: bytes.Repeat([]byte{1}, 32),
	}}
	request := fixture.idempotencyRequest(t, "rollback")
	if _, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, prepared, request,
	); err == nil {
		t.Fatal("registration with an invalid definition succeeded")
	}
	for table, query := range map[string]string{
		"CAS ownership":      `SELECT count(*) FROM cas_objects WHERE org_id = $1`,
		"artifacts":          `SELECT count(*) FROM artifacts WHERE environment_id = $1`,
		"deployments":        `SELECT count(*) FROM deployments WHERE environment_id = $1`,
		"definitions":        `SELECT count(*) FROM deployment_definitions WHERE environment_id = $1`,
		"idempotency claims": `SELECT count(*) FROM idempotency_claims WHERE environment_id = $1`,
	} {
		argument := any(fixture.environmentID)
		if table == "CAS ownership" {
			argument = fixture.orgID
		}
		var count int
		if err := fixture.pool.QueryRow(t.Context(), query, argument).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0 after rollback", table, count)
		}
	}
}

func TestRegisterFinalizedDeploymentBundlePostgresConvergesConcurrentExactRequests(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	responses := make([]string, 2)
	errors := make([]error, 2)
	requests := []idempotency.Request{
		fixture.idempotencyRequest(t, "concurrent"),
		fixture.idempotencyRequest(t, "concurrent"),
	}
	var wait sync.WaitGroup
	for index := range responses {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response, err := fixture.server.registerFinalizedDeploymentBundle(
				t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
				requests[index],
			)
			responses[index], errors[index] = response.ID, err
		}(index)
	}
	wait.Wait()
	for _, err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if responses[0] == "" || responses[0] != responses[1] {
		t.Fatalf("deployment IDs = %v", responses)
	}
	for name, query := range map[string]string{
		"deployment":        `SELECT count(*) FROM deployments WHERE environment_id = $1`,
		"program artifact":  `SELECT count(*) FROM artifacts WHERE environment_id = $1`,
		"idempotency claim": `SELECT count(*) FROM idempotency_claims WHERE environment_id = $1 AND state = 'completed'`,
	} {
		var count int
		if err := fixture.pool.QueryRow(t.Context(), query, fixture.environmentID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s count = %d, want 1", name, count)
		}
	}
	var ownership int
	if err := fixture.pool.QueryRow(
		t.Context(), `SELECT count(*) FROM cas_objects WHERE org_id = $1`, fixture.orgID,
	).Scan(&ownership); err != nil {
		t.Fatal(err)
	}
	if ownership != 2 {
		t.Fatalf("CAS ownership count = %d, want root and Program object", ownership)
	}
}

func TestRegisterFinalizedDeploymentBundlePostgresRejectsIdempotencyKeyForAnotherBundle(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	if _, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "shared"),
	); err != nil {
		t.Fatal(err)
	}
	changed := fixture.prepared
	changed.root.Digest = "sha256:" + string(bytes.Repeat([]byte{'d'}, 64))
	_, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, changed,
		fixture.idempotencyRequestFor(t, fixture.environmentID, "shared", changed.root.Digest),
	)
	var conflict idempotency.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want idempotency conflict", err)
	}
}

func TestFinishFinalizedDeploymentBundlePostgresReplaysAvailableExactBundleWithoutReadingObjects(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	created, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "first"),
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &deploymentFinalizeTrackingStore{
		descriptor: fixture.prepared.objects[0],
		body:       make([]byte, fixture.prepared.objects[0].SizeBytes),
	}
	replayed, err := fixture.server.finishFinalizedDeploymentBundle(
		t.Context(), store, fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "replay"),
		func(deploymentFinalizeProgress) error {
			t.Fatal("replay verified object bytes")
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != created.ID || store.statCount != 1 || store.getCount != 0 {
		t.Fatalf("replayed = %+v stats = %d gets = %d", replayed, store.statCount, store.getCount)
	}
}

func TestFinishFinalizedDeploymentBundlePostgresFailsClosedWhenReplayObjectIsUnavailable(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	if _, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "first"),
	); err != nil {
		t.Fatal(err)
	}
	store := &deploymentFinalizeTrackingStore{
		descriptor: fixture.prepared.objects[0],
		statErr:    errors.New("missing"),
	}
	if _, err := fixture.server.finishFinalizedDeploymentBundle(
		t.Context(), store, fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "replay"), func(deploymentFinalizeProgress) error { return nil },
	); err == nil {
		t.Fatal("replay with an unavailable object succeeded")
	}
	if store.getCount != 0 {
		t.Fatalf("object reads = %d, want 0", store.getCount)
	}
}

func TestFinishFinalizedDeploymentBundlePostgresVerifiesSameDigestInAnotherEnvironment(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	if _, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "first"),
	); err != nil {
		t.Fatal(err)
	}
	otherEnvironment := pgvalue.UUID(uuid.NewV7())
	if _, err := fixture.server.db.CreateEnvironment(t.Context(), db.CreateEnvironmentParams{
		ID: otherEnvironment, OrgID: pgvalue.UUID(fixture.orgID), ProjectID: fixture.projectID,
		Slug: "preview", Name: "Preview", ColorHex: "#315FCE",
	}); err != nil {
		t.Fatal(err)
	}
	store := &deploymentFinalizeTrackingStore{
		descriptor: fixture.prepared.objects[0],
		body:       make([]byte, fixture.prepared.objects[0].SizeBytes),
	}
	if _, err := fixture.server.finishFinalizedDeploymentBundle(
		t.Context(), store, fixture.orgID, fixture.projectID, otherEnvironment, fixture.prepared,
		fixture.idempotencyRequestFor(t, otherEnvironment, "other", fixture.prepared.root.Digest),
		func(deploymentFinalizeProgress) error { return nil },
	); err == nil {
		t.Fatal("unverified object created a deployment in another environment")
	}
	if store.getCount != 1 {
		t.Fatalf("object reads = %d, want full verification", store.getCount)
	}
}

func TestRegisterFinalizedDeploymentBundlePostgresMaximumBulkBudget(t *testing.T) {
	if os.Getenv("HELMR_TEST_DEPLOYMENT_FINALIZE_SCALE") != "1" {
		t.Skip("HELMR_TEST_DEPLOYMENT_FINALIZE_SCALE is not set")
	}
	fixture := newDeploymentFinalizePostgresFixture(t)
	fixture.prepared.definitions = deploymentFinalizeScaleDefinitions(10_000)
	beginner := &deploymentFinalizeCountingBeginner{pool: fixture.pool}
	fixture.server.tx = beginner

	var walBefore string
	var tempFilesBefore, tempBytesBefore int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT pg_current_wal_insert_lsn()::text, temp_files, temp_bytes
		  FROM pg_stat_database
		 WHERE datname = current_database()
	`).Scan(&walBefore, &tempFilesBefore, &tempBytesBefore); err != nil {
		t.Fatal(err)
	}

	baseline, stopHeapSampling := startDeploymentFinalizeHeapSampling()
	started := time.Now()
	response, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "maximum-bulk-budget"),
	)
	elapsed := time.Since(started)
	heapDelta := stopHeapSampling()
	if err != nil {
		t.Fatal(err)
	}
	if response.ID == "" {
		t.Fatal("maximum finalization returned no deployment")
	}

	var walBytes, tempFilesAfter, tempBytesAfter int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT pg_wal_lsn_diff(pg_current_wal_insert_lsn(), $1::pg_lsn)::bigint,
		       temp_files,
		       temp_bytes
		  FROM pg_stat_database
		 WHERE datname = current_database()
	`, walBefore).Scan(&walBytes, &tempFilesAfter, &tempBytesAfter); err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := fixture.pool.QueryRow(
		t.Context(), `SELECT count(*) FROM deployment_definitions WHERE environment_id = $1`, fixture.environmentID,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"deployment finalization bulk budget: count=%d elapsed=%s statements=%d bulk_statements=%d payload_bytes=%d heap_baseline=%d heap_delta=%d wal_bytes=%d temp_files=%d temp_bytes=%d",
		stored, elapsed, beginner.statements.Load(), beginner.bulkStatements.Load(), beginner.bulkPayloadBytes.Load(),
		baseline, heapDelta, walBytes, tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore,
	)
	if stored != 10_000 {
		t.Fatalf("stored definitions = %d, want 10000", stored)
	}
	if statements := beginner.statements.Load(); statements > 10 {
		t.Fatalf("transaction statements = %d, budget <= 10", statements)
	}
	if bulkStatements := beginner.bulkStatements.Load(); bulkStatements != 1 {
		t.Fatalf("bulk statements = %d, want 1", bulkStatements)
	}
	if elapsed > 1850*time.Millisecond {
		t.Fatalf("elapsed/lock time = %s, budget <= 1.85s", elapsed)
	}
	if heapDelta > 8<<20 {
		t.Fatalf("additional sampled heap = %d, budget <= %d", heapDelta, 8<<20)
	}
	if payload := beginner.bulkPayloadBytes.Load(); payload > 5_700_000 {
		t.Fatalf("bulk statement/payload bytes = %d, budget <= 5700000", payload)
	}
	if walBytes > 64<<20 {
		t.Fatalf("WAL bytes = %d, budget <= %d", walBytes, 64<<20)
	}
	if tempFilesAfter != tempFilesBefore || tempBytesAfter != tempBytesBefore {
		t.Fatalf(
			"bulk insert created temp files/bytes: files=%d bytes=%d",
			tempFilesAfter-tempFilesBefore, tempBytesAfter-tempBytesBefore,
		)
	}
}

func TestRegisterFinalizedDeploymentBundlePostgresCancellationRollsBackBulk(t *testing.T) {
	if os.Getenv("HELMR_TEST_DEPLOYMENT_FINALIZE_SCALE") != "1" {
		t.Skip("HELMR_TEST_DEPLOYMENT_FINALIZE_SCALE is not set")
	}
	fixture := newDeploymentFinalizePostgresFixture(t)
	fixture.prepared.definitions = deploymentFinalizeScaleDefinitions(10_000)
	bulkStarted := make(chan struct{}, 1)
	beginner := &deploymentFinalizeCountingBeginner{pool: fixture.pool, bulkStarted: bulkStarted}
	fixture.server.tx = beginner

	blocker, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(context.Background()) })
	if _, err := blocker.Exec(t.Context(), `LOCK TABLE deployment_definitions IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := fixture.server.registerFinalizedDeploymentBundle(
			ctx, fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
			fixture.idempotencyRequest(t, "cancel-bulk"),
		)
		done <- err
	}()
	select {
	case <-bulkStarted:
	case <-time.After(time.Second):
		t.Fatal("bulk definition insert did not start")
	}
	backendPID := beginner.backendPID.Load()
	deadline := time.Now().Add(time.Second)
	for {
		var waitingOnBulkLock bool
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1
				  FROM pg_stat_activity
				 WHERE pid = $1
				   AND wait_event_type = 'Lock'
				   AND position('CreateDeploymentDefinitions' IN query) > 0
			)
		`, backendPID).Scan(&waitingOnBulkLock); err != nil {
			t.Fatal(err)
		}
		if waitingOnBulkLock {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bulk definition insert did not reach the PostgreSQL lock wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
	canceledAt := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled finalization succeeded")
		}
	case <-time.After(1150 * time.Millisecond):
		t.Fatal("canceled finalization exceeded 1.15s rollback budget")
	}
	t.Logf("deployment finalization bulk cancellation rollback: %s", time.Since(canceledAt))
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	var definitions, deployments int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT (SELECT count(*) FROM deployment_definitions WHERE environment_id = $1),
		       (SELECT count(*) FROM deployments WHERE environment_id = $1)
	`, fixture.environmentID).Scan(&definitions, &deployments); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || deployments != 0 {
		t.Fatalf("definitions/deployments after cancellation = %d/%d, want 0/0", definitions, deployments)
	}
}

func TestCreateDeploymentDefinitionsPostgresRejectsMalformedBatchesAtomically(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	created, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "malformed-batches"),
	)
	if err != nil {
		t.Fatal(err)
	}
	deploymentUUID, err := uuid.Parse(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	queries := db.New(fixture.pool)

	t.Run("mismatched cardinality", func(t *testing.T) {
		params := deploymentDefinitionBatchParams(fixture.environmentID, pgvalue.UUID(deploymentUUID), 2)
		params.Kinds = params.Kinds[:1]
		inserted, err := queries.CreateDeploymentDefinitions(t.Context(), params)
		if err != nil {
			t.Fatal(err)
		}
		if inserted != 0 {
			t.Fatalf("inserted = %d, want 0", inserted)
		}
	})
	t.Run("over admitted maximum", func(t *testing.T) {
		params := deploymentDefinitionBatchParams(fixture.environmentID, pgvalue.UUID(deploymentUUID), 10_001)
		inserted, err := queries.CreateDeploymentDefinitions(t.Context(), params)
		if err != nil {
			t.Fatal(err)
		}
		if inserted != 0 {
			t.Fatalf("inserted = %d, want 0", inserted)
		}
	})
	t.Run("duplicate generated ID", func(t *testing.T) {
		params := deploymentDefinitionBatchParams(fixture.environmentID, pgvalue.UUID(deploymentUUID), 2)
		params.Ids[1] = params.Ids[0]
		if _, err := queries.CreateDeploymentDefinitions(t.Context(), params); err == nil {
			t.Fatal("duplicate definition IDs succeeded")
		}
	})
	t.Run("duplicate membership", func(t *testing.T) {
		params := deploymentDefinitionBatchParams(fixture.environmentID, pgvalue.UUID(deploymentUUID), 2)
		params.DeclaredIds[1] = params.DeclaredIds[0]
		if _, err := queries.CreateDeploymentDefinitions(t.Context(), params); err == nil {
			t.Fatal("duplicate definition membership succeeded")
		}
	})
	t.Run("cross-scope artifact", func(t *testing.T) {
		otherEnvironmentID := pgvalue.UUID(uuid.NewV7())
		if _, err := queries.CreateEnvironment(t.Context(), db.CreateEnvironmentParams{
			ID: otherEnvironmentID, OrgID: pgvalue.UUID(fixture.orgID), ProjectID: fixture.projectID,
			Slug: "other", Name: "Other", ColorHex: "#315FCE",
		}); err != nil {
			t.Fatal(err)
		}
		digest := "sha256:" + strings.Repeat("d", 64)
		if _, err := queries.UpsertCasObject(t.Context(), db.UpsertCasObjectParams{
			OrgID: pgvalue.UUID(fixture.orgID), Digest: digest, SizeBytes: 1, MediaType: "application/octet-stream",
		}); err != nil {
			t.Fatal(err)
		}
		artifact, err := queries.CreateArtifact(t.Context(), db.CreateArtifactParams{
			ID: pgvalue.UUID(uuid.NewV7()), OrgID: pgvalue.UUID(fixture.orgID), ProjectID: fixture.projectID,
			EnvironmentID: otherEnvironmentID, Digest: digest, Kind: db.ArtifactKindWorkspaceImage,
			SizeBytes: 1, MediaType: "application/octet-stream",
		})
		if err != nil {
			t.Fatal(err)
		}
		params := deploymentDefinitionBatchParams(fixture.environmentID, pgvalue.UUID(deploymentUUID), 1)
		params.Kinds[0] = "sandbox"
		params.ArtifactIds[0] = artifact.ID
		if _, err := queries.CreateDeploymentDefinitions(t.Context(), params); err == nil {
			t.Fatal("cross-scope artifact succeeded")
		}
	})

	var stored int
	if err := fixture.pool.QueryRow(
		t.Context(), `SELECT count(*) FROM deployment_definitions WHERE deployment_id = $1`, deploymentUUID,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Fatalf("definitions after malformed batches = %d, want 0", stored)
	}
}

func deploymentFinalizeScaleDefinitions(count int) []finalizedDeploymentDefinition {
	definitions := make([]finalizedDeploymentDefinition, count)
	for index := range definitions {
		definitions[index] = finalizedDeploymentDefinition{
			kind: "task", declaredID: fmt.Sprintf("task-%05d", index),
			manifest: []byte(`{}`), manifestDigest: make([]byte, 32),
		}
	}
	return definitions
}

func deploymentDefinitionBatchParams(
	environmentID, deploymentID pgtype.UUID,
	count int,
) db.CreateDeploymentDefinitionsParams {
	params := db.CreateDeploymentDefinitionsParams{
		Ids: make([]pgtype.UUID, count), Kinds: make([]string, count),
		DeclaredIds: make([]string, count), Manifests: make([][]byte, count),
		ManifestDigests: make([][]byte, count), ArtifactIds: make([]pgtype.UUID, count),
		EnvironmentID: environmentID, DeploymentID: deploymentID,
		ManifestVersion: deployment.DeploymentPlanFormatVersion,
	}
	for index := range count {
		params.Ids[index] = pgvalue.UUID(uuid.NewV7())
		params.Kinds[index] = "task"
		params.DeclaredIds[index] = fmt.Sprintf("task-%05d", index)
		params.Manifests[index] = []byte(`{}`)
		params.ManifestDigests[index] = make([]byte, 32)
	}
	return params
}

type deploymentFinalizeCountingBeginner struct {
	pool             *pgxpool.Pool
	statements       atomic.Int64
	bulkStatements   atomic.Int64
	bulkPayloadBytes atomic.Int64
	bulkStarted      chan struct{}
	backendPID       atomic.Int64
}

func (b *deploymentFinalizeCountingBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if b.bulkStarted != nil {
		var backendPID int64
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
			_ = tx.Rollback(ctx)
			return nil, err
		}
		b.backendPID.Store(backendPID)
	}
	return &deploymentFinalizeCountingTx{Tx: tx, owner: b}, nil
}

type deploymentFinalizeCountingTx struct {
	pgx.Tx
	owner *deploymentFinalizeCountingBeginner
}

func (tx *deploymentFinalizeCountingTx) Exec(
	ctx context.Context, sql string, args ...any,
) (pgconn.CommandTag, error) {
	tx.record(sql, args)
	return tx.Tx.Exec(ctx, sql, args...)
}

func (tx *deploymentFinalizeCountingTx) Query(
	ctx context.Context, sql string, args ...any,
) (pgx.Rows, error) {
	tx.record(sql, args)
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *deploymentFinalizeCountingTx) QueryRow(
	ctx context.Context, sql string, args ...any,
) pgx.Row {
	tx.record(sql, args)
	return tx.Tx.QueryRow(ctx, sql, args...)
}

func (tx *deploymentFinalizeCountingTx) record(sql string, args []any) {
	tx.owner.statements.Add(1)
	if !strings.Contains(sql, "-- name: CreateDeploymentDefinitions") {
		return
	}
	tx.owner.bulkStatements.Add(1)
	tx.owner.bulkPayloadBytes.Add(int64(len(sql)) + deploymentDefinitionArgumentBytes(args))
	if tx.owner.bulkStarted != nil {
		select {
		case tx.owner.bulkStarted <- struct{}{}:
		default:
		}
	}
}

func deploymentDefinitionArgumentBytes(args []any) int64 {
	var total int64
	for _, arg := range args {
		switch value := arg.(type) {
		case []pgtype.UUID:
			total += int64(len(value) * 16)
		case []string:
			for _, item := range value {
				total += int64(len(item))
			}
		case [][]byte:
			for _, item := range value {
				total += int64(len(item))
			}
		case pgtype.UUID:
			total += 16
		case int32:
			total += 4
		}
	}
	return total
}

func startDeploymentFinalizeHeapSampling() (uint64, func() uint64) {
	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)
	var peak atomic.Uint64
	peak.Store(baseline.Alloc)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var current runtime.MemStats
				runtime.ReadMemStats(&current)
				for observed := peak.Load(); current.Alloc > observed && !peak.CompareAndSwap(observed, current.Alloc); observed = peak.Load() {
				}
			case <-stop:
				return
			}
		}
	}()
	return baseline.Alloc, func() uint64 {
		close(stop)
		<-done
		return peak.Load() - baseline.Alloc
	}
}

var _ TxBeginner = (*deploymentFinalizeCountingBeginner)(nil)

type deploymentFinalizeTrackingStore struct {
	cas.UploadStore
	descriptor cas.Descriptor
	body       []byte
	statErr    error
	statCount  int
	getCount   int
}

func (s *deploymentFinalizeTrackingStore) Stat(context.Context, string) (cas.Object, error) {
	s.statCount++
	if s.statErr != nil {
		return cas.Object{}, s.statErr
	}
	return cas.Object{
		Digest: s.descriptor.Digest, SizeBytes: s.descriptor.SizeBytes, MediaType: s.descriptor.MediaType,
	}, nil
}

func (s *deploymentFinalizeTrackingStore) Get(context.Context, string) (io.ReadCloser, error) {
	s.getCount++
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

type deploymentFinalizePostgresFixture struct {
	pool          *pgxpool.Pool
	server        *Server
	orgID         uuid.UUID
	projectID     pgtype.UUID
	environmentID pgtype.UUID
	prepared      finalizedDeploymentBundle
}

func newDeploymentFinalizePostgresFixture(t *testing.T) deploymentFinalizePostgresFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	regionID := "test-region"
	if _, err := queries.CreateRegion(t.Context(), db.CreateRegionParams{ID: regionID, DisplayName: "Test"}); err != nil {
		t.Fatal(err)
	}
	orgID := uuid.NewV7()
	projectUUID := uuid.NewV7()
	environmentUUID := uuid.NewV7()
	projectID := pgvalue.UUID(projectUUID)
	environmentID := pgvalue.UUID(environmentUUID)
	if _, err := queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: "Finalize", Slug: "finalize",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateProject(t.Context(), db.CreateProjectParams{
		ID: projectID, OrgID: pgvalue.UUID(orgID), DefaultRegionID: regionID,
		Slug: "finalize", Name: "Finalize", IsDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateEnvironment(t.Context(), db.CreateEnvironmentParams{
		ID: environmentID, OrgID: pgvalue.UUID(orgID), ProjectID: projectID,
		Slug: "staging", Name: "Staging", ColorHex: "#315FCE", IsDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	program := cas.Descriptor{
		Digest:    "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)),
		SizeBytes: 4096, MediaType: deployment.ProgramArtifactMediaType,
	}
	prepared := finalizedDeploymentBundle{
		root: cas.Descriptor{
			Digest:    "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
			SizeBytes: 512, MediaType: deployment.DeploymentBundleMediaType,
		},
		bundle: deployment.DeploymentBundle{
			Runtime: deployment.DeploymentBundleRuntime{Artifact: deployment.BundleObject{
				Digest: "sha256:" + string(bytes.Repeat([]byte{'c'}, 64)),
			}},
			Program: deployment.ProgramOutput{Artifact: deployment.ProgramDescriptor{
				Digest: program.Digest, SizeBytes: program.SizeBytes, MediaType: program.MediaType,
			}},
		},
		objects:     []cas.Descriptor{program},
		indexDigest: bytes.Repeat([]byte{2}, 32),
		queueConfig: []byte(`{"formatVersion":0,"queues":[]}`),
	}
	return deploymentFinalizePostgresFixture{
		pool: database.Pool, server: &Server{db: queries, tx: database.Pool},
		orgID: orgID, projectID: projectID, environmentID: environmentID, prepared: prepared,
	}
}

func (f deploymentFinalizePostgresFixture) idempotencyRequest(t *testing.T, key string) idempotency.Request {
	t.Helper()
	return f.idempotencyRequestFor(t, f.environmentID, key, f.prepared.root.Digest)
}

func (f deploymentFinalizePostgresFixture) idempotencyRequestFor(
	t *testing.T,
	environmentID pgtype.UUID,
	key string,
	bundleDigest string,
) idempotency.Request {
	t.Helper()
	environmentUUID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := pgvalue.UUIDValue(f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := idempotency.NewDeploymentFinalizeRequest(
		environmentUUID, projectID, key,
		idempotency.DeploymentFinalizeFingerprint{BundleDigest: bundleDigest},
	)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
