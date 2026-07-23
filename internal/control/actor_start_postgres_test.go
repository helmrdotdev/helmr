package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/keyedhash"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type actorStartPostgresFixture struct {
	pool          *pgxpool.Pool
	server        *Server
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	workspaceIDs  []uuid.UUID
}

func TestActorStartPostgresCommitsReplaysAndRejectsConflicts(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	key := "thread:42"
	request := fixture.request(0, &key, "start-1")
	request.InputPresent = true
	request.Input = json.RawMessage(`{"message":"hello"}`)
	request.Metadata = json.RawMessage(`{"owner":"test"}`)
	request.Tags = []string{"beta", "alpha"}
	request.ManagedRunMetadata = json.RawMessage(`{"kind":"boot"}`)
	request.ManagedRunTags = []string{"managed"}

	created, err := fixture.server.startActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed || created.InitialRecordID == nil {
		t.Fatalf("created = %+v", created)
	}
	assertActorStartTuple(t, fixture, created, 1)

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors
		   SET state = 'closing',
		       metadata = '{"changed":true}'::jsonb
		 WHERE id = $1
	`, created.ActorID); err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.server.startActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ActorID != created.ActorID ||
		replayed.BootRunID != created.BootRunID ||
		replayed.InitialRecordID == nil || *replayed.InitialRecordID != *created.InitialRecordID {
		t.Fatalf("replayed = %+v, created = %+v", replayed, created)
	}

	changed := request
	changed.Metadata = json.RawMessage(`{"owner":"different"}`)
	var idempotencyConflict idempotency.ConflictError
	if _, err := fixture.server.startActor(t.Context(), changed); !errors.As(err, &idempotencyConflict) {
		t.Fatalf("fingerprint conflict = %v", err)
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspaces SET dirty_state = 'dirty' WHERE id = $1
	`, fixture.workspaceIDs[1]); err != nil {
		t.Fatal(err)
	}
	keyCollision := fixture.request(1, &key, "start-2")
	_, err = fixture.server.startActor(t.Context(), keyCollision)
	var actorKeyConflict ActorKeyConflictError
	if !errors.As(err, &actorKeyConflict) {
		t.Fatalf("Actor key conflict = %v", err)
	}
	var claimCount, actorCount, runCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.start'),
		    (SELECT count(*) FROM actors),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start')
	`).Scan(&claimCount, &actorCount, &runCount); err != nil {
		t.Fatal(err)
	}
	if claimCount != 1 || actorCount != 1 || runCount != 1 {
		t.Fatalf("counts after conflicts = claims %d actors %d runs %d", claimCount, actorCount, runCount)
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspaces SET dirty_state = 'clean' WHERE id = $1
	`, fixture.workspaceIDs[1]); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(500 * time.Millisecond)
	expiringKey := "expiring"
	expiring := fixture.request(1, &expiringKey, "expiring-1")
	expiring.ExpiresAt = &expiresAt
	expiringCreated, err := fixture.server.startActor(t.Context(), expiring)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Until(expiresAt) + 20*time.Millisecond)
	expiringReplayed, err := fixture.server.startActor(t.Context(), expiring)
	if err != nil {
		t.Fatalf("replay after Actor expiry: %v", err)
	}
	if !expiringReplayed.Replayed ||
		expiringReplayed.ActorID != expiringCreated.ActorID ||
		expiringReplayed.BootRunID != expiringCreated.BootRunID {
		t.Fatalf("expired replay = %+v, created = %+v", expiringReplayed, expiringCreated)
	}
}

