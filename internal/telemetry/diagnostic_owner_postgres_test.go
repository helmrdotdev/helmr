package telemetry

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
	"uuid"
)

func TestDiagnosticContentionReleasesAdmissionWithoutBlockingControl(t *testing.T) {
	pool := diagnosticDatabase(t)
	source := diagnosticTestSource(t, pool, "session", uuid.Nil())
	peer := diagnosticTestSource(t, pool, "session", uuid.Nil())
	blocker, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err = blocker.Exec(t.Context(), `SELECT 1 FROM session_processes WHERE session_id=$1 FOR UPDATE`, source.ID); err != nil {
		t.Fatal(err)
	}
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
	_, err = appendDiagnosticTest(t, pool, source, record, diagnosticTestBounds())
	if !IsDiagnosticBusy(err) || errors.Is(err, ErrDiagnosticCapacity) {
		t.Fatalf("owner contention misclassified: %v", err)
	}
	if _, err = appendDiagnosticTest(t, pool, peer, record, diagnosticTestBounds()); err != nil {
		t.Fatalf("busy owner retained gate: %v", err)
	}
	// A telemetry admission lock does not acquire any lifecycle owner.
	gate, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback(t.Context())
	if _, err = BeginDiagnosticAdmission(t.Context(), gate, peer, diagnosticTestBounds()); err != nil {
		t.Fatal(err)
	}
	_, err = appendDiagnosticTest(t, pool, peer, record, diagnosticTestBounds())
	if !IsDiagnosticBusy(err) || errors.Is(err, ErrDiagnosticCapacity) {
		t.Fatalf("gate contention misclassified: %v", err)
	}
	call, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = pool.Exec(call, `UPDATE session_processes SET control_sequence=control_sequence+1 WHERE session_id=$1`, peer.ID); err != nil {
		t.Fatalf("admission blocked lifecycle: %v", err)
	}
}

func TestDiagnosticOneTransactionCannotReuseStaleOccupancy(t *testing.T) {
	pool := diagnosticDatabase(t)
	source := diagnosticTestSource(t, pool, "session", uuid.Nil())
	bounds := diagnosticTestBounds()
	bounds.SourceRecords = 1
	record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
	err := db.RunTx(t.Context(), pool, func(tx pgx.Tx) error {
		admission, err := BeginDiagnosticAdmission(t.Context(), tx, source, bounds)
		if err != nil {
			return err
		}
		if _, err = admission.Append(t.Context(), record); err != nil {
			return err
		}
		record.Stream = "stderr"
		if _, err = admission.Append(t.Context(), record); !errors.Is(err, ErrDiagnosticCapacity) {
			t.Fatalf("transaction over-admission: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d %v", count, err)
	}
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = BeginDiagnosticAdmission(t.Context(), tx, source, bounds); !errors.Is(err, ErrDiagnosticInvalid) {
		t.Fatalf("stale-snapshot isolation allowed: %v", err)
	}
}

func TestDiagnosticTypedOwnerRetentionAndExportNeverLockLifecycle(t *testing.T) {
	for _, kind := range []string{"session", "computer_preparation", "computer_command"} {
		t.Run(kind, func(t *testing.T) {
			pool := diagnosticDatabase(t)
			source := diagnosticTestSource(t, pool, kind, uuid.Nil())
			record := diagnostic.Record{Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: 1, Data: []byte("abc")}
			first, err := appendDiagnosticTest(t, pool, source, record, diagnosticTestBounds())
			if err != nil {
				t.Fatal(err)
			}
			table, predicate := diagnosticOwner(source)
			if _, err = pool.Exec(t.Context(), `DELETE FROM `+table+` WHERE `+predicate, source.EnvironmentID, source.ID, source.ProducerEpoch); err == nil {
				t.Fatal("pending telemetry lost its typed owner")
			}
			claim, err := ClaimDiagnostics(t.Context(), pool, kind, diagnostic.ExportBounds{Records: 4, Bytes: 6, ClaimFor: time.Minute})
			if err != nil || len(claim.IDs) != 1 {
				t.Fatalf("claim: %+v %v", claim, err)
			}
			blocker, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(t.Context())
			if _, err = blocker.Exec(t.Context(), `SELECT 1 FROM `+table+` WHERE `+predicate+` FOR UPDATE`, source.EnvironmentID, source.ID, source.ProducerEpoch); err != nil {
				t.Fatal(err)
			}
			call, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			retired, err := RetireDiagnostics(call, pool, claim.Token, claim.IDs)
			if err != nil || retired.Delivered != 1 {
				t.Fatalf("export blocked on lifecycle: %+v %v", retired, err)
			}
			if err = blocker.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			retry, err := appendDiagnosticTest(t, pool, source, record, diagnosticTestBounds())
			if err != nil || !retry.AcceptedAt.Equal(first.AcceptedAt) {
				t.Fatalf("owner lost original receipt: %+v %v", retry, err)
			}
			record.Sequence, record.ThroughSequence = 2, 2
			if _, err = appendDiagnosticTest(t, pool, source, record, diagnosticTestBounds()); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), pool, `UPDATE telemetry_outbox SET accepted_at=accepted_at-interval '2200 hours',expires_at=expires_at-interval '2200 hours'`)
			blocker, err = pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(t.Context())
			if _, err = blocker.Exec(t.Context(), `SELECT 1 FROM `+table+` WHERE `+predicate+` FOR UPDATE`, source.EnvironmentID, source.ID, source.ProducerEpoch); err != nil {
				t.Fatal(err)
			}
			retired, err = ExpireDiagnostics(call, pool, 10)
			if err != nil || retired.Expired != 1 {
				t.Fatalf("expiry blocked on lifecycle: %+v %v", retired, err)
			}
		})
	}
}
