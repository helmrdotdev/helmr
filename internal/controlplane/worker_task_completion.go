package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerCompleteTask(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.CompleteTaskRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid task completion JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid task completion JSON: trailing value")))
		return
	}
	completion, err := parseTaskCompletionRequest(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if err := s.completeTask(r.Context(), worker, request, completion); err != nil {
		if errors.Is(err, errStaleTaskCompletion) {
			if point, ok := taskCompletionFailurePointOf(err); ok {
				s.log.Warn(
					"task completion receipt rejected",
					"failure_point", point,
					"run_lease_id", request.Lease.ID,
					"lease_sequence", request.Lease.LeaseSequence,
					"worker_instance_id", worker.WorkerInstanceID,
					"worker_group_id", worker.WorkerGroupID,
					"worker_epoch", worker.WorkerEpoch,
				)
			}
			writeError(w, conflict(errStaleTaskCompletion))
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
