package workergroup

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func TestCapacityResolvePostgres(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "run-current")
	group, err := ResolveGroup(t.Context(), f.q, fixtureRegionID, "supply-test")
	if err != nil || group.ID != f.groupID().String() || group.Status != WorkerGroupStatusActive || group.PrimaryPoolID != "" {
		t.Fatalf("ResolveGroup = %+v, %v", group, err)
	}
	resolved, err := ResolvePool(t.Context(), f.q, f.groupID(), "run-current")
	if err != nil || resolved.ID != uuid.UUID(pool.ID.Bytes).String() || resolved.Status != WorkerPoolStatusActive {
		t.Fatalf("ResolvePool = %+v, %v", resolved, err)
	}
	if _, err := ResolveGroup(t.Context(), f.q, fixtureRegionID, "missing"); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("missing group error = %v", err)
	}
	if _, err := ResolvePool(t.Context(), f.q, f.groupID(), "missing"); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("missing pool error = %v", err)
	}
	f.activeHost(t, pool, "ready-primary")
	reconciled, err := SelectPrimary(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, f.currentGroup(t).ClaimVersion, 1)
	if err != nil || !reconciled.Applied || reconciled.WorkerGroup.PrimaryPoolID != resolved.ID {
		t.Fatalf("SelectPrimary = %+v, %v", reconciled, err)
	}
}

func TestCapacityHostsPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "run-current")
	first := f.activeHost(t, pool, "host-1")
	second := f.activeHost(t, pool, "host-2")

	listed, err := ListHosts(t.Context(), f.q, HostFilter{GroupID: f.groupID(), ResourceIDs: []string{"host-2"}, Statuses: []WorkerHostStatus{WorkerHostStatusActive}, Limit: 10})
	if err != nil || len(listed.WorkerHosts) != 1 || listed.WorkerHosts[0].ID != second.String() {
		t.Fatalf("ListHosts = %+v, %v", listed, err)
	}
	all, err := ListHosts(t.Context(), f.q, HostFilter{Limit: 10})
	if err != nil || len(all.WorkerHosts) != 2 {
		t.Fatalf("ListHosts unfiltered = %+v, %v", all, err)
	}
	host, err := GetHost(t.Context(), f.q, first)
	if err != nil || host.ResourceID != "host-1" || host.Status != WorkerHostStatusActive || host.CurrentEpoch == nil || *host.CurrentEpoch != 1 {
		t.Fatalf("GetHost = %+v, %v", host, err)
	}
	if _, err := GetHost(t.Context(), f.q, uuid.NewV7()); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("missing host error = %v", err)
	}

	var input InputError
	if _, err := DrainHost(t.Context(), f.pool, first, DrainWorkerHostRequest{ExpectedEpoch: 0, ExpectedClaimVersion: 1}); !errors.As(err, &input) {
		t.Fatalf("zero epoch error = %v, want InputError", err)
	}
	var conflicting ConflictError
	if _, err := DrainHost(t.Context(), f.pool, first, DrainWorkerHostRequest{ExpectedEpoch: 2, ExpectedClaimVersion: host.ClaimVersion, Reason: DrainReasonReplacement}); !errors.As(err, &conflicting) {
		t.Fatalf("stale epoch drain error = %v, want ConflictError", err)
	}
	drained, err := DrainHost(t.Context(), f.pool, first, DrainWorkerHostRequest{ExpectedEpoch: 1, ExpectedClaimVersion: host.ClaimVersion, Reason: DrainReasonIdleScaleIn})
	if err != nil || drained.Status != WorkerHostStatusDraining || drained.DrainingAt == nil || drained.ClaimVersion != host.ClaimVersion+1 {
		t.Fatalf("DrainHost = %+v, %v", drained, err)
	}

	lost, err := ConfirmHostProviderAbsent(t.Context(), f.q, f.pool, first)
	if err != nil || lost.Status != WorkerHostStatusLost || lost.LostAt == nil {
		t.Fatalf("ConfirmHostProviderAbsent = %+v, %v", lost, err)
	}
	if _, err := ConfirmHostProviderAbsent(t.Context(), f.q, f.pool, uuid.NewV7()); !errors.Is(err, ErrHostNotFound) {
		t.Fatalf("missing host absence error = %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status = 'termination_ready', draining_at = now(), drain_reason = 'shutdown', termination_ready_at = now() WHERE id = $1`, second)
	if _, err := ConfirmHostProviderAbsent(t.Context(), f.q, f.pool, second); !errors.As(err, &conflicting) {
		t.Fatalf("termination-ready absence error = %v, want ConflictError", err)
	}
}

func TestDrainHostPostgresRejectsQueuedDemand(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	claimID, commandID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at,receipt_expires_at) VALUES($1,$2,'computer.command.create',$3,$4,now(),now()+interval '30 days')`, claimID, f.EnvironmentID, dbtest.Hash(commandID.String()), dbtest.Hash("drain-command"))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,cwd,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id) VALUES($1,$2,$3,$4,ARRAY['true'],'/workspace','{}','',300000,'api_key','fixture')`, commandID, f.EnvironmentID, computerID, claimID)
	q := db.New(f.Pool)
	host, err := GetHost(t.Context(), q, f.WorkerID)
	if err != nil {
		t.Fatal(err)
	}
	request := DrainWorkerHostRequest{ExpectedEpoch: 1, ExpectedClaimVersion: host.ClaimVersion, Reason: DrainReasonIdleScaleIn}
	if _, err := DrainHost(t.Context(), f.Pool, f.WorkerID, request); !errors.Is(err, ErrQueuedDemand) {
		t.Fatalf("idle drain with queued demand error = %v, want ErrQueuedDemand", err)
	}
	if unchanged, err := GetHost(t.Context(), q, f.WorkerID); err != nil || unchanged.Status != WorkerHostStatusActive || unchanged.ClaimVersion != host.ClaimVersion {
		t.Fatalf("host after rejected drain = %+v, %v", unchanged, err)
	}
	request.Reason = DrainReasonReplacement
	drained, err := DrainHost(t.Context(), f.Pool, f.WorkerID, request)
	if err != nil || drained.Status != WorkerHostStatusDraining || drained.DrainReason != "replacement" {
		t.Fatalf("replacement drain = %+v, %v", drained, err)
	}
	request.Reason = DrainReasonCapacityReduction
	replay, err := DrainHost(t.Context(), f.Pool, f.WorkerID, request)
	if err != nil || replay.DrainReason != "replacement" || replay.ClaimVersion != drained.ClaimVersion || !replay.DrainingAt.Equal(*drained.DrainingAt) {
		t.Fatalf("drain replay = %+v, %v", replay, err)
	}
}

func TestListHostsPostgresAppliesLimit(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "run-current")
	f.activeHost(t, pool, "host-1")
	f.activeHost(t, pool, "host-2")
	f.activeHost(t, pool, "host-3")
	for _, limit := range []int32{1, 2, 3} {
		listed, err := ListHosts(t.Context(), f.q, HostFilter{GroupID: f.groupID(), Limit: limit})
		if err != nil || len(listed.WorkerHosts) != int(limit) {
			t.Fatalf("ListHosts(limit %d) = %d hosts, %v", limit, len(listed.WorkerHosts), err)
		}
	}
}
