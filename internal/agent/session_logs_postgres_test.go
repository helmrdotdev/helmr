package agent

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
)

func TestSessionLogAcceptanceBindsPhysicalAttachment(t *testing.T) {
	f := newFixture(t)
	e := f.execution()
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	bounds := diagnostic.Bounds{ChunkBytes: 4, SourceBytes: 16, SourceRecords: 4, EnvironmentBytes: 32, EnvironmentRecords: 8, QueueBytes: 64, QueueRecords: 16}
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: time.Now().UnixNano(), Data: []byte{0, 255, 128}}
	first, err := AppendSessionLog(t.Context(), f.pool, *f.host(), e, attachment.Sequence, record, bounds)
	if err != nil {
		t.Fatal(err)
	}
	next, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = AppendSessionLog(t.Context(), f.pool, *f.host(), e, attachment.Sequence, record, bounds); !errors.Is(err, ErrDenied) {
		t.Fatalf("stale physical sender: %v", err)
	}
	retry, err := AppendSessionLog(t.Context(), f.pool, *f.host(), e, next.Sequence, record, bounds)
	if err != nil || !retry.AcceptedAt.Equal(first.AcceptedAt) {
		t.Fatalf("retained logical producer %+v %v", retry, err)
	}
	wrong := e
	wrong.LeaseEpoch++
	if _, err = AppendSessionLog(t.Context(), f.pool, *f.host(), wrong, next.Sequence, record, bounds); err == nil {
		t.Fatal("foreign lease replay accepted")
	}
	host := *f.host()
	host.Epoch++
	if _, err = AppendSessionLog(t.Context(), f.pool, host, e, next.Sequence, record, bounds); err == nil {
		t.Fatal("foreign host replay accepted")
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2`, f.env, f.session)
	end := diagnostic.Record{Stream: "stdout", Kind: "end", Sequence: 2, ThroughSequence: 2, ObservedAtUnixNano: time.Now().UnixNano(), Complete: true}
	if _, err = AppendSessionLog(t.Context(), f.pool, *f.host(), e, next.Sequence, end, bounds); err != nil {
		t.Fatalf("exact stopped tail rejected: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	if _, err = AppendSessionLog(t.Context(), f.pool, *f.host(), e, next.Sequence, end, bounds); err == nil {
		t.Fatal("expired physical owner replay accepted")
	}
	var rows int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("unexpected records %d %v", rows, err)
	}
}

func TestSessionLogQueueContentionReleasesLifecycleLocks(t *testing.T) {
	f := newFixture(t)
	e := f.execution()
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err = blocker.Exec(t.Context(), `SELECT pg_advisory_xact_lock(1835363442,1684627815)`); err != nil {
		t.Fatal(err)
	}
	// The queue stays locked until test cleanup. Admission must return a retryable
	// error, not hold the Session's lifecycle locks until the caller times out.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	bounds := diagnostic.Bounds{ChunkBytes: 4, SourceBytes: 16, SourceRecords: 4, EnvironmentBytes: 32, EnvironmentRecords: 8, QueueBytes: 64, QueueRecords: 16}
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("log")}
	if _, err = AppendSessionLog(ctx, f.pool, *f.host(), e, attachment.Sequence, record, bounds); !telemetry.IsDiagnosticBusy(err) {
		t.Fatalf("queue contention: %v", err)
	}
	if _, err = RenewRuntimeAuthority(ctx, f.pool, *f.host(), e); err != nil {
		t.Fatalf("lifecycle blocked by unrelated queue: %v", err)
	}
	var records int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM telemetry_outbox`).Scan(&records); err != nil || records != 0 {
		t.Fatalf("rejected append committed records=%d err=%v", records, err)
	}
}

// This exercises persisted restore authority and receipts. The opaque checkpoint
// fixture is not evidence of native VM memory restoration.
func TestSessionLogReceiptSurvivesPhysicalHostRestore(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	source := f.execution()
	attachment, err := AcquireRuntimeAttachment(ctx, f.pool, *f.host(), source)
	if err != nil {
		t.Fatal(err)
	}
	bounds := diagnostic.Bounds{ChunkBytes: 4, SourceBytes: 16, SourceRecords: 4, EnvironmentBytes: 32, EnvironmentRecords: 8, QueueBytes: 64, QueueRecords: 16}
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte{0, 255, 128}}
	first, err := AppendSessionLog(ctx, f.pool, *f.host(), source, attachment.Sequence, record, bounds)
	if err != nil {
		t.Fatal(err)
	}
	// Discard the receipt at the producer, then retain the logical process through
	// the normal checkpoint publication and target-authority installation.
	checkpoint := checkpointStorageForFixture(t, f)
	if err = checkpoint.publisher.RegisterCheckpoint(ctx, checkpoint.ref, checkpoint.manifest); err != nil {
		t.Fatal(err)
	}
	checkpoint.upload(t)
	if err = checkpoint.publish(t, checkpoint.save, checkpoint.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err = checkpoint.publisher.CompleteCheckpoint(ctx, checkpoint.ref, checkpoint.manifest); err != nil {
		t.Fatal(err)
	}
	host := *f.host()
	host.HostID = uuid.NewV7()
	dbtest.MustExec(t, ctx, f.pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','diagnostic-restore-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.worker, host.HostID)
	restored := &computerRestoreFixture{checkpointStorageFixture: checkpoint, host: host}
	restored.newTarget(t)
	installation := restored.prepare(t)
	if err = ValidateComputerRestore(ctx, f.pool, host, f.env, installation, restoreReceipt(installation, false, false)); err != nil {
		t.Fatal(err)
	}
	if err = CommitComputerRestore(ctx, f.pool, host, f.env, installation, restoreReceipt(installation, true, false)); err != nil {
		t.Fatal(err)
	}
	acknowledgeComputerMembers(t, f, host, installation)
	if err = CompleteComputerRestore(ctx, f.pool, host, f.env, installation, restoreReceipt(installation, true, true)); err != nil {
		t.Fatal(err)
	}
	target := restored.execution()
	current, err := AcquireRuntimeAttachment(ctx, f.pool, host, target)
	if err != nil {
		t.Fatal(err)
	}
	if target.ProcessEpoch != source.ProcessEpoch || target.WorkerHostID == source.WorkerHostID || target.LeaseEpoch == source.LeaseEpoch {
		t.Fatal("fixture did not preserve logical process across physical replacement")
	}
	retry, err := AppendSessionLog(ctx, f.pool, host, target, current.Sequence, record, bounds)
	if err != nil || !retry.AcceptedAt.Equal(first.AcceptedAt) || !retry.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("restored retry %+v %v", retry, err)
	}
	if _, err = AppendSessionLog(ctx, f.pool, *f.host(), source, attachment.Sequence, record, bounds); err == nil {
		t.Fatal("fenced source accepted")
	}
	record.Sequence, record.ThroughSequence = 2, 2
	if _, err = AppendSessionLog(ctx, f.pool, host, target, current.Sequence, record, bounds); err != nil {
		t.Fatalf("restored producer cannot continue sequence: %v", err)
	}
	var count int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM telemetry_outbox`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("restored logical stream duplicated count=%d err=%v", count, err)
	}
}
