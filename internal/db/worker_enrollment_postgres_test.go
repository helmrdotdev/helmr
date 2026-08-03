package db_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func activateWorkspaceWorker(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workerID uuid.UUID) {
	t.Helper()
	mustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET state = 'active', supervisor_version = 'test-worker',
		       supports_run = true, runtime_identity_id = 'test-runtime',
		       substrate_format = $2, substrate_builder_abi = $3, substrate_layout_abi = $4,
		       epoch_cpu_millis = 4000, epoch_memory_bytes = 8589934592,
		       epoch_guest_ephemeral_disk_bytes = 10737418240,
		       per_vm_cpu_millis = 2000, per_vm_memory_bytes = 2147483648,
		       per_vm_guest_ephemeral_disk_bytes = 4294967296,
		       max_vm_slots = 2, max_run_consumers = 2, max_runtime_starts = 2,
		       activated_at = now()
		 WHERE id = $1
	`, workerID, substrate.Format, substrate.BuilderABI, substrate.LayoutABI)
}

func TestRegisteringEnrollmentRetryUsesFreshNonceAndRotatesCredential(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	resourceID := "operator-host-1"
	firstWorkerID := uuid.Must(uuid.NewV7())
	first := enrollTestWorker(t, ctx, q, firstWorkerID, resourceID, true, false, []byte("first-secret"))
	second := enrollTestWorker(t, ctx, q, uuid.Must(uuid.NewV7()), resourceID, true, false, []byte("second-secret"))

	if first.WorkerInstanceID != second.WorkerInstanceID || second.WorkerInstanceID != pgvalue.UUID(firstWorkerID) {
		t.Fatalf("response-loss retry changed Control identity: first=%v second=%v", first.WorkerInstanceID, second.WorkerInstanceID)
	}
	if first.ID == second.ID || second.ClaimVersion != first.ClaimVersion+1 {
		t.Fatalf("credential fence did not rotate: first=%#v second=%#v", first, second)
	}
	var activeCredentials, revokedCredentials, consumedNonces int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE revoked_at IS NULL),
		       count(*) FILTER (WHERE revoked_at IS NOT NULL)
		  FROM worker_instance_credentials
		 WHERE worker_instance_id = $1
	`, second.WorkerInstanceID).Scan(&activeCredentials, &revokedCredentials); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM worker_enrollment_nonces
		 WHERE consumed_by_worker_instance_id = $1 AND consumed_at IS NOT NULL
	`, second.WorkerInstanceID).Scan(&consumedNonces); err != nil {
		t.Fatal(err)
	}
	if activeCredentials != 1 || revokedCredentials != 1 || consumedNonces != 2 {
		t.Fatalf("active=%d revoked=%d consumed=%d", activeCredentials, revokedCredentials, consumedNonces)
	}

	if _, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: first.WorkerInstanceID, SecretHash: []byte("first-secret"),
		ProtocolVersion: workerapi.CurrentProtocolVersion, ServiceID: pgvalue.NewUUIDv7(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("revoked response-loss credential error = %v, want pgx.ErrNoRows", err)
	}
	if _, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: second.WorkerInstanceID, SecretHash: []byte("second-secret"),
		ProtocolVersion: workerapi.CurrentProtocolVersion, ServiceID: pgvalue.NewUUIDv7(),
	}); err != nil {
		t.Fatal(err)
	}

	mustExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, network_abi
		) VALUES ('test-runtime', 'x86_64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'helmr/v0')
	`)
	activateWorkspaceWorker(t, ctx, pool, firstWorkerID)
	rejectedNonce := createTestEnrollmentNonce(t, ctx, q, dbtest.DefaultWorkerGroupID)
	_, err := q.EnrollWorkerInstance(ctx, enrollmentParams(
		rejectedNonce, uuid.Must(uuid.NewV7()), resourceID, true, false, []byte("third-secret"),
	))
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("active identity re-enrollment error = %v, want pgx.ErrNoRows", err)
	}
	var consumed bool
	if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM worker_enrollment_nonces WHERE nonce_hash = $1`, rejectedNonce).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed {
		t.Fatal("rejected active identity enrollment consumed its nonce")
	}
}

func TestConcurrentRegisteringEnrollmentSerializesCredentialReplacement(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	resourceID := "operator-host-concurrent"
	type enrollmentResult struct {
		row db.EnrollWorkerInstanceRow
		err error
	}
	results := make(chan enrollmentResult, 2)
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(2)
	for index := range 2 {
		nonce := createTestEnrollmentNonce(t, ctx, q, dbtest.DefaultWorkerGroupID)
		params := enrollmentParams(
			nonce,
			uuid.Must(uuid.NewV7()),
			resourceID,
			true,
			false,
			[]byte{byte(index + 1)},
		)
		go func() {
			ready.Done()
			<-start
			row, err := q.EnrollWorkerInstance(ctx, params)
			results <- enrollmentResult{row: row, err: err}
		}()
	}
	ready.Wait()
	close(start)
	first := <-results
	second := <-results
	for _, result := range []enrollmentResult{first, second} {
		if result.err != nil {
			t.Fatal(result.err)
		}
	}
	if first.row.WorkerInstanceID != second.row.WorkerInstanceID {
		t.Fatalf("concurrent retry created two Worker identities: first=%v second=%v", first.row.WorkerInstanceID, second.row.WorkerInstanceID)
	}
	claimVersions := []int64{first.row.ClaimVersion, second.row.ClaimVersion}
	slices.Sort(claimVersions)
	if claimVersions[0] != 1 || claimVersions[1] != 2 {
		t.Fatalf("claim versions = %v", claimVersions)
	}
	var activeCredentials, revokedCredentials, consumedNonces int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE revoked_at IS NULL),
		       count(*) FILTER (WHERE revoked_at IS NOT NULL)
		  FROM worker_instance_credentials
		 WHERE worker_instance_id = $1
	`, first.row.WorkerInstanceID).Scan(&activeCredentials, &revokedCredentials); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM worker_enrollment_nonces
		 WHERE consumed_by_worker_instance_id = $1 AND consumed_at IS NOT NULL
	`, first.row.WorkerInstanceID).Scan(&consumedNonces); err != nil {
		t.Fatal(err)
	}
	if activeCredentials != 1 || revokedCredentials != 1 || consumedNonces != 2 {
		t.Fatalf("active=%d revoked=%d consumed=%d", activeCredentials, revokedCredentials, consumedNonces)
	}
}

func TestTerminalEnrollmentCreatesFreshControlIdentity(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.Must(uuid.NewV7())
	resourceID := "operator-host-reused"
	first := enrollTestWorker(t, ctx, q, workerID, resourceID, true, false, []byte("first-secret"))
	serviceID := pgvalue.NewUUIDv7()
	firstAuth, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: first.WorkerInstanceID, SecretHash: []byte("first-secret"),
		ProtocolVersion: workerapi.CurrentProtocolVersion, ServiceID: serviceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, `UPDATE worker_instances SET state = 'lost', lost_at = now() WHERE id = $1`, workerID)
	secondWorkerID := uuid.Must(uuid.NewV7())
	second := enrollTestWorker(t, ctx, q, secondWorkerID, resourceID, true, false, []byte("second-secret"))
	if second.WorkerInstanceID != pgvalue.UUID(secondWorkerID) || second.ClaimVersion != 1 {
		t.Fatalf("terminal locator reuse did not create a fresh identity: first=%#v second=%#v", first, second)
	}
	secondAuth, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: second.WorkerInstanceID, SecretHash: []byte("second-secret"),
		ProtocolVersion: workerapi.CurrentProtocolVersion, ServiceID: serviceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !firstAuth.CurrentEpoch.Valid || !secondAuth.CurrentEpoch.Valid || secondAuth.CurrentEpoch.Int64 != 1 {
		t.Fatalf("epochs first=%v second=%v", firstAuth.CurrentEpoch, secondAuth.CurrentEpoch)
	}
}

func TestEnrollmentRetryRejectsLiveIdentityStates(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	mustExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, network_abi
		) VALUES ('test-runtime', 'x86_64', 'test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'helmr/v0')
	`)
	for _, state := range []string{"active", "draining"} {
		t.Run(state, func(t *testing.T) {
			workerID := uuid.Must(uuid.NewV7())
			resourceID := "operator-host-reject-" + uuid.NewString()
			initialSecretHash := []byte(resourceID)
			credential := enrollTestWorker(t, ctx, q, workerID, resourceID, true, false, initialSecretHash)
			switch state {
			case "active", "draining":
				authenticateTestWorker(t, ctx, q, credential, initialSecretHash)
				activateWorkspaceWorker(t, ctx, pool, workerID)
				if state == "draining" {
					mustExec(t, ctx, pool, `UPDATE worker_instances SET state = 'draining', draining_at = now() WHERE id = $1`, workerID)
				}
			}
			nonce := createTestEnrollmentNonce(t, ctx, q, dbtest.DefaultWorkerGroupID)
			_, err := q.EnrollWorkerInstance(ctx, enrollmentParams(
				nonce, uuid.Must(uuid.NewV7()), resourceID, true, false, []byte("replacement-"+resourceID),
			))
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("error = %v, want pgx.ErrNoRows", err)
			}
			var consumed bool
			if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM worker_enrollment_nonces WHERE nonce_hash = $1`, nonce).Scan(&consumed); err != nil {
				t.Fatal(err)
			}
			if consumed {
				t.Fatal("rejected enrollment consumed its nonce")
			}
		})
	}
}

func authenticateTestWorker(t *testing.T, ctx context.Context, q *db.Queries, credential db.EnrollWorkerInstanceRow, secretHash []byte) {
	t.Helper()
	if _, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: credential.WorkerInstanceID, SecretHash: secretHash,
		ProtocolVersion: workerapi.CurrentProtocolVersion, ServiceID: pgvalue.NewUUIDv7(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentRoleMustBeAllowedByLogicalWorkerGroup(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	reconcileTestWorkerGroup(t, ctx, q, dbtest.DefaultWorkerGroupID, true, false)
	nonce := createTestEnrollmentNonce(t, ctx, q, dbtest.DefaultWorkerGroupID)
	params := enrollmentParams(nonce, uuid.Must(uuid.NewV7()), "operator-host-2", false, true, []byte("build-secret"))
	if _, err := q.EnrollWorkerInstance(ctx, params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("disallowed build role error = %v, want pgx.ErrNoRows", err)
	}
	var consumed bool
	if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM worker_enrollment_nonces WHERE nonce_hash = $1`, nonce).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed {
		t.Fatal("rejected role enrollment consumed its nonce")
	}
}

