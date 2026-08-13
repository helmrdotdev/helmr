package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerClaimRunLease(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunLeaseClaimRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid worker run lease claim request JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid worker run lease claim request JSON: trailing value")))
		return
	}
	leaseIDValue, err := ids.Parse(request.LeaseID)
	if err != nil || request.LeaseSequence <= 0 {
		writeError(w, badRequest(errors.New("lease_id must be a canonical UUIDv7 and lease_sequence must be positive")))
		return
	}
	leaseID := pgvalue.UUID(leaseIDValue)

	authority, envelopes, err := s.claimRunLease(
		r.Context(),
		workerFromContext(r.Context()),
		leaseID,
		request.LeaseSequence,
	)
	if err != nil {
		if errors.Is(err, errStaleRunLeaseClaim) {
			writeError(w, conflict(errors.New("run lease claim is stale")))
			return
		}
		s.writeRunLeaseClaimFailure(w, authority, err)
		return
	}
	responseAuthority := runLeaseClaimResponseAuthority{
		mode:           authority.mode,
		restoreSource:  authority.restoreSource,
		actor:          authority.actor,
		childRun:       authority.childRun,
		run:            authority.run,
		attempt:        authority.attempt,
		runtime:        authority.runtime,
		runLease:       authority.runLease,
		workspace:      authority.workspace,
		workspaceMount: authority.workspaceMount,
		workspaceLease: authority.workspaceLease,
		enclosingWait:  authority.enclosingWait,
		runWait:        authority.runWait,
		checkpoint:     authority.checkpoint,
	}
	projection, err := loadRunLeaseClaimProjection(r.Context(), s.db, responseAuthority)
	if err != nil {
		s.writeRunLeaseClaimFailure(w, authority, err)
		return
	}
	response, err := projectRunLeaseClaimResponse(
		r.Context(),
		responseAuthority,
		envelopes,
		projection,
		s.platformStore,
		s.secretDelivery,
		s.workspaceFencingKey,
	)
	if err != nil {
		s.writeRunLeaseClaimFailure(w, authority, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeRunLeaseClaimFailure(
	w http.ResponseWriter,
	authority runLeaseClaimAuthority,
	err error,
) {
	s.log.Error(
		"serve worker Run Lease claim failed",
		"run_id", pgvalue.UUIDString(authority.run.ID),
		"run_lease_id", pgvalue.UUIDString(authority.runLease.ID),
		"error", err,
	)
	writeError(w, errors.New("serve worker run lease claim"))
}
