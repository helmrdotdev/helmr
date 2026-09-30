package controlplane

import (
	"context"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerRegisterCheckpointComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerCheckpointComputerObject(w, r, s.publisher.RegisterCheckpointObject)
}

func (s *Server) workerCertifyCheckpointComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerCheckpointComputerObject(w, r, s.publisher.CertifyCheckpointObject)
}

func (s *Server) workerReuseCheckpointComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerCheckpointComputerObject(w, r, s.publisher.ReuseCheckpointObject)
}

func (s *Server) workerCheckpointComputerObject(w http.ResponseWriter, r *http.Request, record func(context.Context, computer.CheckpointRef, blockformat.ObjectInspection) error) {
	var request workerapi.CheckpointComputerObjectRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	ref, err := checkpointRef(workerFromContext(r.Context()), request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = computer.ValidateObjectInspection(request.Inspection); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = record(r.Context(), ref, request.Inspection); err != nil {
		s.writeWorkerComputerError(w, err, computerCheckpointObjectOperation, "computer object publication failed")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
