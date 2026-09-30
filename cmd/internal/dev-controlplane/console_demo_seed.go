package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

const (
	demoSeedOrgID         = "00000000-0000-7000-8000-000000000201"
	demoSeedProjectID     = "00000000-0000-7000-8000-000000000301"
	demoSeedEnvironmentID = "00000000-0000-7000-8000-000000000403"

	demoSeedDeploymentID        = "00000000-0000-7000-8000-000000000501"
	demoSeedProgramArtifactID   = "00000000-0000-7000-8000-000000000502"
	demoSeedComputerSpecID      = "00000000-0000-7000-8000-000000000504"
	demoSeedImageArtifactID     = "00000000-0000-7000-8000-000000000503"
	demoSeedTaskDefinitionID    = "00000000-0000-7000-8000-000000000511"
	demoSeedActorDefinitionID   = "00000000-0000-7000-8000-000000000512"
	demoSeedSandboxDefinitionID = "00000000-0000-7000-8000-000000000513"
	demoSeedScheduleID          = "00000000-0000-7000-8000-000000000521"

	demoSeedComputerActorID        = "00000000-0000-7000-8000-000000000601"
	demoSeedComputerTaskID         = "00000000-0000-7000-8000-000000000602"
	demoSeedComputerActorVersionID = "00000000-0000-7000-8000-000000000603"
	demoSeedComputerTaskVersionID  = "00000000-0000-7000-8000-000000000604"

	demoSeedSessionOpenID   = "00000000-0000-7000-8000-000000000701"
	demoSeedSessionFailedID = "00000000-0000-7000-8000-000000000702"

	demoSeedRunTaskSucceededID = "00000000-0000-7000-8000-000000000711"
	demoSeedRunTaskFailedID    = "00000000-0000-7000-8000-000000000712"
	demoSeedRunActorHistoryID  = "00000000-0000-7000-8000-000000000713"
	demoSeedRunActorFailedID   = "00000000-0000-7000-8000-000000000714"

	demoSeedSessionTurnID        = "00000000-0000-7000-8000-000000000721"
	demoSeedSessionTurnID2       = "00000000-0000-7000-8000-000000000722"
	demoSeedSessionOutputEventID = "00000000-0000-7000-8000-000000000723"

	demoSeedTokenPendingID   = "00000000-0000-7000-8000-000000000801"
	demoSeedTokenCompletedID = "00000000-0000-7000-8000-000000000802"

	demoSeedMarkerTag      = "demo-seed"
	demoSeedMarkerMetadata = `{"helmr.dev.demo_seed":true}`
)

func seedDemoEnvironmentData(ctx context.Context, tx pgx.Tx) error {
	taskManifest, taskDigest, err := manifestDigest(
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}},"schedule":{"cron":"0 9 * * 1-5","timezone":"UTC","computer":{"sandboxId":"demo-sandbox"}}}`,
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

	programDigest := demoDigest("demo-program")
	imageDigest := demoDigest("demo-computer-image")
	bundleDigest := demoDigest("demo-bundle")
	runtimeDigest := demoDigest("demo-runtime")
	queueConfig := `{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"},{"name":"priority"}]}`

	if _, err := tx.Exec(ctx, `
WITH lifetimes AS (
    INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, 1), ($3, 1)
    ON CONFLICT (digest) DO NOTHING
)
INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
VALUES
    ($1::uuid, $2, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
    ($1::uuid, $3, 1, 'application/vnd.helmr.computer.seed.v0+filepack')
`, demoSeedOrgID, programDigest, imageDigest); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO artifacts (
    id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type
) VALUES
    ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
    ($6::uuid, $2::uuid, $3::uuid, $4::uuid, $7, 'computer_image', 1, 'application/vnd.helmr.computer.seed.v0+filepack')
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
	manifest := definition.SandboxManifest{
		Image:     definition.SandboxImageManifest{Profile: definition.ComputerSeedProfile, ArtifactDigest: imageDigest, MediaType: definition.ComputerSeedMediaType},
		Resources: definition.ResourcesManifest{MilliCPU: 1000, MemoryMiB: 512},
	}
	spec, err := definition.CompileComputerSpec(manifest, definition.ComputerImage{
		Profile: definition.ComputerSeedProfile, Architecture: definition.ArchitectureX8664,
		Digest: imageDigest, MediaType: definition.ComputerSeedMediaType, SizeBytes: 1,
	})
	if err != nil {
		return err
	}
	rawManifest, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	sandboxManifest, sandboxDigest, err := manifestDigest(string(rawManifest))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO computer_specs(id,environment_id,config,digest,seed_artifact_id,seed_digest,seed_size_bytes,seed_media_type)
	 VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, demoSeedComputerSpecID, demoSeedEnvironmentID, spec.Config, spec.Digest[:], demoSeedImageArtifactID, spec.Seed.Digest, spec.Seed.SizeBytes, spec.Seed.MediaType); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO deployment_definitions (
    id, environment_id, deployment_id, kind, declared_id,
    manifest_version, manifest, manifest_digest, computer_spec_id
) VALUES
    ($1::uuid, $4::uuid, $5::uuid, 'task', 'demo-task', 0, $6::jsonb, $7, NULL),
    ($2::uuid, $4::uuid, $5::uuid, 'actor', 'demo-actor', 0, $8::jsonb, $9, NULL),
    ($3::uuid, $4::uuid, $5::uuid, 'sandbox', 'demo-sandbox', 0, $10::jsonb, $11, $12::uuid)
