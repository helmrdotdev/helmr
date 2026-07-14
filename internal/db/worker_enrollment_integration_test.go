package db_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerEnrollmentConsumesNonceAndRotatesCredentialAtomically(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)

	createCredential := func(nonceHash []byte, resourceID string, secretHash []byte) db.EnrollWorkerInstanceRow {
		t.Helper()
		row, err := queries.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
			NonceHash: nonceHash, WorkerGroupID: dbtest.DefaultWorkerGroupID,
			AllowsRun: true, AllowsBuild: true, ProtocolVersion: auth.WorkerProtocolVersion,
			WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: resourceID,
			CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(),
			SecretHash: secretHash, EnrollmentPolicyFingerprint: "sha256:test-worker-group",
			AttestationFingerprint: "sha256:test-attestation",
		})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	createNonce := func(hash []byte) {
		t.Helper()
		if _, err := queries.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), NonceHash: hash,
			WorkerGroupID: dbtest.DefaultWorkerGroupID,
			ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
	}

	firstNonce := []byte("first-nonce-hash")
	createNonce(firstNonce)
	firstSecret := []byte("first-secret")
	first := createCredential(firstNonce, "i-stable", firstSecret)
	firstService := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	firstAuth, err := queries.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, SupportsBuild: true, WorkerInstanceID: first.WorkerInstanceID,
		SecretHash: firstSecret, ProtocolVersion: auth.WorkerProtocolVersion, ServiceID: firstService,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
		NonceHash: firstNonce, WorkerGroupID: dbtest.DefaultWorkerGroupID,
		AllowsRun: true, AllowsBuild: true, ProtocolVersion: auth.WorkerProtocolVersion,
		WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: "i-other",
		CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(), SecretHash: []byte("replay"),
		EnrollmentPolicyFingerprint: "sha256:test-worker-group", AttestationFingerprint: "sha256:test-attestation",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replayed enrollment error = %v, want pgx.ErrNoRows", err)
	}

	secondNonce := []byte("second-nonce-hash")
	createNonce(secondNonce)
	secondSecret := []byte("second-secret")
	second := createCredential(secondNonce, "i-stable", secondSecret)
	if first.WorkerInstanceID != second.WorkerInstanceID {
		t.Fatalf("stable resource changed worker identity: %v -> %v", first.WorkerInstanceID, second.WorkerInstanceID)
	}
	if first.ID == second.ID {
		t.Fatal("replacement enrollment reused credential id")
	}
	var enrolledEpoch pgtype.Int8
	var enrollmentFence pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT current_epoch, current_service_id FROM worker_instances WHERE id = $1`, second.WorkerInstanceID).Scan(&enrolledEpoch, &enrollmentFence); err != nil {
		t.Fatal(err)
	}
	if enrolledEpoch != firstAuth.CurrentEpoch || enrollmentFence == firstService {
		t.Fatalf("re-enrollment epoch=%v service=%v, want preserved epoch and a forced service fence", enrolledEpoch, enrollmentFence)
	}
	secondAuth, err := queries.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, SupportsBuild: true, WorkerInstanceID: second.WorkerInstanceID,
		SecretHash: secondSecret, ProtocolVersion: auth.WorkerProtocolVersion,
		ServiceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondAuth.CurrentEpoch.Int64 != firstAuth.CurrentEpoch.Int64+1 {
		t.Fatalf("replacement epoch=%d, want %d", secondAuth.CurrentEpoch.Int64, firstAuth.CurrentEpoch.Int64+1)
	}

	var active int
	var revoked int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE revoked_at IS NULL), count(*) FILTER (WHERE revoked_at IS NOT NULL)
		  FROM worker_instance_credentials WHERE worker_instance_id = $1
	`, first.WorkerInstanceID).Scan(&active, &revoked); err != nil {
		t.Fatal(err)
	}
	if active != 1 || revoked != 1 {
		t.Fatalf("credential state active=%d revoked=%d", active, revoked)
	}
	var consumed int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM worker_enrollment_nonces
		 WHERE consumed_by_worker_instance_id = $1 AND consumed_at IS NOT NULL
	`, first.WorkerInstanceID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 2 {
		t.Fatalf("consumed nonces = %d, want 2", consumed)
	}
	mustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET state = 'disabled', disabled_at = now(),
		       drain_cleanup_fingerprint = $2,
		       drain_cleanup_evidence = '{"inventory_empty":true}'::jsonb
		 WHERE id = $1
	`, second.WorkerInstanceID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := queries.ClaimFleetWorkerTermination(ctx, db.ClaimFleetWorkerTerminationParams{
		WorkerInstanceID: second.WorkerInstanceID, WorkerGroupID: dbtest.DefaultWorkerGroupID,
	}); err != nil {
		t.Fatal(err)
	}
	thirdNonce := []byte("third-nonce-hash")
	createNonce(thirdNonce)
	if _, err := queries.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
		NonceHash: thirdNonce, WorkerGroupID: dbtest.DefaultWorkerGroupID,
		AllowsRun: true, AllowsBuild: true, ProtocolVersion: auth.WorkerProtocolVersion,
		WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: "i-stable",
		CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(),
		SecretHash: []byte("third-secret"), EnrollmentPolicyFingerprint: "sha256:test-worker-group",
		AttestationFingerprint: "sha256:test-attestation",
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("termination-claimed resource enrollment error = %v, want pgx.ErrNoRows", err)
	}
	var thirdConsumed bool
	if err := pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM worker_enrollment_nonces WHERE nonce_hash = $1`, thirdNonce).Scan(&thirdConsumed); err != nil {
		t.Fatal(err)
	}
	if thirdConsumed {
		t.Fatal("failed enrollment consumed its managed nonce")
	}
}

func TestWorkerGroupPolicyChangeRevokesExistingEnrollment(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	groupID := "managed-policy-" + shortUUID(uuid.Must(uuid.NewV7()))
	ensure := func(fingerprint string, allowed []string) db.ReconcileWorkerGroupRow {
		t.Helper()
		group, err := queries.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
			ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
			EnrollmentPolicyFingerprint: fingerprint, AllowsRun: true,
			AllowsBuild: false, ProtocolVersion: auth.WorkerProtocolVersion,
			AllowedAttestationFingerprints: allowed,
			RequiredCpuMillis:              1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredVmSlots: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		return group
	}

	initial := ensure("sha256:policy-one", []string{"sha256:policy-one-attestation"})
	nonceHash := []byte("managed-policy-nonce")
	if _, err := queries.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), NonceHash: nonceHash,
		WorkerGroupID: groupID,
		ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	credential, err := queries.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
		NonceHash: nonceHash, WorkerGroupID: groupID, AllowsRun: true,
		ProtocolVersion:  auth.WorkerProtocolVersion,
		WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: "i-policy-test",
		CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(), SecretHash: []byte("secret"),
		EnrollmentPolicyFingerprint: "sha256:policy-one", AttestationFingerprint: "sha256:policy-one-attestation",
	})
	if err != nil {
		t.Fatal(err)
	}

	unchanged := ensure("sha256:policy-one", []string{"sha256:policy-one-attestation"})
	if unchanged.ClaimVersion != initial.ClaimVersion {
		t.Fatalf("unchanged policy claim version = %d, want %d", unchanged.ClaimVersion, initial.ClaimVersion)
	}
	var revokedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM worker_instance_credentials WHERE id = $1`, credential.ID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt.Valid {
		t.Fatal("unchanged policy revoked credential")
	}

	expanded := ensure("sha256:policy-expanded", []string{"sha256:policy-one-attestation", "sha256:policy-two-attestation"})
	if expanded.ClaimVersion != initial.ClaimVersion+1 {
		t.Fatalf("expanded policy claim version = %d, want %d", expanded.ClaimVersion, initial.ClaimVersion+1)
	}
	if err := pool.QueryRow(ctx, `SELECT revoked_at FROM worker_instance_credentials WHERE id = $1`, credential.ID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt.Valid {
		t.Fatal("additive rollout policy revoked a still-compliant credential")
	}

	changed := ensure("sha256:policy-two", []string{"sha256:policy-two-attestation"})
	if changed.ClaimVersion != initial.ClaimVersion+2 {
		t.Fatalf("narrowed policy claim version = %d, want %d", changed.ClaimVersion, initial.ClaimVersion+2)
	}
	for _, stale := range []struct {
		name        string
		policy      string
		attestation string
	}{
		{name: "policy", policy: "sha256:policy-one", attestation: "sha256:policy-two-attestation"},
		{name: "attestation", policy: "sha256:policy-two", attestation: "sha256:policy-one-attestation"},
	} {
		t.Run("rejects stale "+stale.name, func(t *testing.T) {
			staleNonce := []byte("managed-policy-stale-" + stale.name)
			if _, err := queries.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
				ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), NonceHash: staleNonce, WorkerGroupID: groupID,
				ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := queries.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
				NonceHash: staleNonce, WorkerGroupID: groupID, AllowsRun: true,
				ProtocolVersion: auth.WorkerProtocolVersion, EnrollmentPolicyFingerprint: stale.policy,
				WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: "i-stale-" + stale.name,
				CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(), SecretHash: []byte("stale-secret"),
				AttestationFingerprint: stale.attestation,
			}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("stale %s enrollment error = %v, want pgx.ErrNoRows", stale.name, err)
			}
		})
	}
	var workerState db.WorkerInstanceState
	if err := pool.QueryRow(ctx, `
		SELECT credentials.revoked_at, workers.state
		  FROM worker_instance_credentials AS credentials
		  JOIN worker_instances AS workers ON workers.id = credentials.worker_instance_id
		 WHERE credentials.id = $1
	`, credential.ID).Scan(&revokedAt, &workerState); err != nil {
		t.Fatal(err)
	}
	if !revokedAt.Valid || workerState != db.WorkerInstanceStateDisabled {
		t.Fatalf("policy change revoked=%v worker=%s, want true/disabled", revokedAt.Valid, workerState)
	}

	disabled, err := queries.DisableAbsentWorkerGroups(ctx, db.DisableAbsentWorkerGroupsParams{
		RegionID: dbtest.DefaultRegionID, DesiredIds: []string{dbtest.DefaultWorkerGroupID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 0 {
		t.Fatalf("disabled group before provider confirmation = %#v", disabled)
	}
	if _, err := queries.ClaimFleetWorkerTermination(ctx, db.ClaimFleetWorkerTerminationParams{
		WorkerInstanceID: credential.WorkerInstanceID, WorkerGroupID: groupID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ConfirmFleetWorkerProviderTermination(ctx, db.ConfirmFleetWorkerProviderTerminationParams{
		WorkerInstanceID: credential.WorkerInstanceID, WorkerGroupID: groupID, ResourceID: "i-policy-test",
	}); err != nil {
		t.Fatal(err)
	}
	disabled, err = queries.DisableAbsentWorkerGroups(ctx, db.DisableAbsentWorkerGroupsParams{
		RegionID: dbtest.DefaultRegionID, DesiredIds: []string{dbtest.DefaultWorkerGroupID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(disabled) != 1 || disabled[0].ID != groupID || disabled[0].State != db.WorkerGroupStateDisabled {
		t.Fatalf("disabled groups = %#v, want %s disabled", disabled, groupID)
	}
}

func TestWorkerGroupPolicyReconciliationSerializesAfterConcurrentEnrollment(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	q := db.New(pool)
	groupID := "policy-race-" + shortUUID(uuid.Must(uuid.NewV7()))
	if _, err := q.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		EnrollmentPolicyFingerprint: "sha256:policy-old", AllowsRun: true,
		ProtocolVersion:                auth.WorkerProtocolVersion,
		AllowedAttestationFingerprints: []string{"sha256:attestation-old"},
		RequiredCpuMillis:              1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredVmSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	nonce := []byte("policy-race-nonce")
	if _, err := q.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), NonceHash: nonce, WorkerGroupID: groupID,
		ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	enrollmentTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer enrollmentTx.Rollback(ctx)
	if _, err := enrollmentTx.Exec(ctx, `SELECT id FROM worker_groups WHERE id = $1 FOR UPDATE`, groupID); err != nil {
		t.Fatal(err)
	}
	reconciled := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		tx, err := pool.Begin(ctx)
		if err != nil {
			reconciled <- err
			return
		}
		defer tx.Rollback(ctx)
		qt := db.New(tx)
		close(started)
		if _, err = qt.LockWorkerGroupsForReconciliation(ctx, db.LockWorkerGroupsForReconciliationParams{
			RegionID: dbtest.DefaultRegionID, DesiredIds: []string{groupID},
		}); err == nil {
			_, err = qt.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
				ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
				EnrollmentPolicyFingerprint: "sha256:policy-new", AllowsRun: true,
				ProtocolVersion:                auth.WorkerProtocolVersion,
				AllowedAttestationFingerprints: []string{"sha256:attestation-new"},
				RequiredCpuMillis:              1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredVmSlots: 1,
			})
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		reconciled <- err
	}()
	<-started
	enrollment, err := db.New(enrollmentTx).EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
		NonceHash: nonce, WorkerGroupID: groupID, AllowsRun: true,
		ProtocolVersion: auth.WorkerProtocolVersion, EnrollmentPolicyFingerprint: "sha256:policy-old",
		WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: "i-policy-race",
		CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(), SecretHash: []byte("policy-race-secret"),
		AttestationFingerprint: "sha256:attestation-old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := enrollmentTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reconciled:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("policy reconciliation did not resume after enrollment commit")
	}
	var state db.WorkerInstanceState
	var revoked pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT worker_instances.state, worker_instance_credentials.revoked_at
		  FROM worker_instances
		  JOIN worker_instance_credentials ON worker_instance_credentials.worker_instance_id = worker_instances.id
		 WHERE worker_instances.id = $1 AND worker_instance_credentials.id = $2
	`, enrollment.WorkerInstanceID, enrollment.ID).Scan(&state, &revoked); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerInstanceStateDisabled || !revoked.Valid {
		t.Fatalf("concurrent enrollment survived policy change: state=%s revoked=%v", state, revoked.Valid)
	}
}

func TestTerminalWorkerCannotReuseItsDurableCredential(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	_ = seedIntegration(t, ctx, pool)
	nonceHash := []byte("terminal-worker-nonce")
	secretHash := []byte("terminal-worker-secret")
	if _, err := queries.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), NonceHash: nonceHash,
		WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	enrollment, err := queries.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
		NonceHash: nonceHash, WorkerGroupID: dbtest.DefaultWorkerGroupID,
		AllowsRun: true, AllowsBuild: true, ProtocolVersion: auth.WorkerProtocolVersion,
		WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: "i-terminal-test",
		CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(), SecretHash: secretHash,
		EnrollmentPolicyFingerprint: "sha256:test-worker-group", AttestationFingerprint: "sha256:test-attestation",
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := queries.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: enrollment.WorkerInstanceID, SecretHash: secretHash,
		ProtocolVersion: auth.WorkerProtocolVersion, ServiceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if err != nil {
		t.Fatal(err)
	}
	activateWorkspaceWorker(t, ctx, pool, pgvalue.MustUUIDValue(enrollment.WorkerInstanceID))
	if _, err := queries.DrainWorkerInstance(ctx, db.DrainWorkerInstanceParams{
		ID: enrollment.WorkerInstanceID, WorkerGroupID: dbtest.DefaultWorkerGroupID, ExpectedEpoch: first.CurrentEpoch,
	}); err != nil {
		t.Fatal(err)
	}
	second, err := queries.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: enrollment.WorkerInstanceID, SecretHash: secretHash,
		ProtocolVersion: auth.WorkerProtocolVersion, ServiceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.State != db.WorkerInstanceStateDraining || second.CurrentEpoch.Int64 <= first.CurrentEpoch.Int64 {
		t.Fatalf("draining restart state=%s epoch=%d, first epoch=%d", second.State, second.CurrentEpoch.Int64, first.CurrentEpoch.Int64)
	}
	fenced, err := queries.FenceWorkerInstance(ctx, db.FenceWorkerInstanceParams{
		ID: enrollment.WorkerInstanceID, WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ExpectedEpoch: second.CurrentEpoch, ReasonCode: pgtype.Text{String: "worker_retired", Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fenced.State != db.WorkerInstanceStateLost || fenced.ClaimVersion < 2 {
		t.Fatalf("fenced worker = %+v", fenced)
	}
	if _, err := queries.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: true, WorkerInstanceID: enrollment.WorkerInstanceID, SecretHash: secretHash,
		ProtocolVersion: auth.WorkerProtocolVersion, ServiceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("terminal authentication error = %v, want pgx.ErrNoRows", err)
	}
}

func TestDisableAbsentWorkerGroupsRefusesLiveOrFencedMembers(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	groupID := "retire-" + shortUUID(uuid.Must(uuid.NewV7()))
	if _, err := queries.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		EnrollmentPolicyFingerprint: "sha256:retire", AllowsRun: true,
		ProtocolVersion:                auth.WorkerProtocolVersion,
		AllowedAttestationFingerprints: []string{"sha256:retire"},
		RequiredCpuMillis:              1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredVmSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	workerID := uuid.Must(uuid.NewV7())
	resourceID := "resource-" + workerID.String()
	if _, err := pool.Exec(ctx, `
		INSERT INTO worker_instances (id, resource_id, worker_group_id, attestation_fingerprint, state)
		VALUES ($1, $2, $3, 'sha256:retire', 'registering')
	`, workerID, resourceID, groupID); err != nil {
		t.Fatal(err)
	}
	desired := []string{dbtest.DefaultWorkerGroupID}
	disabled, err := queries.DisableAbsentWorkerGroups(ctx, db.DisableAbsentWorkerGroupsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range disabled {
		if group.ID == groupID {
			t.Fatal("group with a live member was disabled")
		}
	}
	live, err := queries.ListLiveAbsentWorkerGroupIDs(ctx, db.ListLiveAbsentWorkerGroupIDsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0] != groupID {
		t.Fatalf("live absent groups = %#v, want %q", live, groupID)
	}
	mustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET state = 'disabled', disabled_at = now(),
		       drain_cleanup_fingerprint = $2,
		       drain_cleanup_evidence = '{"inventory_empty":true}'::jsonb
		 WHERE id = $1
	`, workerID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := queries.ClaimFleetWorkerTermination(ctx, db.ClaimFleetWorkerTerminationParams{
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: groupID,
	}); err != nil {
		t.Fatal(err)
	}
	disabled, err = queries.DisableAbsentWorkerGroups(ctx, db.DisableAbsentWorkerGroupsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired})
	if err != nil {
		t.Fatal(err)
	}
	for _, group := range disabled {
		if group.ID == groupID {
			t.Fatal("group was disabled before provider termination confirmation")
		}
	}
	if _, err := queries.ConfirmFleetWorkerProviderTermination(ctx, db.ConfirmFleetWorkerProviderTerminationParams{
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: groupID, ResourceID: resourceID,
	}); err != nil {
		t.Fatal(err)
	}
	disabled, err = queries.DisableAbsentWorkerGroups(ctx, db.DisableAbsentWorkerGroupsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, group := range disabled {
		found = found || group.ID == groupID
	}
	if !found {
		t.Fatal("provider-terminated historical worker still blocked group removal")
	}
}

func TestAbsentWorkerGroupRemovalSerializesWithEnrollment(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	desired := []string{dbtest.DefaultWorkerGroupID}
	createGroupAndNonce := func(suffix string) (string, []byte) {
		t.Helper()
		groupID := "remove-race-" + suffix + "-" + shortUUID(uuid.Must(uuid.NewV7()))
		q := db.New(pool)
		if _, err := q.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
			ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
			EnrollmentPolicyFingerprint: "sha256:" + suffix, AllowsRun: true,
			ProtocolVersion:                auth.WorkerProtocolVersion,
			AllowedAttestationFingerprints: []string{"sha256:" + suffix},
			RequiredCpuMillis:              1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredVmSlots: 1,
		}); err != nil {
			t.Fatal(err)
		}
		nonce := []byte("remove-race-" + suffix)
		if _, err := q.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), NonceHash: nonce, WorkerGroupID: groupID,
			ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
		}); err != nil {
			t.Fatal(err)
		}
		return groupID, nonce
	}
	enroll := func(q *db.Queries, groupID string, nonce []byte) error {
		_, err := q.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
			NonceHash: nonce, WorkerGroupID: groupID, AllowsRun: true,
			ProtocolVersion:  auth.WorkerProtocolVersion,
			WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: "i-" + groupID,
			CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(),
			SecretHash: []byte("secret-" + groupID), EnrollmentPolicyFingerprint: "sha256:" + strings.Split(groupID, "-")[2],
			AttestationFingerprint: "sha256:" + strings.Split(groupID, "-")[2],
		})
		return err
	}

	t.Run("removal lock wins", func(t *testing.T) {
		groupID, nonce := createGroupAndNonce("remove")
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		qtx := db.New(tx)
		if _, err := qtx.LockAbsentWorkerGroups(ctx, db.LockAbsentWorkerGroupsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired}); err != nil {
			t.Fatal(err)
		}
		enrollResult := make(chan error, 1)
		go func() { enrollResult <- enroll(db.New(pool), groupID, nonce) }()
		if _, err := qtx.DisableAbsentWorkerGroups(ctx, db.DisableAbsentWorkerGroupsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-enrollResult:
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("enrollment after removal error = %v, want pgx.ErrNoRows", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("enrollment did not resume after group removal commit")
		}
	})

	t.Run("enrollment lock wins", func(t *testing.T) {
		groupID, nonce := createGroupAndNonce("enroll")
		enrollTx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer enrollTx.Rollback(ctx)
		if _, err := enrollTx.Exec(ctx, `SELECT id FROM worker_groups WHERE id = $1 FOR UPDATE`, groupID); err != nil {
			t.Fatal(err)
		}
		type removalResult struct {
			disabled int
			live     []string
			err      error
		}
		result := make(chan removalResult, 1)
		started := make(chan struct{})
		go func() {
			tx, err := pool.Begin(ctx)
			if err != nil {
				result <- removalResult{err: err}
				return
			}
			defer tx.Rollback(ctx)
			q := db.New(tx)
			close(started)
			if _, err = q.LockAbsentWorkerGroups(ctx, db.LockAbsentWorkerGroupsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired}); err != nil {
				result <- removalResult{err: err}
				return
			}
			disabled, err := q.DisableAbsentWorkerGroups(ctx, db.DisableAbsentWorkerGroupsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired})
			if err != nil {
				result <- removalResult{err: err}
				return
			}
			live, err := q.ListLiveAbsentWorkerGroupIDs(ctx, db.ListLiveAbsentWorkerGroupIDsParams{RegionID: dbtest.DefaultRegionID, DesiredIds: desired})
			if err == nil {
				err = tx.Commit(ctx)
			}
			result <- removalResult{disabled: len(disabled), live: live, err: err}
		}()
		<-started
		if err := enroll(db.New(enrollTx), groupID, nonce); err != nil {
			t.Fatal(err)
		}
		if err := enrollTx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-result:
			if got.err != nil || got.disabled != 0 || len(got.live) != 1 || got.live[0] != groupID {
				t.Fatalf("removal after enrollment = %+v", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("group removal did not resume after enrollment commit")
		}
	})
}

