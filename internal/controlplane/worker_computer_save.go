package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http"
)

func (s *Server) workerBeginComputerSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerSaveBeginRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateComputerSaveRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	response, err := s.beginComputerSave(r.Context(), workerFromContext(r.Context()), request)
	if err != nil {
		s.writeRunComputerObjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerAbandonComputerSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerSaveBeginRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateComputerSaveRequest(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	err := s.abandonComputerSave(r.Context(), workerFromContext(r.Context()), request)
	if err != nil {
		s.writeRunComputerObjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) workerPublishComputerSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerSavePublicationRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if _, err := request.Root.Locator(request.Root.LogicalBytes); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateComputerSaveRequest(request.Save); err != nil {
		writeError(w, badRequest(err))
		return
	}
	result, err := s.publishComputerSave(r.Context(), workerFromContext(r.Context()), request.Save, request.Root)
	if err != nil {
		s.writeRunComputerObjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.ComputerSavePublicationResponse{ComputerID: pgvalue.UUIDString(result.ComputerID), VersionID: pgvalue.UUIDString(result.VersionID)})
}

func (s *Server) workerAdoptComputerSave(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerSavePublicationRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if _, err := request.Root.Locator(request.Root.LogicalBytes); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateComputerSaveRequest(request.Save); err != nil {
		writeError(w, badRequest(err))
		return
	}
	err := s.adoptComputerSave(r.Context(), workerFromContext(r.Context()), request.Save, request.Root)
	if err != nil {
		s.writeRunComputerObjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) workerRegisterComputerSaveObject(w http.ResponseWriter, r *http.Request) {
	s.workerComputerSaveObject(w, r, "register")
}
func (s *Server) workerCertifyComputerSaveObject(w http.ResponseWriter, r *http.Request) {
	s.workerComputerSaveObject(w, r, "certify")
}
func (s *Server) workerReuseComputerSaveObject(w http.ResponseWriter, r *http.Request) {
	s.workerComputerSaveObject(w, r, "reuse")
}
func (s *Server) workerComputerSaveObject(w http.ResponseWriter, r *http.Request, operation string) {
	var request workerapi.ComputerSaveObjectRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateComputerSaveRequest(request.Save); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if _, err := describeComputerObject(request.Inspection); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := s.recordComputerSaveObject(r.Context(), workerFromContext(r.Context()), request.Save, request.Inspection, operation); err != nil {
		s.writeRunComputerObjectError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func validateComputerSaveRequest(request workerapi.ComputerSaveBeginRequest) error {
	if _, err := parseCanonicalUUID("save_id", request.SaveID); err != nil {
		return err
	}
	if request.Sequence <= 0 {
		return errors.New("positive save sequence required")
	}
	if request.Lease != nil {
		if request.OrgID != "" || request.WorkspaceMountID != "" {
			return errors.New("one execution authority required")
		}
		_, err := parseRunLeaseFence(*request.Lease)
		return err
	}
	if _, err := parseCanonicalUUID("org_id", request.OrgID); err != nil {
		return err
	}
	_, err := parseCanonicalUUID("workspace_mount_id", request.WorkspaceMountID)
	return err
}
