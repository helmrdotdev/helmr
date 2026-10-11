package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

type admissionEnvironment struct {
	deployment                                        *uuid.UUID
	maxOutstanding                                    int64
	maxResident                                       int64
	maxCausal                                         int
	maxCPU, maxMemory, maxStorage, preparationTimeout int64
	configured                                        bool
}

func lockAdmissionEnvironment(ctx context.Context, tx pgx.Tx, env uuid.UUID) (admissionEnvironment, error) {
	var p admissionEnvironment
	err := tx.QueryRow(ctx, `SELECT current_deployment_id,COALESCE(max_outstanding_admissions,0),COALESCE(max_causal_depth,0),
 COALESCE(max_cpu_millis,0),COALESCE(max_memory_bytes,0),COALESCE(max_reserved_storage_bytes,0),COALESCE(preparation_timeout_ms,0),admission_rate_per_second IS NOT NULL,COALESCE(max_resident_computers,0)
 FROM environments WHERE id=$1 AND retired_at IS NULL FOR NO KEY UPDATE`, env).Scan(&p.deployment, &p.maxOutstanding, &p.maxCausal, &p.maxCPU, &p.maxMemory, &p.maxStorage, &p.preparationTimeout, &p.configured, &p.maxResident)
	return p, err
}

// One token belongs to each newly admitted Turn, including a keyed continuation.
// Historical receipts consume none. The enclosing transaction owns rollback of
// tokens, reservations and work; server time never moves the refill point back.
func consumeAdmission(ctx context.Context, tx pgx.Tx, env uuid.UUID, p admissionEnvironment) error {
	if !p.configured {
		return ErrNotReady
	}
	var outstanding int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM turns WHERE environment_id=$1 AND status IN ('queued','running','finalizing')`, env).Scan(&outstanding); err != nil {
		return err
	}
	if outstanding >= p.maxOutstanding {
		return ErrNotReady
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `WITH refill AS (
 SELECT id,GREATEST(clock_timestamp(),admission_refilled_at) AS at,admission_burst,admission_tokens,admission_rate_per_second,admission_refilled_at
 FROM environments WHERE id=$1
 ), available AS (
 SELECT id,at,LEAST(admission_burst::numeric,admission_tokens+admission_rate_per_second*extract(epoch FROM at-admission_refilled_at)) AS tokens FROM refill
 ) UPDATE environments e SET admission_tokens=a.tokens-1,admission_refilled_at=a.at FROM available a
 WHERE e.id=a.id AND a.tokens>=1 RETURNING e.id`, env).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotReady
	}
	return err
}