`, demoSeedTaskDefinitionID, demoSeedActorDefinitionID, demoSeedSandboxDefinitionID,
		demoSeedEnvironmentID, demoSeedDeploymentID,
		taskManifest, taskDigest, actorManifest, actorDigest, sandboxManifest, sandboxDigest, demoSeedComputerSpecID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO schedules (
    id, environment_id, task_declared_id,
    cron_pattern, timezone, status, effective_from
) VALUES (
    $1::uuid, $2::uuid, 'demo-task',
    '0 9 * * 1-5', 'UTC', 'archived', now() - interval '1 day'
)
`, demoSeedScheduleID, demoSeedEnvironmentID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO computers (
    id, environment_id, region_id, sandbox_declared_id,
    head_disk_version_id, key
, computer_spec_id, creation_deployment_id) VALUES
    ($1::uuid, $3::uuid, current_setting('helmr.seed_region_id'), 'demo-sandbox', $5::uuid, 'demo-actor', (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$3::uuid AND id=$4::uuid), (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$3::uuid AND id=$4::uuid)),
    ($2::uuid, $3::uuid, current_setting('helmr.seed_region_id'), 'demo-sandbox', $6::uuid, NULL, (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$3::uuid AND id=$4::uuid), (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$3::uuid AND id=$4::uuid))
`, demoSeedComputerActorID, demoSeedComputerTaskID, demoSeedEnvironmentID,
		demoSeedSandboxDefinitionID, demoSeedComputerActorVersionID, demoSeedComputerTaskVersionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO computer_disk_versions (
    id, environment_id, computer_id, root_pack_digest, status,
    writer_generation, published_at, logical_bytes
) VALUES
    ($1::uuid, $3::uuid, $4::uuid, NULL, 'initializing', 0, NULL, 0),
    ($2::uuid, $3::uuid, $5::uuid, NULL, 'initializing', 0, NULL, 0)
`, demoSeedComputerActorVersionID, demoSeedComputerTaskVersionID, demoSeedEnvironmentID,
		demoSeedComputerActorID, demoSeedComputerTaskID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO sessions (
    id, environment_id, actor_declared_id, deployment_definition_id, computer_id,
    current_run_id, next_input_sequence, committed_input_sequence, next_event_sequence,
    run_queue_name, run_max_active_duration_ms, run_retry_policy, run_metadata, run_tags,
    status
) VALUES
    (
        $1::uuid, $3::uuid, 'demo-actor', $4::uuid, $5::uuid,
        NULL, 3, 2, 6, 'default', 300000, '{"enabled":false}'::jsonb,
        $6::jsonb, ARRAY[$7::text], 'open'
    ),
    (
        $2::uuid, $3::uuid, 'demo-actor', $4::uuid, $5::uuid,
        NULL, 1, 0, 1, 'default', 300000, '{"enabled":false}'::jsonb,
        $6::jsonb, ARRAY[$7::text], 'open'
    )
`, demoSeedSessionOpenID, demoSeedSessionFailedID, demoSeedEnvironmentID,
		demoSeedActorDefinitionID, demoSeedComputerActorID, demoSeedMarkerMetadata, demoSeedMarkerTag); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO runs (
    id, org_id, project_id, environment_id, deployment_id, deployment_definition_id,
    entrypoint_kind, entrypoint_declared_id, cause_kind, session_id,
    session_input_start_sequence, session_input_high_watermark,
    computer_id, base_computer_disk_version_id, payload, status, revision, current_attempt_number,
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
		demoSeedComputerActorID, demoSeedComputerActorVersionID, demoSeedComputerTaskID, demoSeedComputerTaskVersionID,
		demoSeedMarkerMetadata, demoSeedMarkerTag, demoSeedActorDefinitionID,
		demoSeedSessionOpenID, demoSeedSessionFailedID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO run_attempts (
    run_id, number, entrypoint_kind, computer_id, base_computer_disk_version_id,
    entrypoint_entered_at, terminal_outcome, terminal_reason_code, terminal_at,
    session_input_start_sequence, terminal_session_input_sequence
) VALUES
    ($1::uuid, 1, 'task', $5::uuid, $6::uuid, now() - interval '115 minutes', 'succeeded', 'completed', now() - interval '110 minutes', NULL, NULL),
    ($2::uuid, 1, 'task', $7::uuid, $8::uuid, now() - interval '78 minutes', 'failed', 'task_failed', now() - interval '75 minutes', NULL, NULL),
    ($3::uuid, 1, 'actor', $5::uuid, $6::uuid, now() - interval '48 minutes', 'succeeded', 'completed', now() - interval '45 minutes', 1, 1),
    ($4::uuid, 1, 'actor', $5::uuid, $6::uuid, now() - interval '18 minutes', 'failed', 'actor_failed', now() - interval '15 minutes', 2, 2)
`, demoSeedRunTaskSucceededID, demoSeedRunTaskFailedID, demoSeedRunActorHistoryID, demoSeedRunActorFailedID,
		demoSeedComputerActorID, demoSeedComputerActorVersionID,
		demoSeedComputerTaskID, demoSeedComputerTaskVersionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE sessions
   SET status = 'failed',
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
INSERT INTO session_turns (id, environment_id, session_id, sequence, data)
VALUES ($1::uuid, $3::uuid, $4::uuid, 1, '{"prompt":"Synthetic demo input"}'),
       ($2::uuid, $3::uuid, $4::uuid, 2, '{"prompt":"Follow-up demo input"}')
