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

func (s *Server) workerInitialComputerKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.InitialComputerKeyRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid initial computer key request: %w", err))
		return
	}
	ref, err := computerPreparationRef(request.ComputerInstanceID, request.DesiredVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	material, err := s.computerKeys.InitialKey(r.Context(), workerFromContext(r.Context()), ref)
	if err != nil {
		s.writeWorkerComputerError(w, err, computerKeyDeliveryOperation, "initial computer key delivery failed")
		return
	}
	defer clear(material.Key)
	writeJSON(w, http.StatusOK, workerapi.ComputerKeyMaterial{Scope: material.Scope, ID: material.ID, Key: material.Key})
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
