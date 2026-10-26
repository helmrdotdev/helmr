package db

import (
	"context"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// PendingExecutionClaimsSQL keeps one claim per unallocated Computer. One
// claimant may share its sequential provisioning budget with its attached image
// build; other claimants of the same image retain their independent reservations.
const PendingExecutionClaimsSQL = `WITH candidates AS (
 SELECT c.environment_id,c.id computer_id,c.cpu_millis,c.memory_bytes,p.id build_id,
 p.worker_host_id build_host_id,COALESCE(p.reserved_cpu_millis,0) build_cpu,
 COALESCE(p.reserved_memory_bytes,0) build_memory,
 COALESCE(p.reserved_scratch_bytes,0) build_scratch,
 CASE WHEN p.worker_host_id IS NOT NULL AND p.started_at IS NULL THEN 1 ELSE 0 END build_start,
 COALESCE((d.cpu_millis+999)/1000*1000,0) required_build_cpu,COALESCE(d.memory_bytes,0) required_build_memory,
 row_number() OVER(PARTITION BY c.environment_id,p.id ORDER BY c.id) ordinal
 FROM computers c LEFT JOIN image_builds p ON p.environment_id=c.environment_id AND p.id=COALESCE((SELECT successor.id FROM image_builds successor WHERE successor.environment_id=c.environment_id AND successor.predecessor_build_id=c.image_build_id),c.image_build_id)
 AND p.fenced_at IS NULL AND p.status IN ('queued','running','succeeded')
 LEFT JOIN LATERAL (SELECT cpu_millis,memory_bytes FROM template_definitions d WHERE d.environment_id=p.environment_id AND d.image_spec_id=p.image_spec_id ORDER BY deployment_id,template_id LIMIT 1) d ON true
 WHERE c.execution_deadline_at>clock_timestamp() AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL AND c.preparation_failed_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM (` + RevokedComputerSecretsSQL + `) revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id)
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL AND NOT EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=l.environment_id AND cp.computer_id=l.computer_id AND cp.source_lease_epoch=l.epoch AND cp.status='ready'))
 ) SELECT environment_id,computer_id,
 CASE WHEN ordinal=1 THEN build_id END build_id,
 CASE WHEN ordinal=1 THEN build_host_id END build_host_id,
 greatest((cpu_millis+999)/1000*1000,CASE WHEN ordinal=1 THEN greatest(required_build_cpu,build_cpu) ELSE 0 END)::bigint claim_cpu,
 greatest(memory_bytes,CASE WHEN ordinal=1 THEN greatest(required_build_memory,build_memory) ELSE 0 END)::bigint claim_memory,
 CASE WHEN ordinal=1 THEN build_cpu ELSE 0 END::bigint credit_cpu,
 CASE WHEN ordinal=1 THEN build_memory ELSE 0 END::bigint credit_memory,
 CASE WHEN ordinal=1 THEN build_scratch ELSE 0 END::bigint credit_scratch,
 CASE WHEN ordinal=1 THEN build_start ELSE 0 END::bigint credit_start
 FROM candidates`

type PendingExecutionClaim struct {
	EnvironmentID, ComputerID                                        uuid.UUID
	BuildID, BuildHostID                                             *uuid.UUID
	CPU, Memory, CreditCPU, CreditMemory, CreditScratch, CreditStart int64
}

func ReadPendingExecutionClaims(ctx context.Context, q DBTX, group uuid.UUID) ([]PendingExecutionClaim, error) {
	rows, err := q.Query(ctx, `SELECT c.* FROM (`+PendingExecutionClaimsSQL+`) c JOIN environments e ON e.id=c.environment_id JOIN projects p ON p.id=e.project_id JOIN worker_groups g ON g.region_id=p.default_region_id WHERE g.id=$1`, group)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PendingExecutionClaim, error) {
		var c PendingExecutionClaim
		err := row.Scan(&c.EnvironmentID, &c.ComputerID, &c.BuildID, &c.BuildHostID, &c.CPU, &c.Memory, &c.CreditCPU, &c.CreditMemory, &c.CreditScratch, &c.CreditStart)
		return c, err
	})
}

// ExecutionBudget excludes optional preparation from real-work entitlement.
// Allocation callers hold the Environment admission lock; read projections are
// advisory snapshots and never reserve this budget.
type ExecutionBudget struct{ CPU, Memory, Residents int64 }

func (b ExecutionBudget) Fits(cpu, memory int64, computer bool) bool {
	return cpu > 0 && memory > 0 && cpu <= b.CPU && memory <= b.Memory && (!computer || b.Residents >= 1)
}
func ReadExecutionBudget(ctx context.Context, q DBTX, env uuid.UUID) (ExecutionBudget, error) {
	return ReadExecutionBudgetForComputer(ctx, q, env, uuid.Nil())
}
func ReadExecutionBudgetForComputer(ctx context.Context, q DBTX, env, self uuid.UUID) (ExecutionBudget, error) {
	var b ExecutionBudget
	err := q.QueryRow(ctx, `WITH usage AS (
 SELECT reserved_cpu_millis cpu,reserved_memory_bytes memory,CASE WHEN kind='computer' THEN 1 ELSE 0 END residents
 FROM (`+AllocationUsageSQL+`) a WHERE a.environment_id=$1 AND a.kind NOT IN ('seed','retained')
 UNION ALL
 SELECT claim_cpu-credit_cpu,claim_memory-credit_memory,1 FROM (`+PendingExecutionClaimsSQL+`) c WHERE c.environment_id=$1 AND c.computer_id<>$2
 ) SELECT e.max_cpu_millis-COALESCE((SELECT sum(cpu) FROM usage),0)::bigint,
 e.max_memory_bytes-COALESCE((SELECT sum(memory) FROM usage),0)::bigint,
 e.max_resident_computers-COALESCE((SELECT sum(residents) FROM usage),0)::bigint
 FROM environments e WHERE e.id=$1`, env, self).Scan(&b.CPU, &b.Memory, &b.Residents)
	return b, err
}

// ReadImageBuildExecutionBudget permits the one sequential build to use its
// linked claim. The same derived sharing persists while that build owns a lease.
func ReadImageBuildExecutionBudget(ctx context.Context, q DBTX, env, build uuid.UUID) (ExecutionBudget, error) {
	b, err := ReadExecutionBudget(ctx, q, env)
	if err != nil {
		return b, err
	}
	var cpu, memory int64
	err = q.QueryRow(ctx, `SELECT COALESCE(max(claim_cpu),0),COALESCE(max(claim_memory),0) FROM (`+PendingExecutionClaimsSQL+`) c WHERE c.environment_id=$1 AND c.build_id=$2`, env, build).Scan(&cpu, &memory)
	b.CPU += cpu
	b.Memory += memory
	return b, err
}
