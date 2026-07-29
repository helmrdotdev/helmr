package dispatch

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestAdvisoryLockDiscardsConnectionWhenUnlockIsNotConfirmed(t *testing.T) {
	database := dbtest.Open(t)
	lock, err := newAdvisoryLock(database.Pool, "test-discard-unconfirmed")
	if err != nil {
		t.Fatal(err)
	}
	guard, locked, err := lock.tryLock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("expected advisory lock")
	}
	conn := guard.guard.Conn()
	lockedBackend := backendPID(t, conn)
	var unlocked bool
	if err := conn.QueryRow(
		t.Context(),
		"select pg_advisory_unlock($1)",
		lock.key,
	).Scan(&unlocked); err != nil {
		t.Fatal(err)
	}
	if !unlocked {
		t.Fatal("expected manual unlock")
	}
	if err := guard.Unlock(t.Context()); err == nil {
		t.Fatal("expected unlock confirmation error")
	}
	replacement, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Release()
	if replacementBackend := backendPID(t, replacement); replacementBackend == lockedBackend {
		t.Fatalf("unconfirmed session reused backend %d", lockedBackend)
	}
}

func TestAdvisoryLockDiscardsConnectionWhenUnlockQueryFails(t *testing.T) {
	database := dbtest.Open(t)
	lock, err := newAdvisoryLock(database.Pool, "test-discard-query-failure")
	if err != nil {
		t.Fatal(err)
	}
	guard, locked, err := lock.tryLock(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("expected advisory lock")
	}
	lockedBackend := backendPID(t, guard.guard.Conn())
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
	if err := guard.Unlock(t.Context()); err == nil {
		t.Fatal("expected unlock query error")
	}
	conn, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if replacementBackend := backendPID(t, conn); replacementBackend == lockedBackend {
		t.Fatalf("failed unlock reused backend %d", lockedBackend)
	}
}

func backendPID(t *testing.T, conn interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) int32 {
	t.Helper()
	var pid int32
	if err := conn.QueryRow(t.Context(), "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}
