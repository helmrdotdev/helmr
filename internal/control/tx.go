package control

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
)

type controlTransaction interface {
	Commit(context.Context) error
	Rollback(context.Context) error
}

type queryTransactionBeginner interface {
	BeginQuerier(context.Context) (db.Querier, controlTransaction, error)
}

type txWork struct {
	q           db.Querier
	afterCommit []func(context.Context)
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

// AfterCommit registers a best-effort post-commit effect for the current unit
// of work. Effects run synchronously after Commit succeeds, in registration
// order, with context cancellation detached from the request.
func (work *txWork) AfterCommit(fn func(context.Context)) {
	if fn == nil {
		return
	}
	work.afterCommit = append(work.afterCommit, fn)
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
		return runControlTransaction(ctx, q, tx, fn)
	}
	if txb == nil {
		return errors.New("transactional control database is required")
	}
	tx, err := txb.Begin(ctx)
	if err != nil {
		return txError("begin transaction", err)
	}
	return runControlTransaction(ctx, db.New(tx), tx, fn)
}

func runControlTransaction(ctx context.Context, q db.Querier, tx controlTransaction, fn func(*txWork) error) (err error) {
	if q == nil {
		return errors.New("transaction query store is required")
	}
	if tx == nil {
		return errors.New("transaction is required")
	}
	work := &txWork{q: q}
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
	for _, effect := range work.afterCommit {
		effect(context.WithoutCancel(ctx))
	}
	return nil
}
