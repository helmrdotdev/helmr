package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func (s *Server) workerRenewComputerInstance(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerInstanceRenewRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if ids.Validate(request.EnvironmentID) != nil || ids.Validate(request.ComputerInstanceID) != nil || request.WriterGeneration <= 0 {
		writeError(w, badRequest(errors.New("environment, Instance and positive writer generation are required")))
		return
	}
	var instance db.ComputerInstance
	err := s.inTx(r.Context(), func(work *txWork) error {
		var err error
		instance, err = renewComputerInstance(r.Context(), work.tx, workerFromContext(r.Context()), request)
		return err
	})
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("Computer Instance writer is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("renew Computer Instance writer"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerInstanceRenewResponse{ComputerInstanceID: pgvalue.UUIDString(instance.ID), WriterGeneration: instance.WriterGeneration, DesiredState: instance.DesiredState, DesiredVersion: instance.DesiredVersion, ObservedVersion: instance.ObservedVersion, WriterExpiresAt: instance.WriterExpiresAt.Time})
}
