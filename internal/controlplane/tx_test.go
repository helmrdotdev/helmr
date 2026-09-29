package controlplane

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestInTxWithBuildsWorkForBegunTransaction(t *testing.T) {
	tx := &inTxWithTestTransaction{}
	var called bool
	if err := inTxWith(context.Background(), inTxWithTestBeginner{tx: tx}, func(work *txWork) error {
		if work.q == nil {
			t.Fatal("tx work query store is nil")
		}
		if work.tx != tx {
			t.Fatal("tx work does not hold the begun transaction")
		}
		called = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called || !tx.committed {
		t.Fatalf("called=%v committed=%v", called, tx.committed)
	}
}

func TestInTxWithRejectsNilBody(t *testing.T) {
	err := inTxWith(context.Background(), inTxWithTestBeginner{tx: &inTxWithTestTransaction{}}, nil)
	if err == nil || err.Error() != "transaction function is required" {
		t.Fatalf("err = %v, want transaction function is required", err)
	}
}

type inTxWithTestBeginner struct {
	tx pgx.Tx
}

func (b inTxWithTestBeginner) Begin(context.Context) (pgx.Tx, error) {
	return b.tx, nil
}

type inTxWithTestTransaction struct {
	pgx.Tx
	committed bool
}

func (tx *inTxWithTestTransaction) Commit(context.Context) error {
	tx.committed = true
	return nil
}

func (tx *inTxWithTestTransaction) Rollback(context.Context) error {
	return nil
}
