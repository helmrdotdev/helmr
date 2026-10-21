package deployment

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type preparationSpec struct {
	id     uuid.UUID
	maxAge *int64
}

func preparationSpecs(ctx context.Context, q db.DBTX, env, deployment uuid.UUID) ([]preparationSpec, error) {
	rows, err := q.Query(ctx, `SELECT image_spec_id,min(max_image_age_ms) FROM template_definitions
 WHERE environment_id=$1 AND deployment_id=$2 AND prepare GROUP BY image_spec_id ORDER BY image_spec_id`, env, deployment)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (preparationSpec, error) {
		var spec preparationSpec
		err := row.Scan(&spec.id, &spec.maxAge)
		return spec, err
	})
}

func preparationState(ctx context.Context, q db.DBTX, env, deployment uuid.UUID) (string, error) {
	specs, err := preparationSpecs(ctx, q, env, deployment)
	if err != nil || len(specs) == 0 {
		return "", err
	}
	state := "ready"
	for _, spec := range specs {
		current, err := computer.ImagePreparationState(ctx, q, env, spec.id, spec.maxAge)
		if err != nil {
			return "", err
		}
		if current == "failed" || state != "failed" && (current == "unavailable" || state != "unavailable" && (current == "unprepared" || state == "ready")) {
			state = current
		}
	}
	return state, nil
}

// preparePromotion queues missing initial preparation without changing current.
// The caller owns the Environment lock. Acquire every Secret before any spec;
// the final readiness check and promotion share these locks with rotation.
func preparePromotion(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID) (bool, error) {
	specs, err := preparationSpecs(ctx, tx, env, deployment)
	if err != nil {
		return false, err
	}
	for _, spec := range specs {
		if err := computer.LockImageSpecSecrets(ctx, tx, env, spec.id); err != nil {
			return false, err
		}
	}
	for _, spec := range specs {
		if err := computer.LockImageSpec(ctx, tx, env, spec.id); err != nil {
			return false, err
		}
	}
	allReady := true
	for _, spec := range specs {
		ready, err := computer.ImageReady(ctx, tx, env, spec.id, spec.maxAge)
		if err != nil {
			return false, err
		}
		if !ready {
			allReady = false
			if err := computer.StartDeploymentImage(ctx, tx, env, spec.id, deployment); err != nil {
				return false, err
			}
		}
	}
	return allReady, nil
}
