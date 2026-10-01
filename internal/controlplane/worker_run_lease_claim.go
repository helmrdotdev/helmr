package controlplane

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerClaimRunLease(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunLeaseClaimRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run lease claim request JSON: %w", err))
		return
	}
	leaseIDValue, err := ids.Parse(request.LeaseID)
	if err != nil || request.LeaseSequence <= 0 {
		writeError(w, badRequest(errors.New("lease_id must be a canonical UUIDv7 and lease_sequence must be positive")))
		return
	}
	lease := workerapi.RunLeaseFence{ID: request.LeaseID, LeaseSequence: request.LeaseSequence}
	worker := workerFromContext(r.Context())
	claim, err := run.ClaimLease(r.Context(), s.tx, workerExecutionFence(worker, parsedRunLeaseFence{leaseID: leaseIDValue}, lease))
	if err != nil {
		s.writeRunError(w, err, runLeaseClaimOperation, worker, lease)
		return
	}
	if _, restored := claim.ResumeWait(); restored {
		response, err := projectRestoredRunLeaseClaim(claim, s.computerFencingKey)
		if err != nil {
			s.writeRunLeaseClaimFailure(w, claim, err)
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	responseAuthority := runLeaseClaimResponseAuthority{
		actor:    claim.Session(),
		run:      claim.Run(),
		attempt:  claim.Attempt(),
		runtime:  claim.Instance(),
		runLease: claim.Lease(),
		computer: claim.Computer(),
	}
	projection, err := loadRunLeaseClaimProjection(r.Context(), s.db, responseAuthority)
	if err != nil {
		s.writeRunLeaseClaimFailure(w, claim, err)
		return
	}
	response, err := projectRunLeaseClaimResponse(
		r.Context(),
		responseAuthority,
		claim.DeliverySecrets(),
		projection,
		s.platformStore,
		s.secretDelivery,
		s.computerFencingKey,
	)
	if err != nil {
		s.writeRunLeaseClaimFailure(w, claim, err)
		return
	}
	response.ProtectedEnv, err = workerProtectedEnv(r.Context(), s.db, responseAuthority.computer.EnvironmentID, responseAuthority.computer.ID)
	if err != nil {
		s.writeRunLeaseClaimFailure(w, claim, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// writeRunLeaseClaimFailure writes the failure to project a committed claim,
// which the worker may claim again.
func (s *Server) writeRunLeaseClaimFailure(w http.ResponseWriter, claim run.Claim, err error) {
	s.log.Error(
		"serve worker Run Lease claim failed",
		"run_id", pgvalue.UUIDString(claim.Run().ID),
		"run_lease_id", pgvalue.UUIDString(claim.Lease().ID),
		"error", err,
	)
	writeError(w, errors.New("serve worker run lease claim"))
}
