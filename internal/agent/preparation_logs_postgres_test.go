package agent

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
)

func TestPreparationLogAcceptanceBindsAttemptExecutor(t *testing.T) {
	f := newPreparationFixture(t)
	p := f.attach(t, f.waiter(t))
	ref := f.claim(t, p)
	bounds := diagnostic.Bounds{ChunkBytes: 4, SourceBytes: 16, SourceRecords: 4, EnvironmentBytes: 32, EnvironmentRecords: 8, QueueBytes: 64, QueueRecords: 16}
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: time.Now().UnixNano(), Data: []byte{0, 255, 128}}
	first, err := AppendPreparationLog(t.Context(), f.pool, *f.host(), ref, record, bounds)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := AppendPreparationLog(t.Context(), f.pool, *f.host(), ref, record, bounds)
	if err != nil || replay != first {
		t.Fatalf("immutable retry: %v %v", replay, err)
	}
	for _, change := range []func(*PreparationExecutor){func(r *PreparationExecutor) { r.Epoch++ }, func(r *PreparationExecutor) { r.InstanceID = uuid.NewV7() }, func(r *PreparationExecutor) { r.EnvironmentID = uuid.NewV7() }, func(r *PreparationExecutor) { r.PreparationID = uuid.NewV7() }, func(r *PreparationExecutor) { r.ChannelCredential = bytes.Repeat([]byte{7}, 32) }} {
		wrong := ref
		change(&wrong)
		if _, err := AppendPreparationLog(t.Context(), f.pool, *f.host(), wrong, record, bounds); err == nil {
			t.Fatal("foreign executor accepted")
		}
	}
	host := *f.host()
	host.Epoch++
	if _, err := AppendPreparationLog(t.Context(), f.pool, host, ref, record, bounds); err == nil {
		t.Fatal("foreign physical host accepted")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, ref.PreparationID)
	if _, err := AppendPreparationLog(t.Context(), f.pool, *f.host(), ref, record, bounds); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired executor: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows=%d %v", count, err)
	}
}

func TestPreparationLogContentionDoesNotHoldLifecycle(t *testing.T) {
	f := newPreparationFixture(t)
	ref := f.claim(t, f.attach(t, f.waiter(t)))
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err := blocker.Exec(t.Context(), `SELECT pg_advisory_xact_lock(1835363442,1684627815)`); err != nil {
		t.Fatal(err)
	}
	bounds := diagnostic.Bounds{ChunkBytes: 4, SourceBytes: 16, SourceRecords: 4, EnvironmentBytes: 32, EnvironmentRecords: 8, QueueBytes: 64, QueueRecords: 16}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("log")}
	if _, err := AppendPreparationLog(ctx, f.pool, *f.host(), ref, record, bounds); !telemetry.IsDiagnosticBusy(err) {
		t.Fatalf("queue contention: %v", err)
	}
	if _, err := RenewPreparation(ctx, f.pool, *f.host(), ref); err != nil {
		t.Fatalf("renewal blocked: %v", err)
	}
}
