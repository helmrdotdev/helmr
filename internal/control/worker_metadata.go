package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const runMetadataJSONMaxBytes = 256 * 1024

func (s *Server) workerUpdateRunMetadata(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerUpdateRunMetadataRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run metadata request JSON: %w", err)))
		return
	}
	_, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	operation := strings.TrimSpace(request.Operation)
	key := strings.TrimSpace(request.Key)
	amount := pgtype.Numeric{}
	switch operation {
	case "set":
		if key == "" {
			writeError(w, badRequest(errors.New("metadata key is required")))
			return
		}
		if len(request.Value) == 0 {
			writeError(w, badRequest(errors.New("metadata value is required")))
			return
		}
	case "patch":
		if len(request.Patch) == 0 {
			writeError(w, badRequest(errors.New("metadata patch is required")))
			return
		}
	case "increment":
		if key == "" {
			writeError(w, badRequest(errors.New("metadata key is required")))
			return
		}
		if err := amount.Scan(fmt.Sprintf("%g", request.Amount)); err != nil {
			writeError(w, badRequest(errors.New("metadata increment amount must be numeric")))
			return
		}
	default:
		writeError(w, badRequest(errors.New("metadata operation must be set, patch, or increment")))
		return
	}
	row, err := s.db.UpdateRunMetadataForExecution(r.Context(), db.UpdateRunMetadataForExecutionParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		Operation:        operation,
		Key:              key,
		Value:            request.Value,
		Patch:            request.Patch,
		Amount:           amount,
		MaxMetadataBytes: runMetadataJSONMaxBytes,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("update worker run metadata failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("update run metadata"))
		return
	}
	if row.MetadataTooLarge {
		writeError(w, badRequest(errors.New("run metadata is too large")))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: request.Lease.RunID})
}
