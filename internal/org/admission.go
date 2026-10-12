package org

import (
	"context"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// AdmissionPolicy is an Environment's admission bounds, read under its
// admission lock.
type AdmissionPolicy struct {
	Deployment                                        *uuid.UUID
	MaxOutstanding                                    int64
	MaxResident                                       int64
	MaxCausal                                         int
	MaxCPU, MaxMemory, MaxStorage, PreparationTimeout int64
}

// LockAdmission takes the Environment admission lock (step 4 of the global
// order) and reads its bounds. A retired Environment reads as pgx.ErrNoRows.
func LockAdmission(ctx context.Context, tx pgx.Tx, env uuid.UUID) (AdmissionPolicy, error) {
	var p AdmissionPolicy
	err := tx.QueryRow(ctx, `SELECT current_deployment_id,max_outstanding_admissions,max_causal_depth,
 max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,max_resident_computers
 FROM environments WHERE id=$1 AND retired_at IS NULL FOR NO KEY UPDATE`, env).Scan(&p.Deployment, &p.MaxOutstanding, &p.MaxCausal, &p.MaxCPU, &p.MaxMemory, &p.MaxStorage, &p.PreparationTimeout, &p.MaxResident)
	return p, err
}

// ChargeAdmission takes one token from the Environment's admission bucket and
// reports whether one was available. The caller holds the admission lock and
// owns rollback; server time never moves the refill point back.
func ChargeAdmission(ctx context.Context, tx pgx.Tx, env uuid.UUID) (bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `WITH refill AS (
 SELECT id,GREATEST(clock_timestamp(),admission_refilled_at) AS at,admission_burst,admission_tokens,admission_rate_per_second,admission_refilled_at
 FROM environments WHERE id=$1
 ), available AS (
 SELECT id,at,LEAST(admission_burst::numeric,admission_tokens+admission_rate_per_second*extract(epoch FROM at-admission_refilled_at)) AS tokens FROM refill
 ) UPDATE environments e SET admission_tokens=a.tokens-1,admission_refilled_at=a.at FROM available a
 WHERE e.id=a.id AND a.tokens>=1 RETURNING e.id`, env).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