func TestActorStartPostgresRollbackLeavesNoResidue(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	if _, err := fixture.pool.Exec(t.Context(), `
		CREATE FUNCTION reject_actor_start_admission() RETURNS trigger
		LANGUAGE plpgsql
		AS $$
		BEGIN
			IF NEW.topic = 'run.admit' THEN
				RAISE EXCEPTION 'reject Actor start admission';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER reject_actor_start_admission
		BEFORE INSERT ON outbox_messages
		FOR EACH ROW EXECUTE FUNCTION reject_actor_start_admission();
	`); err != nil {
		t.Fatal(err)
	}
	key := "rollback"
	if _, err := fixture.server.startActor(t.Context(), fixture.request(0, &key, "rollback-1")); err == nil {
		t.Fatal("expected rejected outbox")
	}
	var claims, actors, records, runs, attempts, resolutions, outboxes, owned int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.start'),
		    (SELECT count(*) FROM actors),
		    (SELECT count(*) FROM actor_records),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start'),
		    (SELECT count(*) FROM run_attempts),
		    (SELECT count(*) FROM secret_resolutions),
		    (SELECT count(*) FROM outbox_messages WHERE topic = 'run.admit'),
		    (SELECT count(*) FROM workspaces WHERE owner_actor_id IS NOT NULL OR owner_run_id IS NOT NULL)
	`).Scan(&claims, &actors, &records, &runs, &attempts, &resolutions, &outboxes, &owned); err != nil {
		t.Fatal(err)
	}
	if claims+actors+records+runs+attempts+resolutions+outboxes+owned != 0 {
		t.Fatalf(
			"rollback residue = claims %d actors %d records %d runs %d attempts %d resolutions %d outboxes %d owned %d",
			claims, actors, records, runs, attempts, resolutions, outboxes, owned,
		)
	}
}

func TestActorStartPostgresNoInputBootsAtZeroHighWatermark(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	key := "no-input"
	request := fixture.request(0, &key, "no-input-1")
	request.ManagedQueueName = "priority"
	created, err := fixture.server.startActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.InitialRecordID != nil {
		t.Fatalf("initial record = %s, want absent", created.InitialRecordID)
	}
	assertActorStartTupleWithQueue(t, fixture, created, 0, "priority", nil)
}

func TestActorStartPostgresKeylessRequestsRemainAtLeastOnce(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	for index := range 2 {
		if _, err := fixture.server.startActor(t.Context(), fixture.request(index, nil, "")); err != nil {
			t.Fatal(err)
		}
	}
	var claims, actors, runs, owned int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.start'),
		    (SELECT count(*) FROM actors),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start'),
		    (SELECT count(*) FROM workspaces WHERE owner_actor_id IS NOT NULL)
	`).Scan(&claims, &actors, &runs, &owned); err != nil {
		t.Fatal(err)
	}
	if claims != 0 || actors != 2 || runs != 2 || owned != 2 {
		t.Fatalf("keyless counts claims=%d actors=%d runs=%d owned=%d", claims, actors, runs, owned)
	}
}

