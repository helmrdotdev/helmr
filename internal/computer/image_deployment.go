package computer

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

const deploymentImageReadySQL = `SELECT EXISTS(SELECT 1 FROM images i
 JOIN image_builds p ON (p.environment_id,p.id)=(i.environment_id,i.image_build_id)
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(i.environment_id,i.image_spec_id)
 WHERE i.environment_id=$1 AND i.image_spec_id=$2 AND i.root_id IS NOT NULL AND p.status='succeeded'
 AND ($3::bigint IS NULL OR extract(epoch FROM clock_timestamp()-i.published_at)*1000<=$3)
 AND ` + usableImageSecretsSQL + `)`

// ImageReady uses the same age and current-Secret eligibility as a new Computer
// pin. A promotion caller holds the Environment, Secret and spec locks through
// its cutover; an observational caller must not use this result as authority.
func ImageReady(ctx context.Context, q db.DBTX, env, spec uuid.UUID, maxAge *int64) (bool, error) {
	var ready bool
	err := q.QueryRow(ctx, deploymentImageReadySQL, env, spec, maxAge).Scan(&ready)
	return ready, err
}

// StartDeploymentImage coalesces explicit promotion demand with any live build.
// Its caller has locked the Environment, declared Secrets and spec. A new
// explicit promotion may retry a failed build, but observation never does.
func StartDeploymentImage(ctx context.Context, tx pgx.Tx, env, spec, deployment uuid.UUID) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM secret_bindings b
 WHERE b.environment_id=$1 AND b.image_spec_id=$2 AND NOT EXISTS(
 SELECT 1 FROM secrets s WHERE s.environment_id=b.environment_id AND s.id=b.secret_id AND s.status='active'))`, env, spec).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrSecretUnavailable
	}
	var occupied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM image_builds
 WHERE environment_id=$1 AND image_spec_id=$2 AND (status IN ('queued','running') OR (worker_host_id IS NOT NULL AND fenced_at IS NULL)))`, env, spec).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return nil
	}
	// Existing attached Computers can consume only their original build or its
	// direct successor. Let that chain own the next build after Secret rotation.
	successorPending, err := deploymentImageSuccessorPending(ctx, tx, env, spec)
	if err != nil {
		return err
	}
	if successorPending {
		return nil
	}
	var timeout int64
	if err := tx.QueryRow(ctx, `SELECT preparation_timeout_ms FROM environments WHERE id=$1`, env).Scan(&timeout); err != nil {
		return err
	}
	id := uuid.NewV7()
	if _, err := tx.Exec(ctx, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,deadline_at)
 VALUES($1,$2,$3,$4,clock_timestamp()+$5::bigint*interval '1 millisecond')`, env, id, spec, "deployment:"+deployment.String()+":"+id.String(), timeout); err != nil {
		return err
	}
	return notifyImageBuildChanged(ctx, tx, env, id)
}

const deploymentImageSuccessorPendingSQL = `SELECT EXISTS(SELECT 1 FROM image_builds p
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(p.environment_id,p.image_spec_id)
 WHERE p.environment_id=$1 AND p.image_spec_id=$2
 AND NOT EXISTS(SELECT 1 FROM image_builds successor WHERE successor.environment_id=p.environment_id AND successor.successor_of=p.id)
 AND ` + imageBuildSuccessorEligibilitySQL + `)`

func deploymentImageSuccessorPending(ctx context.Context, q db.DBTX, env, spec uuid.UUID) (bool, error) {
	var pending bool
	err := q.QueryRow(ctx, deploymentImageSuccessorPendingSQL, env, spec).Scan(&pending)
	return pending, err
}

// ImagePreparationState observes readiness and build transitions in one database
// snapshot, so publication or successor creation cannot look unprepared between reads.
func ImagePreparationState(ctx context.Context, q db.DBTX, env, spec uuid.UUID, maxAge *int64) (string, error) {
	var ready, pending bool
	var status string
	var deadline time.Time
	var code *string
	err := q.QueryRow(ctx, `SELECT (`+deploymentImageReadySQL+`), (`+deploymentImageSuccessorPendingSQL+`), status,deadline_at,error_code FROM image_builds WHERE environment_id=$1 AND image_spec_id=$2 ORDER BY id DESC LIMIT 1`, env, spec, maxAge).Scan(&ready, &pending, &status, &deadline, &code)
	if errors.Is(err, pgx.ErrNoRows) {
		return "unprepared", nil
	}
	if err != nil {
		return "", err
	}
	if ready {
		return "ready", nil
	}
	if status == "failed" && code != nil && *code == "preparation_qualification_unavailable" {
		return "unavailable", nil
	}
	if status == "failed" || !deadline.After(time.Now()) && status != "succeeded" {
		return "failed", nil
	}
	if status == "queued" || status == "running" {
		return "preparing", nil
	}
	if pending {
		return "preparing", nil
	}
	return "unprepared", nil
}
