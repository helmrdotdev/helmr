package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// computerPreparationRef parses the Instance preparation a worker request
// addresses.
func computerPreparationRef(instanceID string, desiredVersion int64) (computer.PreparationRef, error) {
	id, err := ids.Parse(instanceID)
	if err != nil || desiredVersion <= 0 {
		return computer.PreparationRef{}, badRequest(errors.New("instance identity and desired version are required"))
	}
	return computer.PreparationRef{InstanceID: id, DesiredVersion: desiredVersion}, nil
}

func (s *Server) workerComputerSource(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.ComputerSourceRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid computer source request: %w", err))
		return
	}
	ref, err := computerPreparationRef(request.ComputerInstanceID, request.DesiredVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	material, err := s.computerKeys.SourceKeys(r.Context(), workerFromContext(r.Context()), ref)
	if err != nil {
		s.writeWorkerComputerError(w, err, computerKeyDeliveryOperation, "computer source delivery failed")
		return
	}
	defer material.Clear()
	response := workerapi.ComputerSourceMaterial{VersionID: material.VersionID, Root: material.Root, WriteKeyID: material.WriteKeyID}
	for _, key := range material.Keys {
		response.Keys = append(response.Keys, workerapi.ComputerKeyMaterial{Scope: key.Scope, ID: key.ID, Key: key.Key})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerPrepareComputerSeed(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.PrepareComputerSeedRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid seed preparation request: %w", err))
		return
	}
	ref, err := computerPreparationRef(request.ComputerInstanceID, request.DesiredVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	prepared, err := s.computerKeys.PrepareSeed(r.Context(), workerFromContext(r.Context()), ref)
	if err != nil {
		s.writeWorkerComputerError(w, err, computerKeyDeliveryOperation, "computer seed preparation failed")
		return
	}
	defer clear(prepared.Key.Key)
	response := workerapi.ComputerSeedPreparation{Status: prepared.Status}
	if prepared.Status == "convert" {
		response.Key = &workerapi.ComputerKeyMaterial{Scope: prepared.Key.Scope, ID: prepared.Key.ID, Key: prepared.Key.Key}
	}
	writeJSON(w, http.StatusOK, response)
}
