package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type txWork struct {
	q  db.Querier
	tx pgx.Tx
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

func inTxWith(ctx context.Context, txb TxBeginner, fn func(*txWork) error) error {
	if fn == nil {
		return errors.New("transaction function is required")
	}
	if txb == nil {
		return errors.New("transactional control plane database is required")
	}
	tx, err := txb.Begin(ctx)
	if err != nil {
		return txError("begin transaction", err)
	}
	return runTransaction(ctx, tx, fn)
}

func runTransaction(ctx context.Context, tx pgx.Tx, fn func(*txWork) error) (err error) {
	if tx == nil {
		return errors.New("transaction is required")
	}
	work := &txWork{q: db.New(tx), tx: tx}
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
	if err := fn(work); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return txError("commit transaction", err)
	}
	committed = true
	return nil
}
