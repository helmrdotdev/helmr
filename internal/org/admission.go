package org

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/executionlimit"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// LockAdmission takes the Environment admission lock (step 4 of the global
// order) and reads its current Deployment. A missing Environment reads as
// pgx.ErrNoRows.
func LockAdmission(ctx context.Context, tx pgx.Tx, env uuid.UUID) (*uuid.UUID, error) {
	var deployment *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, env).Scan(&deployment)
	return deployment, err
}

// ChargeAdmission takes one token from the Environment's admission bucket and
// reports whether one was available. The caller holds the admission lock and
// owns rollback; server time never moves the refill point back, and a lowered
// burst applies on the next charge.
func ChargeAdmission(ctx context.Context, tx pgx.Tx, limits executionlimit.Limits, env uuid.UUID) (bool, error) {
	l := limits.For(env)
	var id uuid.UUID
	err := tx.QueryRow(ctx, `WITH refill AS (
 SELECT id,GREATEST(clock_timestamp(),admission_refilled_at) AS at,admission_tokens,admission_refilled_at
 FROM environments WHERE id=$1
 ), available AS (
 SELECT id,at,LEAST($3::numeric,admission_tokens+$2::numeric*extract(epoch FROM at-admission_refilled_at)) AS tokens FROM refill
 ) UPDATE environments e SET admission_tokens=a.tokens-1,admission_refilled_at=a.at FROM available a
 WHERE e.id=a.id AND a.tokens>=1 RETURNING e.id`, env, l.AdmissionRatePerSecond, l.AdmissionBurst).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