`, demoSeedSessionTurnID, demoSeedSessionTurnID2, demoSeedEnvironmentID, demoSeedSessionOpenID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO session_events (
    id, environment_id, session_id, computer_id, turn_id, sequence, kind, data, computer_disk_version_id
) VALUES
    ('00000000-0000-7000-8000-000000000731', $4::uuid, $5::uuid, $6::uuid, $1::uuid, 1, 'turn.enqueued', '{"input":{"prompt":"Synthetic demo input"}}', NULL),
    ($3::uuid, $4::uuid, $5::uuid, $6::uuid, $1::uuid, 2, 'output', '{"reply":"Synthetic demo output"}', NULL),
    ('00000000-0000-7000-8000-000000000724', $4::uuid, $5::uuid, $6::uuid, $1::uuid, 3, 'turn.completed', jsonb_build_object('computer_disk_version_id',$7::text), $7::uuid),
    ('00000000-0000-7000-8000-000000000732', $4::uuid, $5::uuid, $6::uuid, $2::uuid, 4, 'turn.enqueued', '{"input":{"prompt":"Follow-up demo input"}}', NULL),
    ('00000000-0000-7000-8000-000000000725', $4::uuid, $5::uuid, $6::uuid, $2::uuid, 5, 'turn.failed', jsonb_build_object('error', jsonb_build_object('message','Synthetic demo test failure'),'computer_disk_version_id',$7::text), $7::uuid)
`, demoSeedSessionTurnID, demoSeedSessionTurnID2, demoSeedSessionOutputEventID,
		demoSeedEnvironmentID, demoSeedSessionOpenID, demoSeedComputerActorID, demoSeedComputerActorVersionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE session_turns SET
    status = CASE WHEN sequence = 1 THEN 'completed' ELSE 'failed' END,
    run_generation = 1, run_id = $2::uuid, attempt_number = 1,
    terminal_event_id = CASE WHEN sequence = 1 THEN '00000000-0000-7000-8000-000000000724'::uuid ELSE '00000000-0000-7000-8000-000000000725'::uuid END
WHERE session_id = $1::uuid
`, demoSeedSessionOpenID, demoSeedRunActorHistoryID); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO tokens (
    id, org_id, project_id, environment_id, status, expires_at,
    callback_secret_fingerprint, metadata, tags, result, completed_at, completion_fingerprint
) VALUES
    (
        $1::uuid, $3::uuid, $4::uuid, $5::uuid, 'pending',
        now() + interval '10 years', decode(repeat('aa', 32), 'hex'),
        $6::jsonb, ARRAY[$7::text, 'demo-approval'], NULL, NULL, NULL
    ),
    (
        $2::uuid, $3::uuid, $4::uuid, $5::uuid, 'completed',
        now() + interval '10 years', decode(repeat('bb', 32), 'hex'),
        $6::jsonb, ARRAY[$7::text, 'demo-completed'],
        '{"approved":true}'::jsonb, now() - interval '1 hour', decode(repeat('cc', 32), 'hex')
    )
`, demoSeedTokenPendingID, demoSeedTokenCompletedID,
		demoSeedOrgID, demoSeedProjectID, demoSeedEnvironmentID, demoSeedMarkerMetadata, demoSeedMarkerTag); err != nil {
		return err
	}

	return nil
}

func manifestDigest(raw string) ([]byte, []byte, error) {
	canonical, digest, err := definition.CanonicalManifestAndDigest([]byte(raw))
	if err != nil {
		return nil, nil, err
	}
	return canonical, digest[:], nil
}

func demoDigest(label string) string {
	sum := sha256.Sum256([]byte("helmr.dev.console-demo-seed\x00" + label))
	return fmt.Sprintf("sha256:%x", sum)
}
