package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRunTxCommits(t *testing.T) {
	tx := &testTransaction{}
	txb := testTxBeginner{tx: tx}
	var called bool
	if err := db.RunTx(context.Background(), txb, func(work pgx.Tx) error {
		if work != tx {
			t.Fatal("transaction body did not receive the begun transaction")
		}
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("transaction body was not called")
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
	}
}

func TestRunTxRejectsMissingInputs(t *testing.T) {
	if err := db.RunTx(context.Background(), testTxBeginner{tx: &testTransaction{}}, nil); err == nil || err.Error() != "transaction function is required" {
		t.Fatalf("nil body err = %v", err)
	}
	if err := db.RunTx(context.Background(), nil, func(pgx.Tx) error { return nil }); err == nil || err.Error() != "transactional database is required" {
		t.Fatalf("nil beginner err = %v", err)
	}
}

func TestRunTxReturnsBeginError(t *testing.T) {
	want := errors.New("begin failed")
	txb := testTxBeginner{beginErr: want}
	err := db.RunTx(context.Background(), txb, func(pgx.Tx) error {
		t.Fatal("transaction body should not run")
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if got := err.Error(); got != "begin transaction" {
		t.Fatalf("err string = %q, want sanitized transaction stage", got)
	}
}

func TestRunTxRollsBackOnError(t *testing.T) {
	tx := &testTransaction{}
	txb := testTxBeginner{tx: tx}
	want := errors.New("work failed")
	err := db.RunTx(context.Background(), txb, func(pgx.Tx) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if tx.committed || !tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
	}
}

func TestRunTxJoinsRollbackError(t *testing.T) {
	workErr := errors.New("work failed")
	rollbackErr := errors.New("rollback failed")
	tx := &testTransaction{rollbackErr: rollbackErr}
	txb := testTxBeginner{tx: tx}
	err := db.RunTx(context.Background(), txb, func(pgx.Tx) error {
		return workErr
	})
	if !errors.Is(err, workErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("err = %v, want work and rollback errors", err)
	}
	if strings.Contains(err.Error(), rollbackErr.Error()) {
		t.Fatalf("err string leaked rollback detail: %q", err.Error())
	}
}

func TestRunTxRollsBackOnCommitError(t *testing.T) {
	want := errors.New("commit failed")
	tx := &testTransaction{commitErr: want}
	txb := testTxBeginner{tx: tx}
	err := db.RunTx(context.Background(), txb, func(pgx.Tx) error {
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("err string leaked commit detail: %q", err.Error())
	}
	if !tx.committed || !tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
	}
}

func TestRunTxRollsBackAndRepanics(t *testing.T) {
	tx := &testTransaction{}
	txb := testTxBeginner{tx: tx}
	defer func() {
		recovered := recover()
		if recovered != "boom" {
			t.Fatalf("recovered = %v, want boom", recovered)
		}
		if tx.committed || !tx.rolledBack {
			t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
		}
	}()
	_ = db.RunTx(context.Background(), txb, func(pgx.Tx) error {
		panic("boom")
	})
}

type testTxBeginner struct {
	tx       pgx.Tx
	beginErr error
}

func (b testTxBeginner) Begin(context.Context) (pgx.Tx, error) {
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}

type testTransaction struct {
	committed   bool
	rolledBack  bool
	commitErr   error
	rollbackErr error
}

func (tx *testTransaction) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected nested transaction")
}

func (tx *testTransaction) Commit(context.Context) error {
	tx.committed = true
	return tx.commitErr
}

func (tx *testTransaction) Rollback(context.Context) error {
	tx.rolledBack = true
	return tx.rollbackErr
}

func (tx *testTransaction) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unexpected CopyFrom")
}

func (tx *testTransaction) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}

func (tx *testTransaction) LargeObjects() pgx.LargeObjects {
	panic("unexpected LargeObjects")
}

func (tx *testTransaction) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected Prepare")
}

func (tx *testTransaction) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unexpected Exec")
}

func (tx *testTransaction) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected Query")
}

func (tx *testTransaction) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected QueryRow")
}

func (tx *testTransaction) Conn() *pgx.Conn {
	return nil
}

