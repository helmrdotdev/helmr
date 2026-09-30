package controlplane

import (
	"context"
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// computerSaveRef parses the save a worker request addresses.
func computerSaveRef(request workerapi.ComputerSaveBeginRequest) (computer.SaveRef, error) {
	environment, environmentErr := ids.Parse(request.EnvironmentID)
	instance, instanceErr := ids.Parse(request.ComputerInstanceID)
	save, saveErr := ids.Parse(request.SaveID)
	if environmentErr != nil || instanceErr != nil || saveErr != nil {
		return computer.SaveRef{}, badRequest(errors.New("save, environment and instance IDs must be canonical UUIDv7"))
	}
	return computer.SaveRef{EnvironmentID: environment, InstanceID: instance, WriterGeneration: request.WriterGeneration, SaveID: save, Sequence: request.Sequence}, nil
}

func (s *Server) workerBeginComputerSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerSaveBeginRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	ref, err := computerSaveRef(request)
	if err != nil {
		writeError(w, err)
		return
	}
	begun, err := s.publisher.BeginSave(r.Context(), workerFromContext(r.Context()), ref)
	if err != nil {
		s.writeWorkerComputerError(w, err, computerSaveOperation, "computer object publication failed")
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerSaveBeginResponse{ComputerInstanceID: request.ComputerInstanceID, WriterGeneration: request.WriterGeneration, PredecessorID: pgvalue.UUIDString(begun.PredecessorID), DesiredVersion: begun.DesiredVersion, SaveID: request.SaveID, Sequence: request.Sequence})
}

func (s *Server) workerAbandonComputerSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerSaveBeginRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	ref, err := computerSaveRef(request)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = s.publisher.AbandonSave(r.Context(), workerFromContext(r.Context()), ref); err != nil {
		s.writeWorkerComputerError(w, err, computerSaveOperation, "computer object publication failed")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) workerPublishComputerSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerSavePublicationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if _, err := request.Root.Locator(request.Root.LogicalBytes); err != nil {
		writeError(w, badRequest(err))
		return
	}
	ref, err := computerSaveRef(request.Save)
	if err != nil {
		writeError(w, err)
		return
	}
	published, err := s.publisher.PublishSave(r.Context(), workerFromContext(r.Context()), ref, request.Root)
	if err != nil {
		s.writeWorkerComputerError(w, err, computerSaveOperation, "computer object publication failed")
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerSavePublicationResponse{ComputerID: published.ComputerID.String(), VersionID: published.VersionID.String()})
}

func (s *Server) workerAdoptComputerSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerSavePublicationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	if _, err := request.Root.Locator(request.Root.LogicalBytes); err != nil {
		writeError(w, badRequest(err))
		return
	}
	ref, err := computerSaveRef(request.Save)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = s.publisher.AdoptSave(r.Context(), workerFromContext(r.Context()), ref, request.Root); err != nil {
		s.writeWorkerComputerError(w, err, computerSaveOperation, "computer object publication failed")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) workerRegisterComputerSaveObject(w http.ResponseWriter, r *http.Request) {
	s.workerComputerSaveObject(w, r, s.publisher.RegisterSaveObject)
}

func (s *Server) workerCertifyComputerSaveObject(w http.ResponseWriter, r *http.Request) {
	s.workerComputerSaveObject(w, r, s.publisher.CertifySaveObject)
}

func (s *Server) workerReuseComputerSaveObject(w http.ResponseWriter, r *http.Request) {
	s.workerComputerSaveObject(w, r, s.publisher.ReuseSaveObject)
}

func (s *Server) workerComputerSaveObject(w http.ResponseWriter, r *http.Request, record func(context.Context, workergroup.HostPrincipal, computer.SaveRef, blockformat.ObjectInspection) error) {
	var request workerapi.ComputerSaveObjectRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, err)
		return
	}
	ref, err := computerSaveRef(request.Save)
	if err != nil {
		writeError(w, err)
		return
	}
	if err = computer.ValidateObjectInspection(request.Inspection); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err = record(r.Context(), workerFromContext(r.Context()), ref, request.Inspection); err != nil {
		s.writeWorkerComputerError(w, err, computerSaveOperation, "computer object publication failed")
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
