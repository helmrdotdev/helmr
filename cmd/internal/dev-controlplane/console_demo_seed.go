package main

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
)

const (
	demoSeedOrgID         = "00000000-0000-7000-8000-000000000201"
	demoSeedProjectID     = "00000000-0000-7000-8000-000000000301"
	demoSeedEnvironmentID = "00000000-0000-7000-8000-000000000403"

	demoSeedDeploymentID        = "00000000-0000-7000-8000-000000000501"
	demoSeedProgramArtifactID   = "00000000-0000-7000-8000-000000000502"
	demoSeedImageArtifactID     = "00000000-0000-7000-8000-000000000503"
	demoSeedTaskDefinitionID    = "00000000-0000-7000-8000-000000000511"
	demoSeedActorDefinitionID   = "00000000-0000-7000-8000-000000000512"
	demoSeedSandboxDefinitionID = "00000000-0000-7000-8000-000000000513"
	demoSeedScheduleID          = "00000000-0000-7000-8000-000000000521"

	demoSeedWorkspaceActorID        = "00000000-0000-7000-8000-000000000601"
	demoSeedWorkspaceTaskID         = "00000000-0000-7000-8000-000000000602"
	demoSeedWorkspaceActorVersionID = "00000000-0000-7000-8000-000000000603"
	demoSeedWorkspaceTaskVersionID  = "00000000-0000-7000-8000-000000000604"

	demoSeedSessionOpenID   = "00000000-0000-7000-8000-000000000701"
	demoSeedSessionFailedID = "00000000-0000-7000-8000-000000000702"

	demoSeedRunTaskSucceededID = "00000000-0000-7000-8000-000000000711"
	demoSeedRunTaskFailedID    = "00000000-0000-7000-8000-000000000712"
	demoSeedRunActorHistoryID  = "00000000-0000-7000-8000-000000000713"
	demoSeedRunActorFailedID   = "00000000-0000-7000-8000-000000000714"

	demoSeedSessionInputRecordID  = "00000000-0000-7000-8000-000000000721"
	demoSeedSessionInputRecordID2 = "00000000-0000-7000-8000-000000000722"
	demoSeedSessionOutputRecordID = "00000000-0000-7000-8000-000000000723"

	demoSeedTokenPendingID   = "00000000-0000-7000-8000-000000000801"
	demoSeedTokenCompletedID = "00000000-0000-7000-8000-000000000802"

	demoSeedMarkerTag      = "demo-seed"
	demoSeedMarkerMetadata = `{"helmr.dev.demo_seed":true}`
)

func seedDemoEnvironmentData(ctx context.Context, tx pgx.Tx) error {
	taskManifest, taskDigest, err := manifestDigest(
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * 1-5","timezone":"UTC","workspace":{"sandboxId":"demo-sandbox"}}}`,
	)
	if err != nil {
		return err
	}
	actorManifest, actorDigest, err := manifestDigest(
		`{"idleTimeoutMs":30000,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`,
	)
	if err != nil {
		return err
	}
	sandboxManifest, sandboxDigest, err := manifestDigest(`{}`)
	if err != nil {
		return err
	}

	programDigest := demoDigest("demo-program")
	imageDigest := demoDigest("demo-workspace-image")
	bundleDigest := demoDigest("demo-bundle")
	runtimeDigest := demoDigest("demo-runtime")
	queueConfig := `{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"},{"name":"priority"}]}`

	if _, err := tx.Exec(ctx, `
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES
    ($1::uuid, $2, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
    ($1::uuid, $3, 1, 'application/octet-stream')
`, demoSeedOrgID, programDigest, imageDigest); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES
    ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
    ($6::uuid, $2::uuid, $3::uuid, $4::uuid, $7, 'workspace_image', 1, 'application/octet-stream')
`, demoSeedProgramArtifactID, demoSeedOrgID, demoSeedProjectID, demoSeedEnvironmentID,
		programDigest, demoSeedImageArtifactID, imageDigest); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO deployments (
    id, org_id, project_id, environment_id, version, bundle_digest,
    runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
) VALUES (
    $1::uuid, $2::uuid, $3::uuid, $4::uuid, 'demo-v1', $5, $6, $7::uuid,
    decode(repeat('03', 32), 'hex'), $8::jsonb
)
`, demoSeedDeploymentID, demoSeedOrgID, demoSeedProjectID, demoSeedEnvironmentID,
		bundleDigest, runtimeDigest, demoSeedProgramArtifactID, queueConfig); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE environments
   SET current_deployment_id = $2::uuid
 WHERE id = $1::uuid
