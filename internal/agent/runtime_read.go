package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/jackc/pgx/v5"
)

type RuntimeTurnObservation struct {
	TurnView
	WaitBlocked string
}

// RuntimeObserveTurn reads one attributable outcome under the current caller's
// execution authority. It does not grant the caller the target's unrelated history.
func RuntimeObserveTurn(ctx context.Context, pool db.TxBeginner, caller Caller, session, turn uuid.UUID) (RuntimeTurnObservation, error) {
	var result RuntimeTurnObservation
	if caller.Kind != "session" || session == uuid.Nil() || turn == uuid.Nil() {
		return result, ErrDenied
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := LockRuntimeCaller(ctx, tx, caller); err != nil {
			return err
		}
		var err error
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
            WHERE s.environment_id=$1 AND s.id=$2 AND t.id=$3
              AND (s.parent_session_id=$4 OR s.requester_session_id=$4 OR (t.caller_kind='session' AND t.caller_id=$4)))`, caller.Execution.EnvironmentID, session, turn, caller.ID).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrDenied
		}
		result.TurnView, err = scanTurn(tx.QueryRow(ctx, `SELECT `+turnViewColumns+` FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3`, caller.Execution.EnvironmentID, session, turn))
		// Input and the target's event history are outside outcome observation.
		result.Input = nil
		if err != nil {
			return err
		}
		if result.Status == "queued" {
			blocked, err := runtimeWaitRetainsCapacity(ctx, tx, caller.Execution, session, turn)
			if err != nil {
				return err
			}
			if blocked {
				result.WaitBlocked = "capacity_wait_blocked"
			}
		}
		return nil
	})
	return result, hideMissing(err)
}

// Diagnose only an Environment limit that the caller's own retained allocation
// already makes impossible for this distinct target. Other workloads ending,
// another Host arriving or a transient full pool are not assumed to be dependencies.
func runtimeWaitRetainsCapacity(ctx context.Context, tx pgx.Tx, e Execution, session, turn uuid.UUID) (bool, error) {
	var resources []byte
	var ownCPU, ownMemory int64
	var maxResident, maxCPU, maxMemory *int64
	err := tx.QueryRow(ctx, `WITH RECURSIVE ancestors AS (
        SELECT id,parent_session_id FROM sessions WHERE environment_id=$1 AND id=$2
        UNION ALL SELECT s.id,s.parent_session_id FROM sessions s JOIN ancestors a ON s.id=a.parent_session_id WHERE s.environment_id=$1
    ) SELECT target.resources,own.reserved_cpu_millis,own.reserved_memory_bytes,env.max_resident_computers,env.max_cpu_millis,env.max_memory_bytes
      FROM sessions s JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
      JOIN computers target ON (target.environment_id,target.id)=(s.environment_id,s.computer_id)
      JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
      JOIN environments env ON env.id=s.environment_id
      JOIN session_processes p ON p.environment_id=$1 AND p.session_id=$4 AND p.epoch=$5
      JOIN computer_leases own ON (own.environment_id,own.computer_id,own.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
      WHERE s.environment_id=$1 AND s.id=$2 AND t.id=$3 AND t.status='queued' AND s.status IN ('open','closing')
        AND target.id<>own.computer_id AND target.resources IS NOT NULL
        AND target.preparation_failed_at IS NULL AND target.integrity_fault_at IS NULL AND target.deleted_at IS NULL
        AND d.execution_revoked_at IS NULL
        AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=$1 AND revoked.computer_id=target.id)
        AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=$1 AND l.computer_id=target.id AND l.fenced_at IS NULL)
        AND NOT EXISTS(SELECT 1 FROM session_holds h JOIN ancestors a ON a.id=h.session_id WHERE h.environment_id=$1 AND h.released_at IS NULL AND (h.session_id=$2 OR h.scope='subtree'))`, e.EnvironmentID, session, turn, e.SessionID, e.ProcessEpoch).Scan(&resources, &ownCPU, &ownMemory, &maxResident, &maxCPU, &maxMemory)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var requested definition.ResourcesManifest
	if err := json.Unmarshal(resources, &requested); err != nil {
		return false, err
	}
	if definition.ValidateResourcesManifest(requested) != nil || requested.MemoryMiB > math.MaxInt64/(1<<20) {
		return false, ErrInvalidInput
	}
	cpu, err := vm.ReservedCPUMillis(requested.MilliCPU)
	if err != nil {
		return false, err
	}
	exceeds := func(own, added int64, limit *int64) bool {
		return limit != nil && added > 0 && added <= *limit && own > *limit-added
	}
	return exceeds(1, 1, maxResident) || exceeds(ownCPU, cpu, maxCPU) || exceeds(ownMemory, requested.MemoryMiB*(1<<20), maxMemory), nil
}
