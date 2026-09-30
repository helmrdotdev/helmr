package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func (s *Server) beginComputerSave(ctx context.Context, worker workergroup.HostPrincipal, request workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
	return s.withComputerSave(ctx, worker, request, computerSaveBegin, nil)
}

func (s *Server) withComputerSave(ctx context.Context, worker workergroup.HostPrincipal, request workerapi.ComputerSaveBeginRequest, operation computerSaveOperation, apply func(pgx.Tx, *db.Queries, db.ComputerInstance) error) (workerapi.ComputerSaveBeginResponse, error) {
	var result workerapi.ComputerSaveBeginResponse
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		result, err = applyComputerSave(ctx, work.tx, worker, request, operation, apply)
		return err
	})
	if err != nil {
		return workerapi.ComputerSaveBeginResponse{}, err
	}
	return result, nil
}
