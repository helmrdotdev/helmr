package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerInitialComputerKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.InitialComputerKeyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(errors.New("invalid initial computer key request")))
		return
	}
	runtimeID, err := ids.Parse(request.RuntimeInstanceID)
	if err != nil || request.DesiredVersion <= 0 {
		writeError(w, badRequest(errors.New("runtime identity and desired version are required")))
		return
	}
	worker := workerFromContext(r.Context())
	material, err := s.computerKeys.initial(r.Context(), computerKeyFence{
		ComputerPreparationFence: dispatch.ComputerPreparationFence{
			RuntimeID: pgvalue.UUID(runtimeID), WorkerID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerEpoch: worker.WorkerEpoch,
			DesiredVersion: request.DesiredVersion,
		},
		ClaimVersion: worker.ClaimVersion, GroupClaimVersion: worker.GroupClaimVersion,
	})
	if err != nil {
		if errors.Is(err, errComputerKeyUnavailable) {
			writeError(w, conflict(errors.New("computer key authority is unavailable")))
		} else {
			writeError(w, unavailable(errors.New("computer key delivery is unavailable")))
		}
		return
	}
	defer clear(material.Key)
	writeJSON(w, http.StatusOK, workerapi.ComputerKeyMaterial{Scope: material.Scope, ID: material.ID, Key: material.Key})
}

func (s *Server) workerComputerSource(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.ComputerSourceRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(errors.New("invalid computer source request")))
		return
	}
	runtimeID, err := ids.Parse(request.RuntimeInstanceID)
	if err != nil || request.DesiredVersion <= 0 {
		writeError(w, badRequest(errors.New("runtime identity and desired version are required")))
		return
	}
	worker := workerFromContext(r.Context())
	material, err := s.computerKeys.source(r.Context(), computerKeyFence{
		ComputerPreparationFence: dispatch.ComputerPreparationFence{
			RuntimeID: pgvalue.UUID(runtimeID), WorkerID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerEpoch: worker.WorkerEpoch,
			DesiredVersion: request.DesiredVersion,
		},
		ClaimVersion: worker.ClaimVersion, GroupClaimVersion: worker.GroupClaimVersion,
	})
	if err != nil {
		if errors.Is(err, errComputerKeyUnavailable) {
			writeError(w, conflict(errors.New("computer key authority is unavailable")))
		} else {
			writeError(w, unavailable(errors.New("computer key delivery is unavailable")))
		}
		return
	}
	defer material.clear()
	response := workerapi.ComputerSourceMaterial{VersionID: material.VersionID, Root: material.Root, WriteKeyID: material.WriteKeyID}
	for _, key := range material.Keys {
		response.Keys = append(response.Keys, workerapi.ComputerKeyMaterial{Scope: key.Scope, ID: key.ID, Key: key.Key})
	}
	writeJSON(w, http.StatusOK, response)
}
