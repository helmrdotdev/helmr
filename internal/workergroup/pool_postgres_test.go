package workergroup

import (
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestCreatePoolPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	group, pending, err := CreatePool(t.Context(), f.pool, f.groupID(), "run-next", f.group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != "pending" || pending.Name != "run-next" || group.ID != f.group.ID {
		t.Fatalf("created pool = %+v in group %+v", pending, group)
	}
	var conflicting ConflictError
	if _, _, err := CreatePool(t.Context(), f.pool, f.groupID(), "run-next", f.group.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("duplicate pool error = %v, want ConflictError", err)
	}
	if _, _, err := CreatePool(t.Context(), f.pool, f.groupID(), "stale", f.group.ClaimVersion+1); !errors.As(err, &conflicting) {
		t.Fatalf("stale group claim error = %v, want ConflictError", err)
	}
	if _, _, err := CreatePool(t.Context(), f.pool, uuid.NewV7(), "missing", 1); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("missing group error = %v", err)
	}
	var input InputError
	for _, bad := range []struct {
		name    string
		version int64
	}{{"Upper", 1}, {"valid", 0}} {
		if _, _, err := CreatePool(t.Context(), f.pool, f.groupID(), bad.name, bad.version); !errors.As(err, &input) {
			t.Fatalf("CreatePool(%q, %d) error = %v, want InputError", bad.name, bad.version, err)
		}
	}
	_, pools, err := ListPools(t.Context(), f.q, f.groupID())
	if err != nil || len(pools) != 1 || pools[0].ID != pending.ID {
		t.Fatalf("ListPools = %+v, %v", pools, err)
	}
	if _, _, err := ListPools(t.Context(), f.q, uuid.NewV7()); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("ListPools missing group error = %v", err)
	}
}

func TestSelectPrimaryPoolPostgresIsAtomicAndReplaySafe(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "current")
	group := f.currentGroup(t)
	selection, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, group.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.Applied || selection.Group.ClaimVersion != group.ClaimVersion+1 ||
		selection.Group.PrimaryPoolID != pool.ID || selection.Pool.ID != pool.ID {
		t.Fatalf("selection = %+v", selection)
	}
	stored := f.currentGroup(t)
	if stored.ClaimVersion != selection.Group.ClaimVersion || stored.PrimaryPoolID != pool.ID {
		t.Fatalf("stored primary selection = %+v", stored)
	}
	replay, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, group.ClaimVersion)
	if err != nil || replay.Applied || replay.Group.ClaimVersion != selection.Group.ClaimVersion {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	var conflicting ConflictError
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, selection.Group.ClaimVersion+1); !errors.As(err, &conflicting) {
		t.Fatalf("future claim error = %v, want ConflictError", err)
	}
	pending := f.pendingPool(t, "pending")
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, f.currentGroup(t).ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("pending pool selection error = %v, want ConflictError", err)
	}
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), uuid.NewV7(), 1); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("missing pool error = %v", err)
	}
	var input InputError
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), uuid.Nil(), 1); !errors.As(err, &input) {
		t.Fatalf("zero pool error = %v, want InputError", err)
	}
	if _, err := SelectPrimaryPool(t.Context(), f.pool, uuid.NewV7(), uuid.Nil(), 1); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("zero pool on missing group error = %v, want ErrGroupNotFound first", err)
	}
}

func TestSelectPrimaryPoolPostgresSerializesCompetingControllers(t *testing.T) {
	f := newSupplyFixture(t)
	first := f.activePool(t, "first")
	second := f.activePool(t, "second")
	group := f.currentGroup(t)
	targets := []uuid.UUID{first.ID.Bytes, second.ID.Bytes}
	start := make(chan struct{})
	errorsByController := make([]error, len(targets))
	var wait sync.WaitGroup
	for index, target := range targets {
		wait.Go(func() {
			<-start
			_, errorsByController[index] = SelectPrimaryPool(t.Context(), f.pool, f.groupID(), target, group.ClaimVersion)
		})
	}
	close(start)
	wait.Wait()
	succeeded := 0
	for _, err := range errorsByController {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("controller errors = %v, want exactly one success", errorsByController)
	}
	stored := f.currentGroup(t)
	firstWon := stored.PrimaryPoolID == first.ID
	secondWon := stored.PrimaryPoolID == second.ID
	if stored.ClaimVersion != group.ClaimVersion+1 || firstWon == secondWon {
		t.Fatalf("stored competing primary selection = %+v", stored)
	}
}

func TestPoolTransitionsPostgres(t *testing.T) {
	f := newSupplyFixture(t)
	pending := f.pendingPool(t, "unused")
	var conflicting ConflictError
	if _, _, err := DrainPool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("drain pending pool error = %v, want ConflictError", err)
	}
	_, disabled, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != "disabled" || disabled.ClaimVersion != pending.ClaimVersion+1 || disabled.SealedAt.Valid {
		t.Fatalf("disabled pending pool = %+v", disabled)
	}
	_, replay, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion)
	if err != nil || replay.ClaimVersion != disabled.ClaimVersion {
		t.Fatalf("disable replay = %+v, %v", replay, err)
	}
	if _, _, err := DisablePool(t.Context(), f.pool, f.groupID(), uuid.NewV7(), 1); !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("missing pool error = %v", err)
	}
	var input InputError
	if _, _, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, 0); !errors.As(err, &input) {
		t.Fatalf("zero claim version error = %v, want InputError", err)
	}
}

func TestDisablePoolPostgresWaitsForRegisteringHostToBeLost(t *testing.T) {
	f := newSupplyFixture(t)
	pending := f.pendingPool(t, "lost-before-activation")
	hostID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `
INSERT INTO worker_hosts (id, resource_id, worker_group_id, worker_pool_id, status)
VALUES ($1, 'lost-before-activation', $2, $3, 'registering')`, hostID, f.group.ID, pending.ID)
	var conflicting ConflictError
	if _, _, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("disable pending pool with registering host error = %v, want ConflictError", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status = 'lost', lost_at = now() WHERE id = $1`, hostID)
	_, disabled, err := DisablePool(t.Context(), f.pool, f.groupID(), pending.ID.Bytes, pending.ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Status != "disabled" || disabled.ClaimVersion != pending.ClaimVersion+1 || disabled.SealedAt.Valid {
		t.Fatalf("disabled pending pool with lost host = %+v", disabled)
	}
}

func TestDrainPoolPostgresRejectsPrimaryPool(t *testing.T) {
	f := newSupplyFixture(t)
	pool := f.activePool(t, "current")
	if _, err := SelectPrimaryPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, f.currentGroup(t).ClaimVersion); err != nil {
		t.Fatal(err)
	}
	var conflicting ConflictError
	if _, _, err := DrainPool(t.Context(), f.pool, f.groupID(), pool.ID.Bytes, pool.ClaimVersion); !errors.As(err, &conflicting) {
		t.Fatalf("drain primary pool error = %v, want ConflictError", err)
	}
	if got := f.currentGroup(t); got.Status != db.WorkerGroupStatusActive {
		t.Fatalf("group = %+v", got)
	}
}
