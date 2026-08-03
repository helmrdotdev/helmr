package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestInTxCommits(t *testing.T) {
	tx := &testTransaction{}
	server := &Server{tx: testTxBeginner{tx: tx}}
	var called bool
	if err := server.inTx(context.Background(), func(work *txWork) error {
		if work.q == nil {
			t.Fatal("tx work query store is nil")
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

func TestInTxReturnsBeginError(t *testing.T) {
	want := errors.New("begin failed")
	server := &Server{tx: testTxBeginner{beginErr: want}}
	err := server.inTx(context.Background(), func(*txWork) error {
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

func TestInTxRollsBackOnError(t *testing.T) {
	tx := &testTransaction{}
	server := &Server{tx: testTxBeginner{tx: tx}}
	want := errors.New("work failed")
	err := server.inTx(context.Background(), func(*txWork) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if tx.committed || !tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
	}
}

func TestInTxJoinsRollbackError(t *testing.T) {
	workErr := errors.New("work failed")
	rollbackErr := errors.New("rollback failed")
	tx := &testTransaction{rollbackErr: rollbackErr}
	server := &Server{tx: testTxBeginner{tx: tx}}
	err := server.inTx(context.Background(), func(*txWork) error {
		return workErr
	})
	if !errors.Is(err, workErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("err = %v, want work and rollback errors", err)
	}
	if strings.Contains(err.Error(), rollbackErr.Error()) {
		t.Fatalf("err string leaked rollback detail: %q", err.Error())
	}
}

func TestInTxRollsBackOnCommitError(t *testing.T) {
	want := errors.New("commit failed")
	tx := &testTransaction{commitErr: want}
	server := &Server{tx: testTxBeginner{tx: tx}}
	err := server.inTx(context.Background(), func(*txWork) error {
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

func TestInTxRollsBackAndRepanics(t *testing.T) {
	tx := &testTransaction{}
	server := &Server{tx: testTxBeginner{tx: tx}}
	defer func() {
		recovered := recover()
		if recovered != "boom" {
			t.Fatalf("recovered = %v, want boom", recovered)
		}
		if tx.committed || !tx.rolledBack {
			t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
		}
	}()
	_ = server.inTx(context.Background(), func(*txWork) error {
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
