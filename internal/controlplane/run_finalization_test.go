package controlplane

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestParseRunFinalization(t *testing.T) {
	request := validRunFinalizationRequest()
	parsed, err := parseRunFinalization(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.operationID.String() != request.OperationID ||
		parsed.fingerprint == "" {
		t.Fatalf("parsed finalization = %+v", parsed)
	}
}

func TestRunFinalizationFingerprintIncludesLeaseFence(t *testing.T) {
	first := validRunFinalizationRequest()
	second := first
	second.Lease.LeaseSequence++
	left, err := parseRunFinalization(first)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := parseRunFinalization(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.fingerprint == changed.fingerprint {
		t.Fatal("changed Lease fence did not change fingerprint")
	}
}

func TestParseRunFinalizationRejectsMismatchedQuiescenceProof(t *testing.T) {
	request := validRunFinalizationRequest()
	request.ProgramQuiesced.AttemptNumber = 0
	if _, err := parseRunFinalization(request); err == nil {
		t.Fatal("invalid Attempt was accepted")
	}

	request = validRunFinalizationRequest()
	request.ProgramQuiesced.RunLeaseID = uuid.NewV7().String()
	if _, err := parseRunFinalization(request); err == nil {
		t.Fatal("mismatched Run Lease was accepted")
	}
}

func validRunFinalizationRequest() workerapi.BeginRunFinalizationRequest {
	lease := workerapi.RunLeaseFence{ID: uuid.NewV7().String(), LeaseSequence: 1}
	return workerapi.BeginRunFinalizationRequest{
		Lease: lease,
		ProgramQuiesced: workerapi.RunQuiescenceProof{
			RunID: uuid.NewV7().String(), AttemptNumber: 1, RunLeaseID: lease.ID,
		},
		OperationID: uuid.NewV7().String(),
	}
}