`, demoSeedEnvironmentID, demoSeedDeploymentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id,
    manifest_version, manifest, manifest_digest, artifact_id
) VALUES
    ($1::uuid, $4::uuid, $5::uuid, 'task', 'demo-task', 0, $6::jsonb, $7, NULL),
    ($2::uuid, $4::uuid, $5::uuid, 'actor', 'demo-actor', 0, $8::jsonb, $9, NULL),
    ($3::uuid, $4::uuid, $5::uuid, 'sandbox', 'demo-sandbox', 0, $10::jsonb, $11, $12::uuid)
`, demoSeedTaskDefinitionID, demoSeedActorDefinitionID, demoSeedSandboxDefinitionID,
		demoSeedEnvironmentID, demoSeedDeploymentID,
		taskManifest, taskDigest, actorManifest, actorDigest, sandboxManifest, sandboxDigest, demoSeedImageArtifactID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO schedules (
    id, environment_id, task_declared_id,
    cron_pattern, timezone, state, effective_from
) VALUES (
    $1::uuid, $2::uuid, 'demo-task',
    '0 9 * * 1-5', 'UTC', 'archived', now() - interval '1 day'
)
`, demoSeedScheduleID, demoSeedEnvironmentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO workspaces (
    id, environment_id, region_id, sandbox_declared_id, deployment_definition_id,
    head_version_id, key, owner_session_id
) VALUES
    ($1::uuid, $3::uuid, current_setting('helmr.seed_region_id'), 'demo-sandbox', $4::uuid, $5::uuid, 'demo-actor', $7::uuid),
    ($2::uuid, $3::uuid, current_setting('helmr.seed_region_id'), 'demo-sandbox', $4::uuid, $6::uuid, NULL, NULL)
`, demoSeedWorkspaceActorID, demoSeedWorkspaceTaskID, demoSeedEnvironmentID,
		demoSeedSandboxDefinitionID, demoSeedWorkspaceActorVersionID, demoSeedWorkspaceTaskVersionID,
		demoSeedSessionOpenID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO workspace_versions (
    id, environment_id, workspace_id, content_digest, state,
    ownership_generation, writer_generation, published_at, size_bytes, entry_count
) VALUES
    ($1::uuid, $3::uuid, $4::uuid, $5, 'committed', 0, 0, now() - interval '2 hours', 0, 0),
    ($2::uuid, $3::uuid, $6::uuid, $5, 'committed', 0, 0, now() - interval '90 minutes', 0, 0)
