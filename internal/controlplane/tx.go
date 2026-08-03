package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
)

type transaction interface {
	Commit(context.Context) error
	Rollback(context.Context) error
}

type queryTransactionBeginner interface {
	BeginQuerier(context.Context) (db.Querier, transaction, error)
}

type txWork struct {
	q  db.Querier
	tx transaction
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

// inTx owns the control-plane transaction lifecycle for request-level units of
// work. The queryTransactionBeginner branch is a temporary, package-sealed seam
// for Querier-level unit fakes; production uses ServerConfig.TX and sqlc
// queries over the pgx tx.
func (s *Server) inTx(ctx context.Context, fn func(*txWork) error) (err error) {
	return inTxWith(ctx, s.db, s.tx, fn)
}

func inTxWith(ctx context.Context, store db.Querier, txb TxBeginner, fn func(*txWork) error) (err error) {
	if fn == nil {
		return errors.New("transaction function is required")
	}
	if beginner, ok := store.(queryTransactionBeginner); ok {
		q, tx, err := beginner.BeginQuerier(ctx)
		if err != nil {
			return txError("begin transaction", err)
		}
		return runTransaction(ctx, q, tx, fn)
	}
	if txb == nil {
		return errors.New("transactional Control Plane database is required")
	}
	tx, err := txb.Begin(ctx)
	if err != nil {
		return txError("begin transaction", err)
	}
	return runTransaction(ctx, db.New(tx), tx, fn)
}

func runTransaction(ctx context.Context, q db.Querier, tx transaction, fn func(*txWork) error) (err error) {
	if q == nil {
		return errors.New("transaction query store is required")
	}
	if tx == nil {
		return errors.New("transaction is required")
	}
	work := &txWork{q: q, tx: tx}
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
