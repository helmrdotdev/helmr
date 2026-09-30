package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerCompleteTask(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CompleteTaskRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid task completion JSON: %w", err))
		return
	}
	completion, err := parseTaskCompletionRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if err := s.completeTask(r.Context(), worker, request, completion); err != nil {
		if writeStaleWorkerClaims(w, err) {
			return
		}
		if errors.Is(err, errStaleTaskCompletion) {
			if point, ok := staleAuthorityPointOf(err); ok {
				s.log.Warn(
					"task completion receipt rejected",
					"failure_point", point,
					"run_lease_id", request.Lease.ID,
					"lease_sequence", request.Lease.LeaseSequence,
					"worker_host_id", worker.HostID,
					"worker_group_id", worker.GroupID,
					"worker_epoch", worker.Epoch,
				)
			}
			writeError(w, conflict(err))
			return
		}
		if isDeterministicWorkerAdmission(err) {
			s.log.Warn("task completion admission rejected", "run_lease_id", request.Lease.ID, "error", err)
			writeError(w, apiError{kind: errUnprocessable, err: errors.New("task completion admission is invalid")})
			return
		}
		s.log.Error("complete Task failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("complete task"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