`, demoSeedWorkspaceActorVersionID, demoSeedWorkspaceTaskVersionID, demoSeedEnvironmentID,
		demoSeedWorkspaceActorID, workspace.CanonicalEmptyTreeDigest, demoSeedWorkspaceTaskID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO sessions (
    id, environment_id, actor_declared_id, deployment_definition_id, workspace_id,
    current_run_id, next_input_sequence, committed_input_sequence, next_output_sequence,
    run_queue_name, run_max_active_duration_ms, run_retry_policy, run_metadata, run_tags,
    state
) VALUES
    (
        $1::uuid, $3::uuid, 'demo-actor', $4::uuid, $5::uuid,
        NULL, 3, 2, 2, 'default', 300000, '{"enabled":false}'::jsonb,
        $6::jsonb, ARRAY[$7::text], 'open'
    ),
    (
        $2::uuid, $3::uuid, 'demo-actor', $4::uuid, $5::uuid,
        NULL, 2, 1, 1, 'default', 300000, '{"enabled":false}'::jsonb,
        $6::jsonb, ARRAY[$7::text], 'open'
    )
`, demoSeedSessionOpenID, demoSeedSessionFailedID, demoSeedEnvironmentID,
		demoSeedActorDefinitionID, demoSeedWorkspaceActorID, demoSeedMarkerMetadata, demoSeedMarkerTag); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id, deployment_definition_id,
    entrypoint_kind, entrypoint_declared_id, cause_kind, session_id,
    session_input_start_sequence, session_input_high_watermark,
    workspace_id, base_workspace_version_id, payload, status, state_version, current_attempt_number,
    metadata, tags, queue_name, queue_origin_at, queue_score_at, max_active_duration_ms,
    retry_policy, trace_id, root_span_id, terminal_at, output, failure
) VALUES
    (
        $1::uuid, $5::uuid, $6::uuid, $7::uuid, $8::uuid, $9::uuid,
        'task', 'demo-task', 'api', NULL, NULL, NULL,
        $10::uuid, $11::uuid, '{"message":"Synthetic demo task payload"}'::jsonb,
        'succeeded', 2, 1, $14::jsonb, ARRAY[$15::text], 'default',
        now() - interval '2 hours', now() - interval '2 hours', 300000,
        '{"enabled":false}'::jsonb, '11111111111111111111111111111111', '2222222222222222',
        now() - interval '110 minutes', '{"result":"ok"}'::jsonb, NULL
    ),
    (
        $2::uuid, $5::uuid, $6::uuid, $7::uuid, $8::uuid, $9::uuid,
        'task', 'demo-task', 'manual', NULL, NULL, NULL,
        $12::uuid, $13::uuid, '{"message":"Synthetic demo failure"}'::jsonb,
        'failed', 2, 1, $14::jsonb, ARRAY[$15::text], 'default',
        now() - interval '80 minutes', now() - interval '80 minutes', 300000,
        '{"enabled":false}'::jsonb, '33333333333333333333333333333333', '4444444444444444',
        now() - interval '75 minutes', NULL,
        '{"code":"task_failed","message":"Synthetic demo failure","details":{}}'::jsonb
    ),
    (
        $3::uuid, $5::uuid, $6::uuid, $7::uuid, $8::uuid, $16::uuid,
        'actor', 'demo-actor', 'actor_start', $17::uuid, 1, 1,
        $10::uuid, $11::uuid, NULL,
        'succeeded', 2, 1, $14::jsonb, ARRAY[$15::text], 'default',
        now() - interval '50 minutes', now() - interval '50 minutes', 300000,
        '{"enabled":false}'::jsonb, '55555555555555555555555555555555', '6666666666666666',
        now() - interval '45 minutes', NULL, NULL
    ),
    (
        $4::uuid, $5::uuid, $6::uuid, $7::uuid, $8::uuid, $16::uuid,
        'actor', 'demo-actor', 'continuation', $18::uuid, 2, 2,
        $10::uuid, $11::uuid, NULL,
        'failed', 2, 1, $14::jsonb, ARRAY[$15::text], 'default',
        now() - interval '20 minutes', now() - interval '20 minutes', 300000,
        '{"enabled":false}'::jsonb, '77777777777777777777777777777777', '8888888888888888',
        now() - interval '15 minutes', NULL,
        '{"code":"actor_failed","message":"Synthetic demo actor failure","details":{}}'::jsonb
    )