func TestWorkerGroupRoleNarrowingFencesCurrentCredential(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	workerID := uuid.Must(uuid.NewV7())
	credential := enrollTestWorker(t, ctx, q, workerID, "operator-host-3", true, true, []byte("both-role-secret"))
	if _, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, SupportsBuild: true, WorkerInstanceID: credential.WorkerInstanceID,
		SecretHash: []byte("both-role-secret"), ProtocolVersion: workerapi.CurrentProtocolVersion, ServiceID: pgvalue.NewUUIDv7(),
	}); err != nil {
		t.Fatal(err)
	}
	reconcileTestWorkerGroup(t, ctx, q, dbtest.DefaultWorkerGroupID, true, false)
	if _, err := q.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: credential.WorkerInstanceID,
		SecretHash: []byte("both-role-secret"), ProtocolVersion: workerapi.CurrentProtocolVersion, ServiceID: pgvalue.NewUUIDv7(),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("narrowed credential error = %v, want pgx.ErrNoRows", err)
	}
	var state string
	var revoked bool
	if err := pool.QueryRow(ctx, `
		SELECT worker_instances.state, worker_instance_credentials.revoked_at IS NOT NULL
		  FROM worker_instances JOIN worker_instance_credentials
		    ON worker_instance_credentials.worker_instance_id = worker_instances.id
		 WHERE worker_instance_credentials.id = $1
	`, credential.ID).Scan(&state, &revoked); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerInstanceStateLost || !revoked {
		t.Fatalf("state=%q revoked=%t", state, revoked)
	}
}

