package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

const computerObjectRequestLimit = (16 << 20) + 4096

func (s *Server) workerRegisterInitialComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerInitialComputerObject(w, r, false)
}
func (s *Server) workerCertifyInitialComputerObject(w http.ResponseWriter, r *http.Request) {
	s.workerInitialComputerObject(w, r, true)
}
func (s *Server) workerInitialComputerObject(w http.ResponseWriter, r *http.Request, certify bool) {
	w.Header().Set("Cache-Control", "no-store")
	var request workerapi.InitialComputerObjectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(errors.New("invalid computer object request")))
		return
	}
	id, err := ids.Parse(request.RuntimeInstanceID)
	if err != nil || request.DesiredVersion <= 0 {
		writeError(w, badRequest(errors.New("runtime identity and desired version are required")))
		return
	}
	object, err := describeComputerObject(request.Inspection)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	fence := computerKeyFence{ComputerPreparationFence: dispatch.ComputerPreparationFence{RuntimeID: pgvalue.UUID(id), WorkerID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerEpoch: worker.WorkerEpoch, DesiredVersion: request.DesiredVersion}, ClaimVersion: worker.ClaimVersion, GroupClaimVersion: worker.GroupClaimVersion}
	var uploaded *cas.Object
	if certify {
		// Restrict storage lookup to an exact registration belonging to this physical
		// Runtime's Computer. This is not a commit grant: the owner rechecks all live
		// preparation authority and exact facts after storage I/O.
		var registered bool
		registered, err = s.db.HasRegisteredInitialComputerObject(r.Context(), db.HasRegisteredInitialComputerObjectParams{RuntimeID: fence.RuntimeID, WorkerID: fence.WorkerID, WorkerGroupID: fence.WorkerGroupID, WorkerEpoch: fence.WorkerEpoch, DesiredVersion: fence.DesiredVersion, Digest: object.digest, Inspection: object.encoded})
		if err != nil || !registered {
			writeError(w, conflict(errors.New("computer object registration is unavailable")))
			return
		}
		if s.cas == nil {
			writeError(w, unavailable(errors.New("computer object storage is unavailable")))
			return
		}
		stored, err := s.cas.Stat(r.Context(), object.digest)
		if err != nil {
			writeError(w, unavailable(errors.New("computer object is not available in storage")))
			return
		}
		if stored.Digest != object.digest || stored.SizeBytes != object.size || stored.MediaType != "application/octet-stream" {
			writeError(w, conflict(errors.New("stored computer object differs from registration")))
			return
		}
		uploaded = &stored
	}
	if err = recordInitialComputerObject(r.Context(), s.tx, fence, request.Inspection, uploaded); err != nil {
		writeError(w, conflict(errors.New("computer object authority or registration changed")))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