func TestWorkerGroupRoleNarrowingFencesExistingCapabilities(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	q := db.New(pool)
	groupID := "role-narrow-" + shortUUID(uuid.Must(uuid.NewV7()))
	attestation := "sha256:role-narrow"
	if _, err := q.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		EnrollmentPolicyFingerprint: "sha256:roles-both", AllowsRun: true, AllowsBuild: true,
		ProtocolVersion: auth.WorkerProtocolVersion, AllowedAttestationFingerprints: []string{attestation},
		RequiredCpuMillis: 1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredVmSlots: 1, RequiredBuildExecutors: 1,
	}); err != nil {
		t.Fatal(err)
	}
	nonce := []byte("role-narrow-nonce")
	if _, err := q.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), NonceHash: nonce, WorkerGroupID: groupID,
		ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	enrolled, err := q.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
		NonceHash: nonce, WorkerGroupID: groupID, AllowsRun: true, AllowsBuild: true,
		ProtocolVersion: auth.WorkerProtocolVersion, WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ResourceID: "role-narrow-worker", CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		KeyPrefix: uuid.NewString(), SecretHash: []byte("role-narrow-secret"),
		EnrollmentPolicyFingerprint: "sha256:roles-both", AttestationFingerprint: attestation,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		EnrollmentPolicyFingerprint: "sha256:roles-run", AllowsRun: true, AllowsBuild: false,
		ProtocolVersion: auth.WorkerProtocolVersion, AllowedAttestationFingerprints: []string{attestation},
		RequiredCpuMillis: 1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredVmSlots: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var state db.WorkerInstanceState
	var revoked pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `
		SELECT worker_instances.state, worker_instance_credentials.revoked_at
		  FROM worker_instances
		  JOIN worker_instance_credentials ON worker_instance_credentials.worker_instance_id = worker_instances.id
		 WHERE worker_instances.id = $1 AND worker_instance_credentials.id = $2
	`, enrolled.WorkerInstanceID, enrolled.ID).Scan(&state, &revoked); err != nil {
		t.Fatal(err)
	}
	if state != db.WorkerInstanceStateDisabled || !revoked.Valid {
		t.Fatalf("narrowed worker state=%s revoked=%v", state, revoked.Valid)
	}
}

