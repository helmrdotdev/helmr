package fleet

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestLeaderLeaseDiscardsConnectionWhenUnlockIsNotConfirmed(t *testing.T) {
	database := dbtest.Open(t)
	elector, err := NewPGLeaderElector(database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := elector.TryAcquire(t.Context(), "discard-unconfirmed")
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected leader lease")
	}
	postgresLease := lease.(*pgLeaderLease)
	var lockedBackend int32
	conn := postgresLease.guard.Conn()
	if err := conn.QueryRow(t.Context(), "select pg_backend_pid()").Scan(&lockedBackend); err != nil {
		t.Fatal(err)
	}
	var unlocked bool
	if err := conn.QueryRow(
		t.Context(),
		"select pg_advisory_unlock($1)",
		workergroup.LifecycleLockKey("discard-unconfirmed"),
	).Scan(&unlocked); err != nil {
		t.Fatal(err)
	}
	if !unlocked {
		t.Fatal("expected manual unlock")
	}
	if err := lease.Release(); err == nil {
		t.Fatal("expected unlock confirmation error")
	}
	replacement, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Release()
	var replacementBackend int32
	if err := replacement.QueryRow(t.Context(), "select pg_backend_pid()").Scan(&replacementBackend); err != nil {
		t.Fatal(err)
	}
	if replacementBackend == lockedBackend {
		t.Fatalf("unconfirmed session reused backend %d", lockedBackend)
	}
}

func TestLeaderLeaseDiscardsConnectionWhenUnlockQueryFails(t *testing.T) {
	database := dbtest.Open(t)
	elector, err := NewPGLeaderElector(database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := elector.TryAcquire(t.Context(), "discard-query-failure")
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected leader lease")
	}
	postgresLease := lease.(*pgLeaderLease)
	var lockedBackend int32
	if err := postgresLease.guard.Conn().QueryRow(t.Context(), "select pg_backend_pid()").Scan(&lockedBackend); err != nil {
		t.Fatal(err)
	}
	terminator, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := terminator.QueryRow(
		t.Context(),
		"select pg_terminate_backend($1)",
		lockedBackend,
	).Scan(&terminated); err != nil {
		terminator.Release()
		t.Fatal(err)
	}
	terminator.Release()
	if !terminated {
		t.Fatal("expected backend termination")
	}
	if err := lease.Release(); err == nil {
		t.Fatal("expected unlock query error")
	}
	replacement, acquired, err := elector.TryAcquire(t.Context(), "discard-query-failure")
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected replacement leader lease")
	}
	if err := replacement.Release(); err != nil {
		t.Fatal(err)
	}
}
