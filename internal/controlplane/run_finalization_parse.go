package controlplane

import (
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type parsedRunFinalization struct {
	lease       parsedRunLeaseFence
	runID       uuid.UUID
	attempt     int32
	operationID uuid.UUID
	fingerprint string
}

func parseRunFinalization(request workerapi.BeginRunFinalizationRequest) (parsedRunFinalization, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	operationID, err := parseCanonicalUUID("operation_id", request.OperationID)
	if err != nil {
		return parsedRunFinalization{}, err
	}

	quiescedRunID, err := parseCanonicalUUID("program_quiesced.run_id", request.ProgramQuiesced.RunID)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	quiescedLeaseID, err := parseCanonicalUUID("program_quiesced.run_lease_id", request.ProgramQuiesced.RunLeaseID)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	if quiescedLeaseID != lease.leaseID ||
		request.ProgramQuiesced.AttemptNumber <= 0 {
		return parsedRunFinalization{}, errors.New("program_quiesced does not match the run lease")
	}
	normalized := request
	normalized.OperationID = operationID.String()
	normalized.ProgramQuiesced.RunID = quiescedRunID.String()
	normalized.ProgramQuiesced.RunLeaseID = quiescedLeaseID.String()
	fingerprint, err := run.RequestFingerprint("run.finalization.begin.v0", normalized)
	if err != nil {
		return parsedRunFinalization{}, fmt.Errorf("fingerprint run finalization: %w", err)
	}
	return parsedRunFinalization{
		lease: lease, runID: quiescedRunID, attempt: request.ProgramQuiesced.AttemptNumber,
		operationID: operationID, fingerprint: fingerprint,
	}, nil
}
