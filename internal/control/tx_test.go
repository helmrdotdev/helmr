package control

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestInTxCommitsAndRunsAfterCommit(t *testing.T) {
	tx := &testControlTx{}
	server := &Server{tx: testTxBeginner{tx: tx}}
	var called bool
	var order []string
	if err := server.inTx(context.Background(), func(work *txWork) error {
		if work.q == nil {
			t.Fatal("tx work query store is nil")
		}
		called = true
		work.AfterCommit(func(context.Context) error {
			order = append(order, "first")
			return nil
		})
		work.AfterCommit(func(context.Context) error {
			order = append(order, "second")
			return nil
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called || len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("called=%v order=%v", called, order)
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
}

func TestInTxRollsBackOnError(t *testing.T) {
	tx := &testControlTx{}
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

func TestInTxDoesNotRunAfterCommitAfterRollback(t *testing.T) {
	tx := &testControlTx{}
	server := &Server{tx: testTxBeginner{tx: tx}}
	want := errors.New("work failed")
	var afterCommit bool
	err := server.inTx(context.Background(), func(work *txWork) error {
		work.AfterCommit(func(context.Context) error {
			afterCommit = true
			return nil
		})
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if afterCommit {
		t.Fatal("afterCommit ran after rollback")
	}
}

func TestInTxJoinsRollbackError(t *testing.T) {
	workErr := errors.New("work failed")
	rollbackErr := errors.New("rollback failed")
	tx := &testControlTx{rollbackErr: rollbackErr}
	server := &Server{tx: testTxBeginner{tx: tx}}
	err := server.inTx(context.Background(), func(*txWork) error {
		return workErr
	})
	if !errors.Is(err, workErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("err = %v, want work and rollback errors", err)
	}
}

func TestInTxRollsBackOnCommitError(t *testing.T) {
	want := errors.New("commit failed")
	tx := &testControlTx{commitErr: want}
	server := &Server{tx: testTxBeginner{tx: tx}}
	err := server.inTx(context.Background(), func(*txWork) error {
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if !tx.committed || !tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
	}
}

func TestInTxDoesNotRollbackAfterPostCommitError(t *testing.T) {
	want := errors.New("after commit failed")
	tx := &testControlTx{}
	server := &Server{tx: testTxBeginner{tx: tx}}
	err := server.inTx(context.Background(), func(work *txWork) error {
		work.AfterCommit(func(context.Context) error { return want })
		return nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if !tx.committed || tx.rolledBack {
		t.Fatalf("committed=%v rolledBack=%v", tx.committed, tx.rolledBack)
	}
}

func TestInTxRollsBackAndRepanics(t *testing.T) {
	tx := &testControlTx{}
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

type testControlTx struct {
	committed   bool
	rolledBack  bool
	commitErr   error
	rollbackErr error
}

func (tx *testControlTx) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected nested transaction")
}

func (tx *testControlTx) Commit(context.Context) error {
	tx.committed = true
	return tx.commitErr
}

func (tx *testControlTx) Rollback(context.Context) error {
	tx.rolledBack = true
	return tx.rollbackErr
}

func (tx *testControlTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("unexpected CopyFrom")
}

func (tx *testControlTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}

func (tx *testControlTx) LargeObjects() pgx.LargeObjects {
	panic("unexpected LargeObjects")
}

func (tx *testControlTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("unexpected Prepare")
}

func (tx *testControlTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("unexpected Exec")
}

func (tx *testControlTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	panic("unexpected Query")
}

func (tx *testControlTx) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("unexpected QueryRow")
}

func (tx *testControlTx) Conn() *pgx.Conn {
	return nil
}
