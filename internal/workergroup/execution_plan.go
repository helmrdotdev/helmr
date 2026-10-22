package workergroup

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// executionPlanStore checks accepted execution claims, excluding speculative
// image builds. A linked build and its Computer use their shared reservation in
// sequence; physical allocation still checks the live build until it is fenced.
type executionPlanStore struct {
	*db.Queries
	tx     pgx.Tx
	claims []db.PendingExecutionClaim
}

func (s executionPlanStore) ListAllocationPlanningDemand(ctx context.Context, arg db.ListAllocationPlanningDemandParams) ([]db.ListAllocationPlanningDemandRow, error) {
	rows, err := s.Queries.ListAllocationPlanningDemand(ctx, arg)
	if err != nil {
		return nil, err
	}
	// Filtering a truncated advisory result could hide an accepted claim.
	if len(rows) >= int(arg.RowLimit) {
		return nil, ErrCapacityUnavailable
	}
	budgets := map[uuid.UUID]db.ExecutionBudget{}
	for _, c := range s.claims {
		if _, ok := budgets[c.EnvironmentID]; !ok {
			b, err := db.ReadExecutionBudget(ctx, s.tx, c.EnvironmentID)
			if err != nil {
				return nil, err
			}
			budgets[c.EnvironmentID] = b
		}
	}
	for _, c := range s.claims {
		b := budgets[c.EnvironmentID]
		b.CPU += c.CPU
		b.Memory += c.Memory
		b.Residents++
		budgets[c.EnvironmentID] = b
	}
	result := make([]db.ListAllocationPlanningDemandRow, 0, len(s.claims))
	for _, c := range s.claims {
		found := false
		for _, r := range rows {
			if r.Kind != "computer" || r.EnvironmentID != pgvalue.UUID(c.EnvironmentID) || r.OwnerID != pgvalue.UUID(c.ComputerID) {
				continue
			}
			r.CpuMillis = c.CPU
			r.MemoryBytes = c.Memory
			b := budgets[c.EnvironmentID]
			r.RemainingCpuMillis = b.CPU
			r.RemainingMemoryBytes = b.Memory
			r.RemainingResidents = b.Residents
			result = append(result, r)
			found = true
			break
		}
		if !found {
			return nil, ErrCapacityUnavailable
		}
	}
	return result, nil
}
func (s executionPlanStore) ListWorkerCapacityBins(ctx context.Context, arg db.ListWorkerCapacityBinsParams) ([]db.ListWorkerCapacityBinsRow, error) {
	rows, err := s.Queries.ListWorkerCapacityBins(ctx, arg)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		r := &rows[i]
		for _, c := range s.claims {
			if c.BuildHostID == nil || r.WorkerHostID != pgvalue.UUID(*c.BuildHostID) {
				continue
			}
			r.AvailableCPUMillis += c.CreditCPU
			r.AvailableMemoryBytes += c.CreditMemory
			r.AvailableGuestEphemeralDiskBytes += c.CreditScratch
			r.AvailableVMSlots++
			r.AvailableInstanceStarts += c.CreditStart
		}
	}
	return rows, nil
}
