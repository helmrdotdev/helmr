package controlplane

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (s *Server) capacityScaleIn(w http.ResponseWriter, r *http.Request) {
	group, pool, ok := capacityPoolIDs(w, r)
	if !ok {
		return
	}
	var request struct {
		OperationKey string `json:"operation_key"`
	}
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	key, err := ids.Parse(request.OperationKey)
	if err != nil {
		writeError(w, badRequest(errors.New("operation_key must be a canonical UUIDv7")))
		return
	}
	result, created, err := workergroup.SelectScaleIn(r.Context(), s.tx, s.auditPolicy, audit.CapacityAuthority(), group, pool, key)
	if err != nil {
		s.writeWorkerGroupError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (s *Server) capacityGetScaleIn(w http.ResponseWriter, r *http.Request) {
	group, ok := capacityWorkerGroupID(w, r)
	if !ok {
		return
	}
	key, err := ids.Parse(chi.URLParam(r, "operationKey"))
	if err != nil {
		writeError(w, badRequest(errors.New("operation_key must be a canonical UUIDv7")))
		return
	}
	result, err := workergroup.GetScaleInOperation(r.Context(), s.tx, group, key)
	if err != nil {
		s.writeWorkerGroupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) capacityPendingScaleIn(w http.ResponseWriter, r *http.Request) {
	group, ok := capacityWorkerGroupID(w, r)
	if !ok {
		return
	}
	if _, err := workergroup.GetGroup(r.Context(), s.db, group); err != nil {
		s.writeWorkerGroupError(w, r, err)
		return
	}
	result, err := workergroup.PendingScaleInOperation(r.Context(), s.tx, group)
	if err != nil {
		s.writeWorkerGroupError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Operation *workergroup.ScaleInOperation `json:"operation"`
	}{result})
}