func enrollTestWorker(t *testing.T, ctx context.Context, q *db.Queries, workerID uuid.UUID, resourceID string, allowsRun bool, allowsBuild bool, secretHash []byte) db.EnrollWorkerInstanceRow {
	t.Helper()
	nonce := createTestEnrollmentNonce(t, ctx, q, dbtest.DefaultWorkerGroupID)
	row, err := q.EnrollWorkerInstance(ctx, enrollmentParams(nonce, workerID, resourceID, allowsRun, allowsBuild, secretHash))
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func createTestEnrollmentNonce(t *testing.T, ctx context.Context, q *db.Queries, groupID string) []byte {
	t.Helper()
	nonce := []byte(uuid.NewString())
	if _, err := q.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
		ID: pgvalue.NewUUIDv7(), NonceHash: nonce, WorkerGroupID: groupID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	return nonce
}

func enrollmentParams(nonce []byte, workerID uuid.UUID, resourceID string, allowsRun bool, allowsBuild bool, secretHash []byte) db.EnrollWorkerInstanceParams {
	return db.EnrollWorkerInstanceParams{
		NonceHash: nonce, WorkerGroupID: dbtest.DefaultWorkerGroupID,
		AllowsRun: allowsRun, AllowsBuild: allowsBuild, ProtocolVersion: workerapi.CurrentProtocolVersion,
		WorkerInstanceID: pgvalue.UUID(workerID), ResourceID: resourceID, CurrentServiceID: pgvalue.NewUUIDv7(),
		CredentialID: pgvalue.NewUUIDv7(), KeyPrefix: uuid.NewString(), SecretHash: secretHash,
	}
}

func reconcileTestWorkerGroup(t *testing.T, ctx context.Context, q *db.Queries, groupID string, allowsRun bool, allowsBuild bool) {
	t.Helper()
	buildExecutors := int32(0)
	if allowsBuild {
		buildExecutors = 1
	}
	if _, err := q.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		AllowsRun: allowsRun, AllowsBuild: allowsBuild, ProtocolVersion: workerapi.CurrentProtocolVersion,
		RequiredCpuMillis: 1, RequiredMemoryBytes: 1, RequiredGuestEphemeralDiskBytes: 1,
		RequiredVmSlots: 1, RequiredBuildExecutors: buildExecutors, ObservationTtlSeconds: 120,
	}); err != nil {
		t.Fatal(err)
	}
}