func TestRunTxBeginDiagnosticsDoNotExposePrivateCauses(t *testing.T) {
	const private = "postgres://private-user:private-password@private-host/private-database"
	cases := []struct {
		name        string
		cause       error
		kind, state string
	}{
		{"cancelled", fmt.Errorf(private+": %w", context.Canceled), "context_cancelled", ""},
		{"deadline", fmt.Errorf(private+": %w", context.DeadlineExceeded), "deadline_exceeded", ""},
		{"EOF", fmt.Errorf(private+": %w", io.EOF), "connection_closed", ""},
		{"truncated reply", fmt.Errorf(private+": %w", io.ErrUnexpectedEOF), "connection_closed", ""},
		{"postgres", &pgconn.PgError{Code: "53300", Message: private, Detail: "private SQL"}, "postgres", "53300"},
		{"malformed state", &pgconn.PgError{Code: private, Message: private}, "postgres", ""},
		{"network", &net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.9"), Port: 5432}, Err: errors.New(private)}, "network", ""},
		{"network timeout", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, "network_timeout", ""},
		{"unknown", errors.New(private), "unknown", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := db.RunTx(t.Context(), testTxBeginner{beginErr: tc.cause}, func(pgx.Tx) error { t.Fatal("unexpected transaction body"); return nil })
			if !errors.Is(err, tc.cause) || err.Error() != "begin transaction" {
				t.Fatalf("error identity/text changed: %v", err)
			}
			var output bytes.Buffer
			slog.New(slog.NewJSONHandler(&output, nil)).Error("transaction failed", "error", err)
			if strings.Contains(output.String(), "private") || strings.Contains(output.String(), "192.0.2.9") {
				t.Fatalf("private cause escaped: %s", output.String())
			}
			var record struct {
				Error struct{ Stage, Cause, SQLState string }
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record.Error.Stage != "begin transaction" || record.Error.Cause != tc.kind || record.Error.SQLState != tc.state {
				t.Fatalf("diagnostic category: %s", output.String())
			}
		})
	}
}

func TestRunTxCommitAndRollbackDiagnostics(t *testing.T) {
	const private = "postgres://private-user:private-password@private-host/private-database"
	for _, tc := range []struct {
		name, stage, cause, rollbackCause string
		work, commit, rollback            error
	}{
		{"commit with successful rollback", "commit transaction", "context_cancelled", "", nil, fmt.Errorf(private+": %w", context.Canceled), nil},
		{"commit with closed rollback", "commit transaction", "context_cancelled", "transaction_closed", nil, fmt.Errorf(private+": %w", context.Canceled), pgx.ErrTxClosed},
		{"aborted commit", "commit transaction", "transaction_rolled_back", "transaction_closed", nil, pgx.ErrTxCommitRollback, pgx.ErrTxClosed},
		{"body with failed rollback", "transaction body", "context_cancelled", "connection_closed", fmt.Errorf(private+": %w", context.Canceled), nil, fmt.Errorf(private+": %w", io.EOF)},
		{"postgres commit with failed rollback", "commit transaction", "postgres", "connection_closed", nil, &pgconn.PgError{Code: "40001", Message: private}, fmt.Errorf(private+": %w", io.EOF)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &testTransaction{commitErr: tc.commit, rollbackErr: tc.rollback}
			err := db.RunTx(t.Context(), testTxBeginner{tx: tx}, func(pgx.Tx) error { return tc.work })
			for _, cause := range []error{tc.work, tc.commit, tc.rollback} {
				if cause != nil && !errors.Is(err, cause) {
					t.Fatalf("lost error identity: %v", err)
				}
			}
			if tc.cause == "postgres" {
				var pgError *pgconn.PgError
				if !errors.As(err, &pgError) || pgError.Code != "40001" {
					t.Fatalf("lost PostgreSQL error type: %v", err)
				}
			}
			var output bytes.Buffer
			slog.New(slog.NewJSONHandler(&output, nil)).Error("transaction failed", "error", err)
			if strings.Contains(output.String(), "private") {
				t.Fatalf("private cause escaped: %s", output.String())
			}
			type stage struct{ Stage, Cause, SQLState string }
			var record struct {
				Error struct {
					stage
					Operation, Rollback stage
				}
			}
			if err := json.Unmarshal(output.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			operation := record.Error.stage
			if tc.rollback != nil {
				operation = record.Error.Operation
				if record.Error.Rollback.Stage != "rollback transaction" || record.Error.Rollback.Cause != tc.rollbackCause {
					t.Fatalf("missing rollback category: %s", output.String())
				}
			}
			if operation.Stage != tc.stage || operation.Cause != tc.cause || (tc.cause == "postgres" && operation.SQLState != "40001") {
				t.Fatalf("missing operation category: %s", output.String())
			}
		})
	}
}
