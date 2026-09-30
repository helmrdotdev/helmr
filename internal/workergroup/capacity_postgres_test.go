package workergroup

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
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
	reconciled, err := ReconcilePrimaryPools(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, f.currentGroup(t).ClaimVersion)
	if err != nil || !reconciled.Applied || reconciled.WorkerGroup.PrimaryPoolID != resolved.ID {
		t.Fatalf("ReconcilePrimaryPools = %+v, %v", reconciled, err)
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
	if _, err := DrainHost(t.Context(), f.q, first, DrainWorkerHostRequest{ExpectedEpoch: 0, ExpectedClaimVersion: 1}); !errors.As(err, &input) {
		t.Fatalf("zero epoch error = %v, want InputError", err)
	}
	var conflicting ConflictError
	if _, err := DrainHost(t.Context(), f.q, first, DrainWorkerHostRequest{ExpectedEpoch: 2, ExpectedClaimVersion: host.ClaimVersion}); !errors.As(err, &conflicting) {
		t.Fatalf("stale epoch drain error = %v, want ConflictError", err)
	}
	drained, err := DrainHost(t.Context(), f.q, first, DrainWorkerHostRequest{ExpectedEpoch: 1, ExpectedClaimVersion: host.ClaimVersion, RequireZeroQueuedDemand: true})
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
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status = 'termination_ready', draining_at = now(), termination_ready_at = now() WHERE id = $1`, second)
	if _, err := ConfirmHostProviderAbsent(t.Context(), f.q, f.pool, second); !errors.As(err, &conflicting) {
		t.Fatalf("termination-ready absence error = %v, want ConflictError", err)
	}
}

// queuedRunQuerier reports one eligible queued Run in every region.
type queuedRunQuerier struct {
	db.Querier
}

func (queuedRunQuerier) ListQueuedRunEligibleScopes(context.Context, db.ListQueuedRunEligibleScopesParams) ([]db.ListQueuedRunEligibleScopesRow, error) {
	return []db.ListQueuedRunEligibleScopesRow{{}}, nil
}

func TestDrainHostPostgresRejectsQueuedDemand(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "run-current")
	hostID := f.activeHost(t, pool, "host-1")
	host, err := GetHost(t.Context(), f.q, hostID)
	if err != nil {
		t.Fatal(err)
	}
	request := DrainWorkerHostRequest{ExpectedEpoch: 1, ExpectedClaimVersion: host.ClaimVersion, RequireZeroQueuedDemand: true}
	if _, err := DrainHost(t.Context(), queuedRunQuerier{f.q}, hostID, request); !errors.Is(err, ErrQueuedDemand) {
		t.Fatalf("drain with queued demand error = %v, want ErrQueuedDemand", err)
	}
	if unchanged, err := GetHost(t.Context(), f.q, hostID); err != nil || unchanged.Status != WorkerHostStatusActive || unchanged.ClaimVersion != host.ClaimVersion {
		t.Fatalf("host after rejected drain = %+v, %v", unchanged, err)
	}
	request.RequireZeroQueuedDemand = false
	drained, err := DrainHost(t.Context(), queuedRunQuerier{f.q}, hostID, request)
	if err != nil || drained.Status != WorkerHostStatusDraining {
		t.Fatalf("drain without the demand requirement = %+v, %v", drained, err)
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
