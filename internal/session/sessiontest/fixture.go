// Package sessiontest builds Session test databases. Fixture is an
// environment whose current deployment declares the Actor operator.v1
// (queues default, with a concurrency limit of two, and priority), the Task
// resize-image and the sandbox computer.v1, and active Computers with
// committed heads that each bind the environment's active Secret API_TOKEN.
// Execution is an Actor execution of a worker on a run test database.
package sessiontest

import (
	"context"
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ActorDeclaredID is the Actor the fixture's deployment declares.
const ActorDeclaredID = "operator.v1"

// Fixture is a migrated database with one organization, project and
// environment, its current deployment and its Computers.
type Fixture struct {
	Pool          *pgxpool.Pool
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	DeploymentID  uuid.UUID
	ComputerIDs   []uuid.UUID
	ComputerKeys  []string
}

// New builds the fixture with computerCount Computers.
func New(t *testing.T, computerCount int) Fixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	pool := database.Pool
	fixture := Fixture{
		Pool: pool, OrgID: uuid.NewV7(), ProjectID: uuid.NewV7(),
		EnvironmentID: uuid.NewV7(), ComputerIDs: make([]uuid.UUID, computerCount),
		ComputerKeys: make([]string, computerCount),
	}
	deploymentID := uuid.NewV7()
	fixture.DeploymentID = deploymentID
	actorDefinitionID := uuid.NewV7()
	taskDefinitionID := uuid.NewV7()
	computerDefinitionID := uuid.NewV7()
	programID, imageID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO regions (id, display_name)
		VALUES ('us-east-1', 'Actor Start Test')
	`)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Actor Start Test', $2)
	`, fixture.OrgID, "actor-start-"+fixture.OrgID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, 'us-east-1', $3, 'Actor Start Test')
	`, fixture.ProjectID, fixture.OrgID, "actor-start-"+fixture.ProjectID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Actor Start Test', '#3366ff')
	`, fixture.EnvironmentID, fixture.OrgID, fixture.ProjectID,
		"actor-start-"+fixture.EnvironmentID.String())

	digests := []string{
		"sha256:" + fmt.Sprintf("%064x", 1),
		"sha256:" + fmt.Sprintf("%064x", 2),
		"sha256:" + fmt.Sprintf("%064x", 3),
		"sha256:" + fmt.Sprintf("%064x", 4),
	}
	actorManifest := []byte(
		`{"idleTimeoutMs":30000,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`,
	)
	taskManifest := []byte(
		`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`,
	)
	_, actorManifestDigest, err := definition.CanonicalManifestAndDigest(actorManifest)
	if err != nil {
		t.Fatal(err)
	}
	_, taskManifestDigest, err := definition.CanonicalManifestAndDigest(taskManifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig := []byte(
		`{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"},{"name":"priority"}]}`,
	)
	dbtest.MustExec(t, t.Context(), pool, `
		WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, 1), ($3, 1), ($4, 1), ($5, 1) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/vnd.helmr.deployment-bundle.v0+json'),
		       ($1, $3, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
		       ($1, $4, 1, 'application/vnd.helmr.computer.seed.v0+filepack'),
		       ($1, $5, 1, 'application/vnd.helmr.runtime.v0+squashfs')
	`, fixture.OrgID, digests[0], digests[1], digests[2], digests[3])
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $3, $4, $5, $6, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
		       ($2, $3, $4, $5, $7, 'computer_image', 1, 'application/vnd.helmr.computer.seed.v0+filepack')
	`, programID, imageID, fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, digests[1], digests[2])
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployments (
		    id, org_id, project_id, environment_id, version, bundle_digest,
		    runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
		) VALUES (
		    $1, $2, $3, $4, 'actor-start-test', $5, $6, $7,
		    decode(repeat('03', 32), 'hex'), $8::jsonb
		)
	`, deploymentID, fixture.OrgID, fixture.ProjectID,
		fixture.EnvironmentID, digests[0], digests[3], programID, queueConfig)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployment_definitions (
		    id, environment_id, deployment_id, kind, declared_id,
		    manifest_version, manifest, manifest_digest, computer_spec_id
		) VALUES
		    ($1, $4, $5, 'actor', 'operator.v1', 0, $7::jsonb, $8, NULL),
		    ($2, $4, $5, 'task', 'resize-image', 0, $9::jsonb, $10, NULL),
		    ($3, $4, $5, 'sandbox', 'computer.v1', 0, '{}'::jsonb, decode(repeat('04', 32), 'hex'), $6)
	`, actorDefinitionID, taskDefinitionID, computerDefinitionID,
		fixture.EnvironmentID, deploymentID, dbtest.InsertDefaultComputerSpec(t, t.Context(), pool, imageID),
		actorManifest, actorManifestDigest[:], taskManifest, taskManifestDigest[:])
	dbtest.MustExec(t, t.Context(), pool, `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, deploymentID, fixture.EnvironmentID)

	secretID, secretVersionID := uuid.NewV7(), uuid.NewV7()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `
		INSERT INTO secrets (id, environment_id, name, current_version_id)
		VALUES ($1, $2, 'API_TOKEN', $3)
	`, secretID, fixture.EnvironmentID, secretVersionID)
	dbtest.MustExec(t, t.Context(), tx, `
		INSERT INTO secret_versions (
		    id, secret_id, version, nonce, ciphertext
		) VALUES ($1, $2, 1, decode(repeat('01', 12), 'hex'),
		          decode(repeat('02', 16), 'hex'))
	`, secretVersionID, secretID)
	for index := range computerCount {
		computerID, versionID := uuid.NewV7(), uuid.NewV7()
		fixture.ComputerIDs[index] = computerID
		fixture.ComputerKeys[index] = fmt.Sprintf("computer:%d", index)
		dbtest.MustExec(t, t.Context(), tx, `
			INSERT INTO computers (
			    id, environment_id, region_id,
			    sandbox_declared_id, head_disk_version_id, key
			, computer_spec_id, creation_deployment_id) VALUES ($1, $2, 'us-east-1', 'computer.v1', $4, $5, (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$2 AND id=$3), (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$2 AND id=$3))
		`, computerID, fixture.EnvironmentID, computerDefinitionID, versionID,
			fixture.ComputerKeys[index])
		dbtest.InsertCommittedComputerRoot(t, t.Context(), tx, versionID, fixture.EnvironmentID, computerID)
		dbtest.MustExec(t, t.Context(), tx, `
			INSERT INTO computer_secrets (mode,
			    computer_id, environment_id, placement_kind, placement_target, secret_id
			) VALUES ('raw', $1, $2, 'env', 'API_TOKEN', $3)
		`, computerID, fixture.EnvironmentID, secretID)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return fixture
}
