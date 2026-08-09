package dispatch

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRunPlacementLaneLockTransfersBetweenDispatchers(t *testing.T) {
	database := dbtest.Open(t)
	first, err := NewRunPlacementLaneLock(database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRunPlacementLaneLock(database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	guard, locked, err := first.TryLock(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("first dispatcher did not acquire lane")
	}
	independent, locked, err := second.TryLock(t.Context(), 8)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("second dispatcher did not acquire an independent lane")
	}
	if err := independent.Unlock(); err != nil {
		t.Fatal(err)
	}
	if _, locked, err := second.TryLock(t.Context(), 7); err != nil {
		t.Fatal(err)
	} else if locked {
		t.Fatal("second dispatcher acquired an owned lane")
	}
	if err := guard.Unlock(); err != nil {
		t.Fatal(err)
	}
	replacement, locked, err := second.TryLock(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("second dispatcher did not acquire released lane")
	}
	if err := replacement.Unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestRunPlacementLaneLockReleasesAfterConnectionLoss(t *testing.T) {
	database := dbtest.Open(t)
	first, err := NewRunPlacementLaneLock(database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewRunPlacementLaneLock(database.Pool)
	if err != nil {
		t.Fatal(err)
	}
	guard, locked, err := first.TryLock(t.Context(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("first dispatcher did not acquire lane")
	}
	store, ok := guard.Discovery().(*RunPlacementStore)
	if !ok {
		t.Fatal("lane guard did not expose its connection-bound store")
	}
	var backendPID int32
	if err := store.db.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Pool.Exec(t.Context(), `SELECT pg_terminate_backend($1)`, backendPID); err != nil {
		t.Fatal(err)
	}
	if err := guard.Unlock(); err == nil {
		t.Fatal("unlock succeeded after its database connection was terminated")
	}

	var replacement RunPlacementLaneGuard
	for range 20 {
		replacement, locked, err = second.TryLock(t.Context(), 9)
		if err != nil {
			t.Fatal(err)
		}
		if locked {
			break
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !locked {
		t.Fatal("second dispatcher did not take over lane after connection loss")
	}
	if err := replacement.Unlock(); err != nil {
		t.Fatal(err)
	}
}
