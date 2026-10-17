package controlplane

import (
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http"
)

func (s *Server) workerAgentStoragePressure(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentStoragePressure
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if request.Kind != "computer" {
		s.writeAllocationError(w, session.ErrInvalidInput)
		return
	}
	identity, err := allocationIdentity(request.AllocationIdentity)
	if err == nil {
		err = session.ReportStoragePressure(r.Context(), s.tx, workerFromContext(r.Context()), computerAllocationIdentity(identity), request.Active)
	}
	if err != nil {
		s.writeAgentSaveError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
