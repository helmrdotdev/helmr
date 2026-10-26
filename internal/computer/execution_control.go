package computer

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ExecutionControl is an accepted request, not evidence of physical completion.
type ExecutionControl struct {
	ID         uuid.UUID  `json:"id"`
	ComputerID uuid.UUID  `json:"computer_id"`
	Kind       string     `json:"kind"`
	Status     string     `json:"status"`
	ErrorCode  *string    `json:"error_code,omitempty"`
	AcceptedAt time.Time  `json:"accepted_at"`
	SettledAt  *time.Time `json:"settled_at"`
}

// FindExecutionControl reads an idempotent receipt under the caller's Computer
// lock. Authorization must be checked even when the operation is already settled.
func FindExecutionControl(ctx context.Context, tx pgx.Tx, env, id uuid.UUID, callerKind string, caller uuid.UUID, kind, key string) (ExecutionControl, bool, error) {
	var r ExecutionControl
	err := tx.QueryRow(ctx, `SELECT id,computer_id,kind,status,error_code,accepted_at,settled_at FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 AND caller_kind=$3 AND caller_id=$4 AND kind=$5 AND idempotency_key=$6`, env, id, callerKind, caller, kind, key).Scan(&r.ID, &r.ComputerID, &r.Kind, &r.Status, &r.ErrorCode, &r.AcceptedAt, &r.SettledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// RequestExecutionControl records intent after the orchestrator has checked all
// members and admission. The Computer lock serializes it with capture and input.
func RequestExecutionControl(ctx context.Context, tx pgx.Tx, env, id uuid.UUID, callerKind string, caller uuid.UUID, kind, key string, complete bool) (ExecutionControl, error) {
	r := ExecutionControl{ID: uuid.NewV7(), ComputerID: id, Kind: kind, Status: "accepted"}
	if complete {
		r.Status = "completed"
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 AND status='accepted')`, env, id).Scan(&pending); err != nil {
		return r, err
	}
	if pending {
		return r, ErrBusy
	}
	err := tx.QueryRow(ctx, `INSERT INTO computer_controls(environment_id,computer_id,id,caller_kind,caller_id,kind,idempotency_key,status,deadline_at,settled_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp()+(SELECT preparation_timeout_ms FROM environments WHERE id=$1)*interval '1 millisecond',CASE WHEN $8='completed' THEN clock_timestamp() END) RETURNING accepted_at,settled_at`, env, id, r.ID, callerKind, caller, kind, key, r.Status).Scan(&r.AcceptedAt, &r.SettledAt)
	if err != nil {
		return ExecutionControl{}, err
	}
	return r, notifyComputerChanged(ctx, tx, env, id)
}

// BindHibernateCapture attaches the accepted operation to its exact capture.
// Automatic capture has no public operation receipt.
func BindHibernateCapture(ctx context.Context, tx pgx.Tx, env, id, checkpoint uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE computer_controls SET checkpoint_id=$3 WHERE environment_id=$1 AND computer_id=$2 AND kind='hibernate' AND status='accepted' AND checkpoint_id IS NULL`, env, id, checkpoint)
	return err
}

func settleHibernateCapture(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, checkpoint uuid.UUID, code string) error {
	rows, err := tx.Query(ctx, `UPDATE computer_controls SET status=CASE WHEN $3='' THEN 'completed' ELSE 'failed' END,error_code=NULLIF($3,''),settled_at=clock_timestamp() WHERE environment_id=$1 AND checkpoint_id=$2 AND kind='hibernate' AND status='accepted' RETURNING environment_id,computer_id,id,kind,status,caller_kind,caller_id`, env, checkpoint, code)
	if err != nil {
		return err
	}
	return auditControlSettlements(ctx, tx, auditPolicy, rows)
}

// NoteExecutionDemand cancels an uncompleted explicit sleep intent. This joins
// the admission transaction, so rejected or duplicate input cannot wake a VM.
func NoteExecutionDemand(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, id uuid.UUID) error {
	if err := RequireExecutionCapacity(ctx, tx, env, id); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `UPDATE computer_controls SET status='failed',error_code='new_execution_requested',settled_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2 AND kind='hibernate' AND status='accepted' RETURNING environment_id,computer_id,id,kind,status,caller_kind,caller_id`, env, id)
	if err != nil {
		return err
	}
	return auditControlSettlements(ctx, tx, auditPolicy, rows)
}

