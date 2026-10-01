package controlplane

import (
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
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

func workerExecutionFence(worker workergroup.HostPrincipal, parsed parsedRunLeaseFence, lease workerapi.RunLeaseFence) run.ExecutionFence {
	return run.ExecutionFence{LeaseID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID), WorkerEpoch: worker.Epoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.HostClaimVersion}
}

// workerLeaseFence is the execution fence of a worker's Run lease; a
// malformed lease receipt is returned as its parse error.
func workerLeaseFence(worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence) (run.ExecutionFence, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return run.ExecutionFence{}, err
	}
	return workerExecutionFence(worker, parsed, lease), nil
}

// workerSourceReceipt is the worker's receipt for its source Run lease, as
// the run owner checks it.
func workerSourceReceipt(worker workergroup.HostPrincipal, lease workerapi.RunLeaseFence) run.SourceReceipt {
	return run.SourceReceipt{Worker: worker, LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence}
}