func TestActorStartPostgresConcurrentKeyCollisionCreatesOneIdentity(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	key := "shared-key"
	type outcome struct {
		result actorStartResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for index := range 2 {
		index := index
		go func() {
			<-start
			result, err := fixture.server.startActor(
				context.Background(),
				fixture.request(index, &key, fmt.Sprintf("race-%d", index)),
			)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	var successes, conflicts int
	for range 2 {
		value := <-outcomes
		var conflict ActorKeyConflictError
		switch {
		case value.err == nil:
			successes++
		case errors.As(value.err, &conflict):
			conflicts++
		default:
			t.Fatalf("race error = %v", value.err)
		}
	}
	var claimCount, actorCount, runCount, ownedCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.start'),
		    (SELECT count(*) FROM actors),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start'),
		    (SELECT count(*) FROM workspaces WHERE owner_actor_id IS NOT NULL)
	`).Scan(&claimCount, &actorCount, &runCount, &ownedCount); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 ||
		claimCount != 1 || actorCount != 1 || runCount != 1 || ownedCount != 1 {
		t.Fatalf(
			"race successes=%d conflicts=%d claims=%d actors=%d runs=%d owned=%d",
			successes, conflicts, claimCount, actorCount, runCount, ownedCount,
		)
	}
}

func (fixture actorStartPostgresFixture) request(index int, key *string, idempotencyKey string) actorStartRequest {
	workspaceID := fixture.workspaceIDs[index]
	return actorStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		ActorDeclaredID: "operator.v1", WorkspaceID: workspaceID,
		WorkspaceAddress: json.RawMessage(fmt.Sprintf(`{"id":%q}`, workspaceID.String())),
		Key:              key, IdempotencyKey: idempotencyKey,
	}
}

func assertActorStartTuple(t *testing.T, fixture actorStartPostgresFixture, result actorStartResult, highWatermark int64) {
	limit := int64(2)
	assertActorStartTupleWithQueue(t, fixture, result, highWatermark, "default", &limit)
}

func assertActorStartTupleWithQueue(
	t *testing.T,
	fixture actorStartPostgresFixture,
	result actorStartResult,
	highWatermark int64,
	wantQueue string,
	wantQueueLimit *int64,
) {
	t.Helper()
	var actorCurrentRun uuid.UUID
	var actorNextInput, actorCommitted int64
	var actorQueue string
	var actorQueueLimit *int64
	var actorMaxDuration int64
	var actorRetry []byte
	var workspaceOwner uuid.UUID
	var runActor uuid.UUID
	var runCause string
	var runStart, runHigh int64
	var attemptStart int64
	var recordSequence int64
	var recordSource string
	var claimState string
	var resolutionCount, outboxCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT current_run_id, next_input_sequence, committed_input_sequence,
		       managed_queue_name, managed_queue_concurrency_limit,
		       managed_max_active_duration_ms, managed_retry_policy
		  FROM actors
		 WHERE id = $1
	`, result.ActorID).Scan(
		&actorCurrentRun,
		&actorNextInput,
		&actorCommitted,
		&actorQueue,
		&actorQueueLimit,
		&actorMaxDuration,
		&actorRetry,
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT owner_actor_id FROM workspaces WHERE id = $1
	`, fixture.workspaceIDs[0]).Scan(&workspaceOwner); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT actor_id, cause_kind, actor_start_input_sequence, actor_start_input_high_watermark
		  FROM runs
		 WHERE id = $1
	`, result.BootRunID).Scan(&runActor, &runCause, &runStart, &runHigh); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT actor_start_input_sequence FROM run_attempts WHERE run_id = $1 AND number = 1
	`, result.BootRunID).Scan(&attemptStart); err != nil {
		t.Fatal(err)
	}
	if result.InitialRecordID != nil {
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT sequence, source_kind FROM actor_records WHERE id = $1
		`, *result.InitialRecordID).Scan(&recordSequence, &recordSource); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state
		  FROM idempotency_claims
		 WHERE operation = 'actor.start'
	`).Scan(&claimState); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM secret_resolutions WHERE run_id = $1 AND attempt_number = 1),
		    (SELECT count(*) FROM outbox_messages WHERE topic = 'run.admit' AND payload->>'runId' = $1::text)
	`, result.BootRunID).Scan(&resolutionCount, &outboxCount); err != nil {
		t.Fatal(err)
	}
	recordValid := (highWatermark == 0 && recordSequence == 0 && recordSource == "") ||
		(highWatermark == 1 && recordSequence == 1 && recordSource == "external")
	queueLimitValid := (wantQueueLimit == nil && actorQueueLimit == nil) ||
		(wantQueueLimit != nil && actorQueueLimit != nil && *wantQueueLimit == *actorQueueLimit)
	if actorCurrentRun != result.BootRunID || workspaceOwner != result.ActorID ||
		runActor != result.ActorID || runCause != "actor_start" ||
		runStart != 0 || runHigh != highWatermark || attemptStart != 0 ||
		actorNextInput != highWatermark+1 || actorCommitted != 0 ||
		actorQueue != wantQueue || !queueLimitValid || actorMaxDuration != 300_000 ||
		string(actorRetry) != `{"enabled": false}` ||
		!recordValid ||
		claimState != "completed" || resolutionCount != 1 || outboxCount != 1 {
		t.Fatalf(
			"Actor start tuple actorRun=%s next=%d committed=%d queue=%s/%v max=%d retry=%s owner=%s runActor=%s cause=%s cursor=%d high=%d attempt=%d record=%d/%s claim=%s resolutions=%d outboxes=%d",
			actorCurrentRun, actorNextInput, actorCommitted,
			actorQueue, actorQueueLimit, actorMaxDuration, actorRetry,
			workspaceOwner, runActor, runCause,
			runStart, runHigh, attemptStart, recordSequence, recordSource, claimState, resolutionCount, outboxCount,
		)
	}
}

