package db

import (
	"context"
	"uuid"
)

// ExecutionBudget excludes optional preparation from real-work entitlement.
// Allocation callers hold the Environment admission lock; read projections are
// advisory snapshots and never reserve this budget.
type ExecutionBudget struct{ CPU, Memory, Residents int64 }

func (b ExecutionBudget) Fits(cpu, memory int64, computer bool) bool {
	return cpu > 0 && memory > 0 && cpu <= b.CPU && memory <= b.Memory && (!computer || b.Residents >= 1)
}
func ReadExecutionBudget(ctx context.Context, q DBTX, env uuid.UUID) (ExecutionBudget, error) {
	var b ExecutionBudget
	err := q.QueryRow(ctx, `SELECT e.max_cpu_millis-COALESCE(sum(a.reserved_cpu_millis),0)::bigint,
 e.max_memory_bytes-COALESCE(sum(a.reserved_memory_bytes),0)::bigint,
 e.max_resident_computers-count(a.allocation_id) FILTER(WHERE a.kind='computer')
 FROM environments e LEFT JOIN (`+AllocationUsageSQL+`) a ON a.environment_id=e.id AND a.kind<>'seed'
 WHERE e.id=$1 GROUP BY e.id`, env).Scan(&b.CPU, &b.Memory, &b.Residents)
	return b, err
}
