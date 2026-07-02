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