func TestBuildOnlyWorkerCertificationRetainsRuntimeContract(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	queries := db.New(pool)
	const workerGroupID = "test-build-workers"
	if _, err := queries.ReconcileWorkerGroup(ctx, db.ReconcileWorkerGroupParams{
		ID: workerGroupID, RegionID: dbtest.DefaultRegionID, Name: "build",
		Description: "build workers", AllowsRun: false, AllowsBuild: true,
		ProtocolVersion: auth.WorkerProtocolVersion, EnrollmentPolicyFingerprint: "sha256:test-build-policy",
		AllowedAttestationFingerprints: []string{"sha256:test-build-attestation"},
		RequiredCpuMillis:              1, RequiredMemoryBytes: 1, RequiredWorkloadDiskBytes: 1, RequiredScratchBytes: 1, RequiredBuildExecutors: 1,
	}); err != nil {
		t.Fatal(err)
	}
	nonceHash := []byte("build-worker-nonce")
	secretHash := []byte("secret")
	if _, err := queries.CreateWorkerEnrollmentNonce(ctx, db.CreateWorkerEnrollmentNonceParams{
		ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), NonceHash: nonceHash,
		WorkerGroupID: workerGroupID,
		ExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	enrollment, err := queries.EnrollWorkerInstance(ctx, db.EnrollWorkerInstanceParams{
		NonceHash: nonceHash, WorkerGroupID: workerGroupID,
		AllowsRun: false, AllowsBuild: true, ProtocolVersion: auth.WorkerProtocolVersion,
		WorkerInstanceID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ResourceID: "i-build-only",
		CredentialID: pgvalue.UUID(uuid.Must(uuid.NewV7())), KeyPrefix: uuid.NewString(), SecretHash: secretHash,
		EnrollmentPolicyFingerprint: "sha256:test-build-policy", AttestationFingerprint: "sha256:test-build-attestation",
	})
	if err != nil {
		t.Fatal(err)
	}
	authenticated, err := queries.AuthenticateWorkerInstanceCredential(ctx, db.AuthenticateWorkerInstanceCredentialParams{
		SupportsRun: false, SupportsBuild: true, WorkerInstanceID: enrollment.WorkerInstanceID,
		SecretHash: secretHash, ProtocolVersion: auth.WorkerProtocolVersion,
		ServiceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	})
	if err != nil {
		t.Fatal(err)
	}
	const rootfsDigest = "sha256:build-worker-rootfs"
	const runtimeABI = "helmr.firecracker.snapshot.v0"
	certified, err := queries.CertifyWorkerInstance(ctx, db.CertifyWorkerInstanceParams{
		RuntimeIdentityID: "sha256:build-worker-runtime", RuntimeArch: "amd64", RuntimeABI: runtimeABI,
		KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: rootfsDigest,
		CniProfile: "helmr/v0", WorkerInstanceID: enrollment.WorkerInstanceID,
		WorkerGroupID: workerGroupID, WorkerEpoch: authenticated.CurrentEpoch,
		SupportsRun: false, SupportsBuild: true, MaxVmSlots: 0,
		ProtocolVersion: auth.WorkerProtocolVersion, SupervisorVersion: "test",
		CertifiedCpuMillis: 4000, CertifiedMemoryBytes: 8 << 30,
		CertifiedWorkloadDiskBytes: 64 << 30, CertifiedScratchBytes: 16 << 30,
		CertifiedBuildCacheBytes: 8 << 30, CertifiedArtifactCacheBytes: 4 << 30,
		PerVmCpuMillis: 2000, PerVmMemoryBytes: 4 << 30, PerVmWorkloadDiskBytes: 32 << 30,
		PerVmScratchBytes: 8 << 30, MaxBuildExecutors: 1, MaxRuntimeStarts: 1,
		CertificationProfile: "test", CertificationFingerprint: "sha256:certification",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !certified.RuntimeIdentityID.Valid || certified.RuntimeIdentityID.String != "sha256:build-worker-runtime" {
		t.Fatalf("certified runtime identity = %#v", certified.RuntimeIdentityID)
	}
	state, err := queries.GetWorkerInstanceState(ctx, db.GetWorkerInstanceStateParams{
		ID: enrollment.WorkerInstanceID, WorkerGroupID: workerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !state.RootfsDigest.Valid || state.RootfsDigest.String != rootfsDigest || !state.RuntimeABI.Valid || state.RuntimeABI.String != runtimeABI {
		t.Fatalf("build runtime contract rootfs=%#v abi=%#v", state.RootfsDigest, state.RuntimeABI)
	}
	if _, err := queries.RenewWorkerCertification(ctx, db.RenewWorkerCertificationParams{
		WorkerInstanceID: enrollment.WorkerInstanceID, WorkerGroupID: workerGroupID,
		WorkerEpoch: authenticated.CurrentEpoch, SupportsRun: false,
		RuntimeIdentityID: "sha256:build-worker-runtime", ProtocolVersion: auth.WorkerProtocolVersion,
		SupportsBuild: true, CertifiedCpuMillis: 4000, CertifiedMemoryBytes: 8 << 30,
		CertifiedWorkloadDiskBytes: 64 << 30, CertifiedScratchBytes: 16 << 30,
		CertifiedBuildCacheBytes: 8 << 30, CertifiedArtifactCacheBytes: 4 << 30,
		PerVmCpuMillis: 2000, PerVmMemoryBytes: 4 << 30, PerVmWorkloadDiskBytes: 32 << 30,
		PerVmScratchBytes: 8 << 30, MaxVmSlots: 0, MaxBuildExecutors: 1, MaxRuntimeStarts: 1,
	}); err != nil {
		t.Fatalf("renew build-only certification: %v", err)
	}
}