var ErrExecutionSettling = errors.New("previous execution deadline elapsed; retry after its pending work settles")

var ErrExecutionLimit = errors.New("environment execution limit reached; raise the limit or release an unused Computer")

// RequireExecutionCapacity checks new execution under the Environment admission
// lock. An existing allocation already owns its entitlement, so follow-up input
// does not need room for another VM.
func RequireExecutionCapacity(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	var allocated bool
	var cpu, memory int64
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL AND NOT EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=l.environment_id AND cp.computer_id=l.computer_id AND cp.source_lease_epoch=l.epoch AND cp.status='ready')),
 COALESCE((SELECT l.reserved_cpu_millis FROM checkpoints cp JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(cp.environment_id,cp.computer_id,cp.source_lease_epoch) WHERE cp.environment_id=c.environment_id AND cp.computer_id=c.id AND cp.status='ready'),(c.cpu_millis+999)/1000*1000),
 COALESCE((SELECT l.reserved_memory_bytes FROM checkpoints cp JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(cp.environment_id,cp.computer_id,cp.source_lease_epoch) WHERE cp.environment_id=c.environment_id AND cp.computer_id=c.id AND cp.status='ready'),c.memory_bytes)
 FROM computers c WHERE c.environment_id=$1 AND c.id=$2`, env, id).Scan(&allocated, &cpu, &memory)
	if err != nil {
		return err
	}
	var pending, expired bool
	if err = tx.QueryRow(ctx, `SELECT execution_deadline_at IS NOT NULL,COALESCE(execution_deadline_at<=clock_timestamp(),false) FROM computers WHERE environment_id=$1 AND id=$2`, env, id).Scan(&pending, &expired); err != nil {
		return err
	}
	if expired {
		var unsettled bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND computer_id=$2 AND status='queued') OR EXISTS(SELECT 1 FROM commands WHERE environment_id=$1 AND computer_id=$2 AND status='pending') OR EXISTS(SELECT 1 FROM computer_controls WHERE environment_id=$1 AND computer_id=$2 AND status='accepted')`, env, id).Scan(&unsettled); err != nil {
			return err
		}
		if unsettled {
			return ErrExecutionSettling
		}
		pending = false
	}
	if allocated {
		if expired {
			return ErrExecutionSettling
		}
		return nil
	}
	if pending {
		return nil
	}
	budget, err := db.ReadExecutionBudgetForComputer(ctx, tx, env, id)
	if err != nil {
		return err
	}
	if !budget.Fits(cpu, memory, true) {
		return ErrExecutionLimit
	}
	before, err := workergroup.ReadExecutionPlacement(ctx, tx, env)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE computers SET execution_deadline_at=clock_timestamp()+(SELECT preparation_timeout_ms FROM environments WHERE id=$1)*interval '1 millisecond' WHERE environment_id=$1 AND id=$2`, env, id); err != nil {
		return err
	}
	return workergroup.RequireExecutionPlacement(ctx, tx, env, id, before)
}

// RequireRetainedHostCapacity runs under the Host lock before a paused source
// becomes mandatory again. New real grants may have claimed its optional room.
func RequireRetainedHostCapacity(ctx context.Context, tx pgx.Tx, env, id uuid.UUID, epoch int64) error {
	var fits bool
	err := tx.QueryRow(ctx, `SELECT
 COALESCE(sum(a.reserved_cpu_millis),0)+l.reserved_cpu_millis<=h.epoch_cpu_millis
 AND COALESCE(sum(a.reserved_memory_bytes),0)+l.reserved_memory_bytes<=h.epoch_memory_bytes
 AND COALESCE(sum(a.reserved_scratch_bytes),0)+l.reserved_scratch_bytes<=h.epoch_guest_ephemeral_disk_bytes
 AND count(a.allocation_id)+1<=h.max_vm_slots
 FROM computer_leases l JOIN worker_hosts h ON h.id=l.worker_host_id
 LEFT JOIN (`+db.AllocationUsageSQL+`) a ON a.worker_host_id=h.id AND a.kind NOT IN ('seed','retained')
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3
 GROUP BY l.environment_id,l.computer_id,l.epoch,h.id`, env, id, epoch).Scan(&fits)
	if err != nil {
		return err
	}
	if !fits {
		return ErrNotReady
	}
	return nil
}

// FailExecutionAdmission settles pending receipts with the provisioning episode.
func FailExecutionAdmission(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, id uuid.UUID) error {
	rows, err := tx.Query(ctx, `UPDATE computer_controls SET status='failed',error_code='execution_deadline_exceeded',settled_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2 AND status='accepted' RETURNING environment_id,computer_id,id,kind,status,caller_kind,caller_id`, env, id)
	if err != nil {
		return err
	}
	return auditControlSettlements(ctx, tx, auditPolicy, rows)
}

// RunnableExecutionDemandSQL assumes a Computer row c. Held Turns do not ask for
// execution; explicit wake and Commands have independent Computer lifetimes.
const RunnableExecutionDemandSQL = `(EXISTS(SELECT 1 FROM turns t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.session_id)
 WHERE t.environment_id=c.environment_id AND t.computer_id=c.id AND t.status='queued' AND ` + db.NoSessionHoldsSQL + `)
 OR EXISTS(SELECT 1 FROM commands cmd WHERE cmd.environment_id=c.environment_id AND cmd.computer_id=c.id AND cmd.status='pending')
 OR EXISTS(SELECT 1 FROM computer_controls r WHERE r.environment_id=c.environment_id AND r.computer_id=c.id AND r.kind='wake' AND r.status='accepted'))`

// RetireUnusedExecutionAdmission releases an unallocated reservation when its
// last runnable demand is held or cancelled. The caller owns the Computer lock.
func RetireUnusedExecutionAdmission(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE computers c SET execution_deadline_at=NULL
 WHERE c.environment_id=$1 AND c.id=$2 AND c.execution_deadline_at IS NOT NULL
 AND NOT `+RunnableExecutionDemandSQL+`
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND l.fenced_at IS NULL AND NOT EXISTS(SELECT 1 FROM checkpoints cp WHERE cp.environment_id=l.environment_id AND cp.computer_id=l.computer_id AND cp.source_lease_epoch=l.epoch AND cp.status='ready'))`, env, id)
	return err
}

// auditControlSettlements records receipt outcomes, not a claim that a physical
// transition rolled back or that every process stopped. Only changed receipts
// are returned by the owner UPDATE, so retries do not recreate expired facts.
func auditControlSettlements(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, rows pgx.Rows) error {
	type settlement struct {
		environment, computer, request uuid.UUID
		kind, status, callerKind       string
		caller                         uuid.UUID
	}
	results, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (settlement, error) {
		var r settlement
		err := row.Scan(&r.environment, &r.computer, &r.request, &r.kind, &r.status, &r.callerKind, &r.caller)
		return r, err
	})
	if err != nil {
		return err
	}
	for _, r := range results {
		outcome, reason := "succeeded", ""
		if r.status == "failed" {
			outcome, reason = "failed", "operation_failed"
		}
		if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: audit.Internal(audit.ControlPlane), EnvironmentID: r.environment, ComputerID: r.computer, Action: "computer." + r.kind + "_settled", Outcome: outcome, TargetKind: "computer", TargetID: r.computer, Causality: audit.Causality{InitiatorKind: r.callerKind, InitiatorID: r.caller}, Details: audit.ComputerControlDetails{RequestID: r.request, Reason: reason}}); err != nil {
			return err
		}
	}
	return nil
}
