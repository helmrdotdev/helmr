package pglock

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestTryAcquireHoldsAndReleasesKey(t *testing.T) {
	database := dbtest.Open(t)
	key := Key("held")
	guard, locked, err := TryAcquire(t.Context(), database.Pool, key)
	if err != nil || !locked {
		t.Fatalf("TryAcquire = %v, %v", locked, err)
	}
	if _, contended, err := TryAcquire(t.Context(), database.Pool, key); err != nil || contended {
		t.Fatalf("contended TryAcquire = %v, %v", contended, err)
	}

	observer, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Release()
	var acquired bool
	if err := observer.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if acquired {
		t.Fatal("observer acquired the held key")
	}

	if err := guard.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := observer.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("observer did not acquire the released key")
	}
	var released bool
	if err := observer.QueryRow(t.Context(), "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("observer did not release the key")
	}
	if err := guard.Unlock(); err == nil {
		t.Fatal("second Unlock succeeded")
	}
}

func TestGuardDiscardsConnectionWhenReleaseCannotBeConfirmed(t *testing.T) {
	database := dbtest.Open(t)
	key := Key("unconfirmed-release")
	guard, locked, err := TryAcquire(t.Context(), database.Pool, key)
	if err != nil || !locked {
		t.Fatalf("TryAcquire = %v, %v", locked, err)
	}
	var backendPID int32
	if err := guard.Conn().QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&backendPID); err != nil {
		t.Fatal(err)
	}
	var released bool
	if err := guard.Conn().QueryRow(t.Context(), "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if !released {
		t.Fatal("test setup did not release the PostgreSQL advisory lock")
	}
	if err := guard.Unlock(); err == nil {
		t.Fatal("Unlock accepted an unconfirmed release")
	}

	conn, err := database.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var replacementPID int32
	if err := conn.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&replacementPID); err != nil {
		t.Fatal(err)
	}
	if replacementPID == backendPID {
		t.Fatalf("discarded backend %d returned to the pool", backendPID)
	}
}

func TestGuardDiscardsConnectionWhenReleaseQueryFails(t *testing.T) {
	database := dbtest.Open(t)
	key := Key("failed-release-query")
	guard, locked, err := TryAcquire(t.Context(), database.Pool, key)
	if err != nil || !locked {
		t.Fatalf("TryAcquire = %v, %v", locked, err)
	}
	var backendPID int32
	if err := guard.Conn().QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&backendPID); err != nil {
		t.Fatal(err)
	}
	terminator, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := terminator.QueryRow(t.Context(), "SELECT pg_terminate_backend($1)", backendPID).Scan(&terminated); err != nil {
		terminator.Release()
		t.Fatal(err)
	}
	terminator.Release()
	if !terminated {
		t.Fatal("test setup did not terminate the lock backend")
	}
	if err := guard.Unlock(); err == nil {
		t.Fatal("Unlock accepted a failed release query")
	}

	conn, err := database.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var replacementPID int32
	if err := conn.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&replacementPID); err != nil {
		t.Fatal(err)
	}
	if replacementPID == backendPID {
		t.Fatalf("discarded backend %d returned to the pool", backendPID)
	}
}