func newActorStartPostgresFixture(t *testing.T, workspaceCount int) actorStartPostgresFixture {
	t.Helper()
	pool := openActorStartPostgres(t)
	hashBytes := bytes.Repeat([]byte{7}, keyedhash.KeySize)
	hashes, err := keyedhash.New(map[int32][]byte{1: hashBytes})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := hashes.Fingerprint(1)
	if err != nil {
		t.Fatal(err)
	}
	fixture := actorStartPostgresFixture{
		pool: pool, orgID: uuid.Must(uuid.NewV7()), projectID: uuid.Must(uuid.NewV7()),
		environmentID: uuid.Must(uuid.NewV7()), workspaceIDs: make([]uuid.UUID, workspaceCount),
	}
	deploymentID := uuid.Must(uuid.NewV7())
	actorDefinitionID := uuid.Must(uuid.NewV7())
	workspaceDefinitionID := uuid.Must(uuid.NewV7())
	sourceID, codeID, dependencyID, imageID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	mustActorStartExec(t, pool, `
		INSERT INTO regions (id, provider, provider_region, display_name)
		VALUES ('us-east-1', 'aws', 'us-east-1', 'Actor Start Test')
	`)
	mustActorStartExec(t, pool, `
		INSERT INTO lookup_hmac_versions (version, key_fingerprint, is_current)
		VALUES (1, $1, true)
	`, fingerprint[:])
	mustActorStartExec(t, pool, `
		INSERT INTO organizations (id, public_id, name, slug)
		VALUES ($1, $2, 'Actor Start Test', $3)
	`, fixture.orgID, actorStartPublicID(t, publicid.Organization), "actor-start-"+fixture.orgID.String())
	mustActorStartExec(t, pool, `
		INSERT INTO projects (id, public_id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, $3, 'us-east-1', $4, 'Actor Start Test')
	`, fixture.projectID, actorStartPublicID(t, publicid.Project), fixture.orgID, "actor-start-"+fixture.projectID.String())
	mustActorStartExec(t, pool, `
		INSERT INTO environments (id, public_id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, $5, 'Actor Start Test', '#3366ff')
	`, fixture.environmentID, actorStartPublicID(t, publicid.Environment), fixture.orgID,
		fixture.projectID, "actor-start-"+fixture.environmentID.String())

	digests := []string{
		"sha256:" + fmt.Sprintf("%064x", 1),
		"sha256:" + fmt.Sprintf("%064x", 2),
		"sha256:" + fmt.Sprintf("%064x", 3),
		"sha256:" + fmt.Sprintf("%064x", 4),
	}
	actorManifest := []byte(
		`{"idleTimeoutMs":30000,"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`,
	)
	_, actorManifestDigest, err := deployment.CanonicalManifestAndDigest(actorManifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig := []byte(
		`{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"},{"name":"priority"}]}`,
	)
	mustActorStartExec(t, pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/octet-stream'),
		       ($1, $3, 1, 'application/octet-stream'),
		       ($1, $4, 1, 'application/octet-stream'),
		       ($1, $5, 1, 'application/octet-stream')
	`, fixture.orgID, digests[0], digests[1], digests[2], digests[3])
	mustActorStartExec(t, pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $5, $6, $7, $8,  'deployment_source', 1, 'application/octet-stream'),
		       ($2, $5, $6, $7, $9,  'deployment_program_code', 1, 'application/octet-stream'),
		       ($3, $5, $6, $7, $10, 'deployment_program_dependencies', 1, 'application/octet-stream'),
		       ($4, $5, $6, $7, $11, 'workspace_image', 1, 'application/octet-stream')
	`, sourceID, codeID, dependencyID, imageID, fixture.orgID, fixture.projectID,
		fixture.environmentID, digests[0], digests[1], digests[2], digests[3])
	mustActorStartExec(t, pool, `
		INSERT INTO deployments (
		    id, public_id, org_id, project_id, environment_id, build_region_id,
		    build_architecture, build_runtime_digest, build_standard_toolchain_digest,
		    build_contract_version, version, content_hash, deployment_source_artifact_id,
		    program_code_artifact_id, program_dependency_artifact_id,
		    program_runtime_digest, program_architecture, queue_config, status
		) VALUES (
		    $1, $2, $3, $4, $5, 'us-east-1', 'aarch64',
		    decode(repeat('01', 32), 'hex'), decode(repeat('02', 32), 'hex'),
		    'helmr.program-build.v0', 'actor-start-test', $6, $7, $8, $9,
		    decode(repeat('01', 32), 'hex'), 'aarch64', $10::jsonb, 'deployed'
		)
	`, deploymentID, actorStartPublicID(t, publicid.Deployment), fixture.orgID, fixture.projectID,
		fixture.environmentID, digests[0], sourceID, codeID, dependencyID, queueConfig)
	mustActorStartExec(t, pool, `
		INSERT INTO deployment_definitions (
		    id, environment_id, deployment_id, kind, declared_id,
		    manifest_version, manifest, manifest_digest, workspace_architecture, artifact_id
		) VALUES
		    ($1, $3, $4, 'actor', 'operator.v1', 0, $6::jsonb, $7, NULL, NULL),
		    ($2, $3, $4, 'workspace', 'workspace.v1', 0, '{}'::jsonb, decode(repeat('04', 32), 'hex'), 'aarch64', $5)
	`, actorDefinitionID, workspaceDefinitionID, fixture.environmentID, deploymentID, imageID,
		actorManifest, actorManifestDigest[:])
	mustActorStartExec(t, pool, `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, deploymentID, fixture.environmentID)

	secretID, secretVersionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	mustActorStartExec(t, tx, `SET CONSTRAINTS ALL DEFERRED`)
	mustActorStartExec(t, tx, `
		INSERT INTO secrets (id, environment_id, name, current_version_id)
		VALUES ($1, $2, 'API_TOKEN', $3)
	`, secretID, fixture.environmentID, secretVersionID)
	mustActorStartExec(t, tx, `
		INSERT INTO secret_versions (
		    id, secret_id, version, key_id, nonce, ciphertext,
		    value_authenticator, authenticator_key_version
		) VALUES ($1, $2, 1, 'test-key', decode(repeat('01', 12), 'hex'),
		          decode(repeat('02', 16), 'hex'), decode(repeat('03', 32), 'hex'), 1)
	`, secretVersionID, secretID)
	for index := range workspaceCount {
		workspaceID, versionID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
		fixture.workspaceIDs[index] = workspaceID
		mustActorStartExec(t, tx, `
			INSERT INTO workspaces (
			    id, public_id, org_id, project_id, environment_id, region_id,
			    declaration_kind, workspace_declared_id, deployment_definition_id, head_version_id
			) VALUES ($1, $2, $3, $4, $5, 'us-east-1', 'workspace', 'workspace.v1', $6, $7)
		`, workspaceID, actorStartPublicID(t, publicid.Workspace), fixture.orgID,
			fixture.projectID, fixture.environmentID, workspaceDefinitionID, versionID)
		mustActorStartExec(t, tx, `
			INSERT INTO workspace_versions (
			    id, public_id, org_id, project_id, environment_id, workspace_id,
			    kind, state, content_digest, size_bytes, entry_count,
			    ownership_generation, writer_generation, published_at
			) VALUES ($1, $2, $3, $4, $5, $6, 'system', 'committed',
			          'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
			          0, 0, 0, 0, now())
		`, versionID, actorStartPublicID(t, publicid.WorkspaceVersion), fixture.orgID,
			fixture.projectID, fixture.environmentID, workspaceID)
		mustActorStartExec(t, tx, `
			INSERT INTO workspace_secrets (
			    workspace_id, environment_id, placement_kind, placement_target, secret_id
			) VALUES ($1, $2, 'env', 'API_TOKEN', $3)
		`, workspaceID, fixture.environmentID, secretID)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture.server = &Server{db: db.New(pool), tx: pool, claims: idempotency.New(hashes)}
	return fixture
}

func openActorStartPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping PostgreSQL Actor start test", name)
		}
	}
	versionOutput, err := exec.Command("postgres", "--version").CombinedOutput()
	if err != nil || !bytes.Contains(versionOutput, []byte(" 18.")) {
		t.Skipf("PostgreSQL 18 is required; got %s", bytes.TrimSpace(versionOutput))
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	if output, err := exec.Command("initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freeActorStartPostgresPort(t)
	logPath := filepath.Join(filepath.Dir(dataDir), "postgres.log")
	command := exec.Command(
		"pg_ctl", "-D", dataDir, "-l", logPath,
		"-o", fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port), "-w", "start",
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})
	dsn := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", os.Getenv("USER"), port)
	if err := schema.Up(t.Context(), dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func freeActorStartPostgresPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func mustActorStartExec(t *testing.T, executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, query string, args ...any) {
	t.Helper()
	if _, err := executor.Exec(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func actorStartPublicID(t *testing.T, prefix publicid.Prefix) string {
	t.Helper()
	value, err := publicid.New(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
