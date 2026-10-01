package run_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func timerWait(fence run.ExecutionFence, waitID uuid.UUID, fingerprint string) run.TimerWait {
	return run.TimerWait{
		Fence: fence, WaitID: waitID, Fingerprint: fingerprint,
		DueAt:       time.Now().Add(time.Hour),
		IdleTimeout: pgtype.Int8{Int64: 30_000, Valid: true},
		Metadata:    json.RawMessage(`{}`), Tags: []string{},
	}
}

func TestRegisterTimerWaitRegistersOnceAndRejectsStaleWork(t *testing.T) {
	f, work, fence, _ := taskExecutionFixture(t)
	waitID := uuid.NewV7()
	request := timerWait(fence, waitID, dbtest.Digest("timer-wait"))
	registered, err := run.RegisterTimerWait(t.Context(), f.Pool, request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := run.RegisterTimerWait(t.Context(), f.Pool, request)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ID != replayed.ID || registered.Kind != db.WaitKindTimer || registered.RunID.Bytes != work.RunID {
		t.Fatalf("registered=%+v replayed=%+v", registered, replayed)
	}
	changed := timerWait(fence, waitID, dbtest.Digest("changed-timer-wait"))
	if _, err := run.RegisterTimerWait(t.Context(), f.Pool, changed); !errors.Is(err, run.ErrStale) {
		t.Fatalf("changed registration = %v", err)
	}
	cursor := timerWait(fence, uuid.NewV7(), dbtest.Digest("cursor-timer-wait"))
	cursor.Cursor = pgtype.Int8{Int64: 1, Valid: true}
	if _, err := run.RegisterTimerWait(t.Context(), f.Pool, cursor); !errors.Is(err, run.ErrWaitCursor) {
		t.Fatalf("Task cursor = %v", err)
	}
	generation := int64(1)
	turn := timerWait(fence, uuid.NewV7(), dbtest.Digest("turn-timer-wait"))
	turn.RunGeneration = &generation
	if _, err := run.RegisterTimerWait(t.Context(), f.Pool, turn); !errors.Is(err, run.ErrTurnScope) {
		t.Fatalf("partial Turn = %v", err)
	}
	stale := timerWait(fence, uuid.NewV7(), dbtest.Digest("stale-timer-wait"))
	stale.Fence.LeaseSequence++
	if _, err := run.RegisterTimerWait(t.Context(), f.Pool, stale); !errors.Is(err, run.ErrStale) {
		t.Fatalf("stale receipt = %v", err)
	}
	claims := timerWait(fence, uuid.NewV7(), dbtest.Digest("claims-timer-wait"))
	claims.Fence.HostClaimVersion++
	if _, err := run.RegisterTimerWait(t.Context(), f.Pool, claims); !errors.Is(err, workergroup.ErrStaleClaims) {
		t.Fatalf("stale claims = %v", err)
	}
	var waits int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM run_waits WHERE run_id=$1`, work.RunID).Scan(&waits); err != nil {
		t.Fatal(err)
	}
	if waits != 1 {
		t.Fatalf("waits = %d", waits)
	}
}
