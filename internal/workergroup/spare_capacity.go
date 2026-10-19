package workergroup

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrHostBusy = errors.New("worker host still owns execution")

var ErrSpareCapacity = errors.New("drain would remove required spare capacity")

// A spare slot fits the pool's full per-VM shape. Reuse actual placement rules;
// raw VM slot counts alone cannot establish CPU, memory or disk availability.
func spareItem(plan *poolPlan) item {
	resources := plan.template.perVM
	resources.VMSlots = 1
	return item{targetPoolID: plan.id, vmPlatformID: plan.template.vmPlatformID, resources: resources}
}

func placeSpareSlots(plan *poolPlan, bins []bin, limit int32) int32 {
	candidate := spareItem(plan)
	var count int32
	for index := range bins {
		if !candidateMatchesBin(candidate, bins[index]) {
			continue
		}
		for count < limit && place(&bins[index], candidate) {
			count++
		}
		if count == limit {
			break
		}
	}
	return count
}

func planSpareSlots(plan *poolPlan, available []bin) {
	if plan.reserve == 0 {
		return
	}
	// Keep actual readiness separate from hypothetical workers and consume
	// queued demand before testing the spare capacity left over.
	ready := placeSpareSlots(plan, append([]bin(nil), available...), plan.reserve+1)
	plan.result.SpareSlotsShortfall = max(plan.reserve-ready, 0)
	plan.result.ScaleInBlocked = plan.result.ScaleInBlocked || ready <= plan.reserve
	remaining := plan.result.SpareSlotsShortfall
	remaining -= placeSpareSlots(plan, plan.bins, remaining)
	for remaining > 0 && int32(len(plan.bins)) < plan.max {
		fresh := []bin{plan.template}
		placed := placeSpareSlots(plan, fresh, remaining)
		if placed == 0 {
			break
		}
		remaining -= placed
		plan.bins = append(plan.bins, fresh[0])
		plan.result.RecommendedAdditionalWorkers++
	}
	plan.result.Saturated = plan.result.Saturated || remaining > 0
}

// Called after the candidate's drain transition, in the same transaction. The
// group mutation lock excludes admissions and other drains until commit. An
// insufficient result rolls the transition back; a prior plan is not authority.
func checkSpareCapacity(ctx context.Context, tx pgx.Tx, groupID, poolID uuid.UUID, minimum int32) error {
	if minimum == 0 {
		return nil
	}
	q := db.New(tx)
	pools, err := q.ListCapacityWorkerPools(ctx, db.ListCapacityWorkerPoolsParams{
		WorkerGroupID: pgvalue.UUID(groupID), WorkerPoolIDs: []pgtype.UUID{pgvalue.UUID(poolID)},
	})
	if err != nil {
		return err
	}
	if len(pools) != 1 {
		return ErrSpareCapacity
	}
	plan, err := capacityPoolPlan(pools[0], 0)
	if err != nil {
		return err
	}
	bins, complete, err := currentBins(ctx, q, groupID)
	if err != nil {
		return fmt.Errorf("check spare capacity: %w", err)
	}
	if !complete || placeSpareSlots(&plan, bins, minimum) < minimum {
		return ErrSpareCapacity
	}
	return nil
}
