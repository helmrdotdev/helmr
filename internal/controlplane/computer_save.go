package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) beginComputerSave(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
	return s.withComputerSave(ctx, worker, request, computerSaveBegin, nil)
}

func (s *Server) withComputerSave(ctx context.Context, worker workerActor, request workerapi.ComputerSaveBeginRequest, operation computerSaveOperation, apply func(pgx.Tx, *db.Queries, db.ComputerInstance) error) (workerapi.ComputerSaveBeginResponse, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return workerapi.ComputerSaveBeginResponse{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	result, err := applyComputerSave(ctx, tx, worker, request, operation, apply)
	if err != nil {
		return workerapi.ComputerSaveBeginResponse{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return workerapi.ComputerSaveBeginResponse{}, err
	}
	return result, nil
}
