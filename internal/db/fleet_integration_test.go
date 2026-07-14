package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestFleetRunAttestationCoverageTracksCurrentLaunchAMI(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	q := db.New(pool)
	mustExec(t, ctx, pool, `
		UPDATE worker_groups
		   SET launch_attestation_fingerprint = 'sha256:test-attestation'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID)

	count, err := q.CountUncertifiedRunLaunchAttestations(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("fresh coverage gaps = %d, want 1", count)
	}

	mustExec(t, ctx, pool, `
		INSERT INTO runtime_identities (
			id, runtime_arch, runtime_abi, kernel_digest, initramfs_digest, rootfs_digest, cni_profile
		) VALUES ('fleet-runtime', 'x86_64', 'fleet-test', 'sha256:kernel', 'sha256:initramfs', 'sha256:rootfs', 'default')
	`)
	oldWorkerID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			supports_run, runtime_identity_id, certified_at, disabled_at
		) VALUES ($1, $2, $3, 'sha256:test-attestation', 'disabled', true, 'fleet-runtime', now(), now())
	`, oldWorkerID, "i-"+oldWorkerID.String(), dbtest.DefaultWorkerGroupID)
	count, err = q.CountUncertifiedRunLaunchAttestations(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil || count != 0 {
		t.Fatalf("certified coverage gaps = %d error=%v, want 0", count, err)
	}

	mustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET termination_claimed_at = now(), provider_terminated_at = now()
		 WHERE id = $1
	`, oldWorkerID)
	mustExec(t, ctx, pool, `
		UPDATE worker_groups
		   SET allowed_attestation_fingerprints = ARRAY['sha256:test-attestation', 'sha256:new-ami'],
		       launch_attestation_fingerprint = 'sha256:new-ami'
		 WHERE id = $1
	`, dbtest.DefaultWorkerGroupID)
	count, err = q.CountUncertifiedRunLaunchAttestations(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil || count != 1 {
		t.Fatalf("launch AMI coverage gaps = %d error=%v, want new launch AMI uncovered", count, err)
	}

	buildWorkerID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			supports_build, runtime_identity_id, certified_at, disabled_at
		) VALUES ($1, $2, $3, 'sha256:new-ami', 'disabled', true, 'fleet-runtime', now(), now())
	`, buildWorkerID, "i-"+buildWorkerID.String(), dbtest.DefaultWorkerGroupID)
	count, err = q.CountUncertifiedRunLaunchAttestations(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil || count != 1 {
		t.Fatalf("build-only coverage gaps = %d error=%v, want new run AMI uncovered", count, err)
	}

	runWorkerID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			supports_run, runtime_identity_id, certified_at, disabled_at
		) VALUES ($1, $2, $3, 'sha256:new-ami', 'disabled', true, 'fleet-runtime', now(), now())
	`, runWorkerID, "i-"+runWorkerID.String(), dbtest.DefaultWorkerGroupID)
	count, err = q.CountUncertifiedRunLaunchAttestations(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil || count != 0 {
		t.Fatalf("launch AMI coverage gaps = %d error=%v, want 0", count, err)
	}
}

func TestFleetTerminationProofFailsClosedWithoutDurableCleanupEvidence(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	workerID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint,
			state, disabled_at
		) VALUES ($1, $2, $3, 'sha256:fleet-proof', 'disabled', now())
	`, workerID, "i-"+workerID.String(), dbtest.DefaultWorkerGroupID)

	proof, err := db.New(pool).GetFleetTerminationProof(ctx, db.GetFleetTerminationProofParams{
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proof.AuthorityCount != 0 || proof.LocalCleanupComplete {
		t.Fatalf("proof = %#v, want zero authority but cleanup evidence fail-closed", proof)
	}
	mustExec(t, ctx, pool, `
		UPDATE worker_instances
		   SET drain_cleanup_fingerprint = $2, drain_cleanup_evidence = '{"inventory_empty":true}'::jsonb
		 WHERE id = $1
	`, workerID, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	proof, err = db.New(pool).GetFleetTerminationProof(ctx, db.GetFleetTerminationProofParams{
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if proof.AuthorityCount != 0 || !proof.LocalCleanupComplete {
		t.Fatalf("proof = %#v, want durable disabled cleanup proof", proof)
	}
}

func TestFleetCooldownIsDurableAndMonotonic(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	q := db.New(pool)
	first := time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)
	later := first.Add(time.Minute)
	initial, err := q.GetFleetCooldown(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	if initial.LastScaleOutAt.Valid || initial.LastScaleInAt.Valid {
		t.Fatalf("initial cooldown = %#v, want null timestamps", initial)
	}
	if _, err := q.RecordFleetScaleOut(ctx, db.RecordFleetScaleOutParams{WorkerGroupID: "missing", ActionAt: pgvalue.Timestamptz(first)}); err == nil {
		t.Fatal("record cooldown for missing worker group succeeded")
	}
	if _, err := q.RecordFleetScaleOut(ctx, db.RecordFleetScaleOutParams{WorkerGroupID: dbtest.DefaultWorkerGroupID, ActionAt: pgvalue.Timestamptz(first)}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RecordFleetScaleOut(ctx, db.RecordFleetScaleOutParams{WorkerGroupID: dbtest.DefaultWorkerGroupID, ActionAt: pgvalue.Timestamptz(first.Add(-time.Minute))}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RecordFleetScaleIn(ctx, db.RecordFleetScaleInParams{WorkerGroupID: dbtest.DefaultWorkerGroupID, ActionAt: pgvalue.Timestamptz(later)}); err != nil {
		t.Fatal(err)
	}
	row, err := q.GetFleetCooldown(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	if !row.LastScaleOutAt.Valid || !row.LastScaleOutAt.Time.Equal(first) || !row.LastScaleInAt.Valid || !row.LastScaleInAt.Time.Equal(later) {
		t.Fatalf("cooldown = %#v", row)
	}
}

func TestFleetTerminationProofAllowsFencedLostWorkerOnlyAfterCredentialRevocation(t *testing.T) {
	ctx := context.Background()
	pool := newIntegrationDB(t, ctx)
	workerID := uuid.Must(uuid.NewV7())
	credentialID := uuid.Must(uuid.NewV7())
	serviceID := uuid.Must(uuid.NewV7())
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instances (
			id, resource_id, worker_group_id, attestation_fingerprint, state,
			current_epoch, current_service_id, epoch_started_at, lost_at
		) VALUES ($1, $2, $3, 'sha256:fleet-lost-proof', 'lost', 1, $4, now(), now())
	`, workerID, "i-"+workerID.String(), dbtest.DefaultWorkerGroupID, serviceID)
	mustExec(t, ctx, pool, `
		INSERT INTO worker_instance_credentials (
			id, worker_group_id, worker_instance_id, key_prefix, claim_version,
			allows_run, allows_build, protocol_version, secret_hash
		) VALUES ($1, $2, $3, $4, 1, true, false, 'helmr.worker.v0', $5)
	`, credentialID, dbtest.DefaultWorkerGroupID, workerID, "lost-"+credentialID.String(), []byte("lost-secret"))
	q := db.New(pool)
	params := db.GetFleetTerminationProofParams{WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID}
	proof, err := q.GetFleetTerminationProof(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if proof.FencedForTermination {
		t.Fatal("lost worker with an active credential was termination eligible")
	}
	mustExec(t, ctx, pool, `UPDATE worker_instance_credentials SET revoked_at = now() WHERE id = $1`, credentialID)
	proof, err = q.GetFleetTerminationProof(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.FencedForTermination || proof.AuthorityCount != 0 || proof.State != db.WorkerInstanceStateLost || proof.CurrentEpoch != (pgtype.Int8{Int64: 1, Valid: true}) {
		t.Fatalf("proof = %#v, want fenced lost zero-authority proof", proof)
	}
	claimed, err := q.ClaimFleetWorkerTermination(ctx, db.ClaimFleetWorkerTerminationParams{
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !claimed.FencedForTermination || claimed.ResourceID != "i-"+workerID.String() {
		t.Fatalf("termination claim = %#v", claimed)
	}
	if _, err := q.ConfirmFleetWorkerProviderTermination(ctx, db.ConfirmFleetWorkerProviderTerminationParams{
		WorkerInstanceID: pgvalue.UUID(workerID), WorkerGroupID: dbtest.DefaultWorkerGroupID,
		ResourceID: "i-" + workerID.String(),
	}); err != nil {
		t.Fatal(err)
	}
	workers, err := q.ListFleetWorkers(ctx, dbtest.DefaultWorkerGroupID)
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range workers {
		if worker.ID == pgvalue.UUID(workerID) {
			t.Fatal("provider-terminated worker remained billable fleet supply")
		}
	}
}