`, demoSeedRunTaskSucceededID, demoSeedRunTaskFailedID, demoSeedRunActorHistoryID, demoSeedRunActorFailedID,
		demoSeedOrgID, demoSeedProjectID, demoSeedEnvironmentID, demoSeedDeploymentID, demoSeedTaskDefinitionID,
		demoSeedWorkspaceActorID, demoSeedWorkspaceActorVersionID, demoSeedWorkspaceTaskID, demoSeedWorkspaceTaskVersionID,
		demoSeedMarkerMetadata, demoSeedMarkerTag, demoSeedActorDefinitionID,
		demoSeedSessionOpenID, demoSeedSessionFailedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, workspace_id, base_workspace_version_id,
    entrypoint_entered_at, terminal_outcome, terminal_reason_code, terminal_at,
    session_input_start_sequence, terminal_session_input_sequence
) VALUES
    ($1::uuid, 1, 'task', $5::uuid, $6::uuid, now() - interval '115 minutes', 'succeeded', 'completed', now() - interval '110 minutes', NULL, NULL),
    ($2::uuid, 1, 'task', $7::uuid, $8::uuid, now() - interval '78 minutes', 'failed', 'task_failed', now() - interval '75 minutes', NULL, NULL),
    ($3::uuid, 1, 'actor', $5::uuid, $6::uuid, now() - interval '48 minutes', 'succeeded', 'completed', now() - interval '45 minutes', 1, 1),
    ($4::uuid, 1, 'actor', $5::uuid, $6::uuid, now() - interval '18 minutes', 'failed', 'actor_failed', now() - interval '15 minutes', 2, 2)
`, demoSeedRunTaskSucceededID, demoSeedRunTaskFailedID, demoSeedRunActorHistoryID, demoSeedRunActorFailedID,
		demoSeedWorkspaceActorID, demoSeedWorkspaceActorVersionID,
		demoSeedWorkspaceTaskID, demoSeedWorkspaceTaskVersionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE sessions
   SET state = 'failed',
       failure = jsonb_build_object(
           'code', 'actor_failed',
           'message', 'Synthetic demo session failure',
           'details', jsonb_build_object('run_id', $2::text)
       ),
       failure_run_id = $2::uuid,
       failed_at = now() - interval '15 minutes'
 WHERE id = $1::uuid
`, demoSeedSessionFailedID, demoSeedRunActorFailedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO session_records (
    id, environment_id, session_id, direction, sequence, data,
    source_run_id, producer_run_id, producer_attempt_number
) VALUES
    ($1::uuid, $4::uuid, $5::uuid, 'input', 1, '{"prompt":"Synthetic demo input"}'::jsonb, NULL, NULL, NULL),
    ($2::uuid, $4::uuid, $5::uuid, 'input', 2, '{"prompt":"Follow-up demo input"}'::jsonb, NULL, NULL, NULL),
    ($3::uuid, $4::uuid, $5::uuid, 'output', 1, '{"reply":"Synthetic demo output"}'::jsonb, NULL, $6::uuid, 1)
`, demoSeedSessionInputRecordID, demoSeedSessionInputRecordID2, demoSeedSessionOutputRecordID,
		demoSeedEnvironmentID, demoSeedSessionOpenID, demoSeedRunActorHistoryID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO tokens (
    id, org_id, project_id, environment_id, state, expires_at,
    callback_secret_fingerprint, metadata, tags, result, completed_at
) VALUES
    (
        $1::uuid, $3::uuid, $4::uuid, $5::uuid, 'pending',
        now() + interval '10 years', decode(repeat('aa', 32), 'hex'),
        $6::jsonb, ARRAY[$7::text, 'demo-approval'], NULL, NULL
    ),
    (
        $2::uuid, $3::uuid, $4::uuid, $5::uuid, 'completed',
        now() + interval '10 years', decode(repeat('bb', 32), 'hex'),
        $6::jsonb, ARRAY[$7::text, 'demo-completed'],
        '{"approved":true}'::jsonb, now() - interval '1 hour'
    )
`, demoSeedTokenPendingID, demoSeedTokenCompletedID,
		demoSeedOrgID, demoSeedProjectID, demoSeedEnvironmentID, demoSeedMarkerMetadata, demoSeedMarkerTag); err != nil {
		return err
	}

	return nil
}

func manifestDigest(raw string) ([]byte, []byte, error) {
	canonical, digest, err := deployment.CanonicalManifestAndDigest([]byte(raw))
	if err != nil {
		return nil, nil, err
	}
	return canonical, digest[:], nil
}

func demoDigest(label string) string {
	sum := sha256.Sum256([]byte("helmr.dev.console-demo-seed\x00" + label))
	return fmt.Sprintf("sha256:%x", sum)
}
