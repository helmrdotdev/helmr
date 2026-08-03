package control

import (
	"errors"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type parsedRunLeaseFence struct {
	leaseID uuid.UUID
}

func parseRunLeaseFence(fence workerapi.RunLeaseFence) (parsedRunLeaseFence, error) {
	leaseID, err := ids.Parse(fence.ID)
	if err != nil {
		return parsedRunLeaseFence{}, errors.New("lease.id must be a canonical UUIDv7")
	}
	if fence.LeaseSequence <= 0 {
		return parsedRunLeaseFence{}, errors.New("lease.lease_sequence must be positive")
	}
	return parsedRunLeaseFence{leaseID: leaseID}, nil
}
