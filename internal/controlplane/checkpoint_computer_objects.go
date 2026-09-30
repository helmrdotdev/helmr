package controlplane

import (
	"context"
	"errors"
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
	instanceID, err := parseCanonicalUUID("computer_instance_id", request.ComputerInstanceID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	checkpointID, err := parseCanonicalUUID("checkpoint_id", request.CheckpointID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.WorkerEpoch <= 0 || request.DesiredVersion <= 0 {
		writeError(w, badRequest(errors.New("checkpoint source versions must be positive")))
		return
	}
	if err = computer.ValidateObjectInspection(request.Inspection); err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	ref := computer.CheckpointRef{
		Host:       computer.Host{GroupID: worker.GroupID, HostID: worker.HostID, Epoch: worker.Epoch},
		InstanceID: instanceID, WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.DesiredVersion, CheckpointID: checkpointID,
	}
	if err = record(r.Context(), ref, request.Inspection); err != nil {
		s.writeComputerPublicationError(w, err, computerCheckpointObjectOperation)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
