package control

import (
	"context"
	"errors"
	"fmt"

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
	afterCommit []func(context.Context) error
}

func (work *txWork) AfterCommit(fn func(context.Context) error) {
	if fn == nil {
		return
	}
	work.afterCommit = append(work.afterCommit, fn)
}

func (s *Server) inTx(ctx context.Context, fn func(*txWork) error) (err error) {
	if fn == nil {
		return errors.New("transaction function is required")
	}
	if beginner, ok := s.db.(queryTransactionBeginner); ok {
		q, tx, err := beginner.BeginQuerier(ctx)
		if err != nil {
			return err
		}
		return runControlTransaction(ctx, q, tx, fn)
	}
	if s.tx == nil {
		return errors.New("transactional control database is required")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return err
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
				err = errors.Join(err, tx.Rollback(context.WithoutCancel(ctx)))
			}
			panic(recovered)
		}
		if err != nil && !committed {
			err = errors.Join(err, tx.Rollback(context.WithoutCancel(ctx)))
		}
	}()
	if err := fn(work); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	for _, effect := range work.afterCommit {
		if effectErr := effect(context.WithoutCancel(ctx)); effectErr != nil {
			err = errors.Join(err, effectErr)
		}
	}
	if err != nil {
		return fmt.Errorf("run post-commit effects: %w", err)
	}
	return nil
}

func (s *Server) beginControlTransaction(ctx context.Context) (db.Querier, controlTransaction, error) {
	if beginner, ok := s.db.(queryTransactionBeginner); ok {
		return beginner.BeginQuerier(ctx)
	}
	if s.tx == nil {
		return nil, nil, errors.New("transactional control database is required")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	return db.New(tx), tx, nil
}
