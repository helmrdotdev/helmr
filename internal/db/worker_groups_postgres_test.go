package db_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestWorkerGroupObservationTTLIsPositiveClaimAuthority(t *testing.T) {
	ctx := context.Background()
	q := db.New(newPostgresDB(t, ctx))
	groupID := "ttl-" + shortUUID(uuid.Must(uuid.NewV7()))
	params := db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		ObservationTtlSeconds:       120,
		EnrollmentPolicyFingerprint: "sha256:ttl-policy",
		AllowsBuild:                 true, ProtocolVersion: auth.WorkerProtocolVersion,
		AllowedAttestationFingerprints: []string{"sha256:test-attestation"},
		RequiredCpuMillis:              1, RequiredMemoryBytes: 1,
		RequiredGuestEphemeralDiskBytes: 1, RequiredBuildExecutors: 1,
	}
	first, err := q.ReconcileWorkerGroup(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := q.ReconcileWorkerGroup(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ClaimVersion != first.ClaimVersion {
		t.Fatalf("unchanged TTL advanced claim version: first=%d replay=%d", first.ClaimVersion, replayed.ClaimVersion)
	}
	params.ObservationTtlSeconds = 60
	changed, err := q.ReconcileWorkerGroup(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ClaimVersion != first.ClaimVersion+1 {
		t.Fatalf("changed TTL claim version=%d, want %d", changed.ClaimVersion, first.ClaimVersion+1)
	}
	params.ObservationTtlSeconds = 0
	if _, err := q.ReconcileWorkerGroup(ctx, params); err == nil {
		t.Fatal("zero observation TTL unexpectedly reconciled")
	}
}

func TestWorkerGroupReconcileDoesNotReactivateDrainingGroup(t *testing.T) {
	ctx := context.Background()
	pool := newPostgresDB(t, ctx)
	q := db.New(pool)
	groupID := "drift-" + shortUUID(uuid.Must(uuid.NewV7()))
	params := db.ReconcileWorkerGroupParams{
		ID: groupID, RegionID: dbtest.DefaultRegionID, Name: groupID,
		ObservationTtlSeconds:       120,
		EnrollmentPolicyFingerprint: "sha256:drift-policy",
		AllowsBuild:                 true, ProtocolVersion: auth.WorkerProtocolVersion,
		AllowedAttestationFingerprints: []string{"sha256:test-attestation"},
		RequiredCpuMillis:              1, RequiredMemoryBytes: 1,
		RequiredGuestEphemeralDiskBytes: 1, RequiredBuildExecutors: 1,
	}
	if _, err := q.ReconcileWorkerGroup(ctx, params); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE worker_groups
		   SET state = 'draining', updated_at = now()
		 WHERE id = $1
	`, groupID); err != nil {
		t.Fatal(err)
	}
	reconciled, err := q.ReconcileWorkerGroup(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.State != db.WorkerGroupStateDraining {
		t.Fatalf("reconciled state = %s, want draining", reconciled.State)
	}
}
