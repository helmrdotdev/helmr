package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// TxBeginner starts the PostgreSQL transactions that RunTx owns.
type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type txLifecycleError struct {
	stage string
	err   error
}

func (e txLifecycleError) Error() string {
	return e.stage
}

func (e txLifecycleError) Unwrap() error {
	return e.err
}

func txError(stage string, err error) error {
	if err == nil {
		return nil
	}
	return txLifecycleError{stage: stage, err: err}
}

// RunTx runs fn in one PostgreSQL transaction. It commits when fn returns nil
// and rolls back when fn returns an error, panics or the commit fails; a panic
// is re-raised after rollback. Lifecycle errors keep their cause for errors.Is
// but report only the transaction stage in their text.
func RunTx(ctx context.Context, txb TxBeginner, fn func(pgx.Tx) error) error {
	if fn == nil {
		return errors.New("transaction function is required")
	}
	if txb == nil {
		return errors.New("transactional database is required")
	}
	tx, err := txb.Begin(ctx)
	if err != nil {
		return txError("begin transaction", err)
	}
	return runTransaction(ctx, tx, fn)
}

func runTransaction(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) (err error) {
	if tx == nil {
		return errors.New("transaction is required")
	}
	committed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if !committed {
				err = errors.Join(err, txError("rollback transaction", tx.Rollback(context.WithoutCancel(ctx))))
			}
			panic(recovered)
		}
		if err != nil && !committed {
			err = errors.Join(err, txError("rollback transaction", tx.Rollback(context.WithoutCancel(ctx))))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return txError("commit transaction", err)
	}
	committed = true
	return nil
}
