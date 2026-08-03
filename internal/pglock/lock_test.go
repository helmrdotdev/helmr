package pglock

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestAcquireHoldsAndReleasesEveryKey(t *testing.T) {
	database := dbtest.Open(t)
	keys := []int64{Key("first"), Key("second")}
	guard, err := Acquire(t.Context(), database.Pool, keys)
	if err != nil {
		t.Fatal(err)
	}

	observer, err := database.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Release()
	for _, key := range keys {
		var acquired bool
		if err := observer.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
			t.Fatal(err)
		}
		if acquired {
			t.Fatalf("observer acquired held key %d", key)
		}
	}

	if err := guard.Unlock(); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		var acquired bool
		if err := observer.QueryRow(t.Context(), "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
			t.Fatal(err)
		}
		if !acquired {
			t.Fatalf("observer did not acquire released key %d", key)
		}
		var released bool
		if err := observer.QueryRow(t.Context(), "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil {
			t.Fatal(err)
		}
		if !released {
			t.Fatalf("observer did not release key %d", key)
		}
	}
}

func TestGuardDiscardsConnectionWhenReleaseCannotBeConfirmed(t *testing.T) {
	database := dbtest.Open(t)
	key := Key("unconfirmed-release")
	guard, err := Acquire(t.Context(), database.Pool, []int64{key})
	if err != nil {
		t.Fatal(err)
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
