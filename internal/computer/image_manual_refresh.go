package computer

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// StartTemplateRefresh reuses ordinary build admission while requiring any
// already exposed build Secrets to still match current versions. The caller
// holds the Environment, Secret and spec locks through its receipt commit.
func StartTemplateRefresh(ctx context.Context, tx pgx.Tx, env, spec, deployment uuid.UUID) (uuid.UUID, error) {
	if err := StartDeploymentImage(ctx, tx, env, spec, deployment); err != nil {
		return uuid.Nil(), err
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT p.id FROM image_builds p WHERE p.environment_id=$1 AND p.image_spec_id=$2 AND `+usableImageBuildSQL+`
 AND NOT EXISTS (SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
 WHERE x.environment_id=p.environment_id AND x.image_build_id=p.id AND (s.status<>'active' OR s.current_version_id<>x.version_id))`, env, spec).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil(), ErrNotReady
	}
	return id, err
}

type ImageBuildState struct {
	Build     ImageBuild
	Ready     bool
	ErrorCode string
}

// ReadImageBuildState observes one build and that exact build's image eligibility
// in one snapshot. An older eligible image cannot make a failed refresh ready.
func ReadImageBuildState(ctx context.Context, q db.DBTX, env, id uuid.UUID, maxAge *int64) (ImageBuildState, error) {
	result := ImageBuildState{Build: ImageBuild{EnvironmentID: env, ID: id}}
	err := q.QueryRow(ctx, `SELECT p.image_spec_id,p.status,p.deadline_at,CASE WHEN p.status IN ('queued','running') AND p.deadline_at<=clock_timestamp() THEN 'deadline_exceeded' ELSE COALESCE(p.error_code,'') END,
 EXISTS(SELECT 1 FROM images i WHERE i.environment_id=p.environment_id AND i.image_build_id=p.id AND i.root_id IS NOT NULL
 AND p.status='succeeded' AND ($3::bigint IS NULL OR extract(epoch FROM clock_timestamp()-i.published_at)*1000<=$3)
 AND `+usableImageSecretsSQL+`)
 FROM image_builds p JOIN image_specs spec ON (spec.environment_id,spec.id)=(p.environment_id,p.image_spec_id)
 WHERE p.environment_id=$1 AND p.id=$2`, env, id, maxAge).Scan(&result.Build.SpecID, &result.Build.Status, &result.Build.Deadline, &result.ErrorCode, &result.Ready)
	return result, err
}
