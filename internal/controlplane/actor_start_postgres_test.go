package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5/pgxpool"
)

type actorStartPostgresFixture struct {
	pool          *pgxpool.Pool
	server        *Server
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
	deploymentID  uuid.UUID
	workspaceIDs  []uuid.UUID
	workspaceRefs []string
	workspaceKeys []string
}

func TestActorStartPostgresCommitsReplaysAndRejectsConflicts(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	key := "thread:42"
	request := fixture.request(0, &key, "start-1")
	request.InputPresent = true
	request.Input = json.RawMessage(`{"message":"hello"}`)
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
		UPDATE sessions
		   SET state = 'closing'
		 WHERE id = $1
	`, created.SessionID); err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.server.startActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.SessionID != created.SessionID ||
		replayed.BootRunID != created.BootRunID ||
		replayed.InitialRecordID == nil || *replayed.InitialRecordID != *created.InitialRecordID {
		t.Fatalf("replayed = %+v, created = %+v", replayed, created)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = NULL WHERE id = $1
	`, fixture.environmentID); err != nil {
		t.Fatal(err)
	}
	replayedAfterUndeploy, err := fixture.server.startActor(t.Context(), request)
	if err != nil {
		t.Fatalf("replay after undeploy: %v", err)
	}
	if !replayedAfterUndeploy.Replayed || replayedAfterUndeploy.SessionID != created.SessionID {
		t.Fatalf("undeployed replay = %+v, created = %+v", replayedAfterUndeploy, created)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, fixture.deploymentID, fixture.environmentID); err != nil {
		t.Fatal(err)
	}

	changed := request
	changed.ManagedRunMetadata = json.RawMessage(`{"kind":"different"}`)
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
		    (SELECT count(*) FROM sessions),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start')
	`).Scan(&claimCount, &actorCount, &runCount); err != nil {
		t.Fatal(err)
	}
	if claimCount != 1 || actorCount != 1 || runCount != 1 {
		t.Fatalf("counts after conflicts = claims %d sessions %d runs %d", claimCount, actorCount, runCount)
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspaces SET dirty_state = 'clean' WHERE id = $1
	`, fixture.workspaceIDs[1]); err != nil {
		t.Fatal(err)
	}
}

func TestActorStartHTTPPostgresCreatesAndReplaysIDs(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	body := fmt.Sprintf(
		`{"workspace":{"id":%q},"input":null,"idempotency_key":"http-start-1","run":{"ttl":"30m","retry":{"max_attempts":3}}}`,
		fixture.workspaceRefs[0],
	)
	principal := auth.Actor{
		OrgID:         fixture.orgID,
		Kind:          auth.ActorKindAPIKey,
		Role:          auth.RoleDeveloper,
		ProjectID:     fixture.projectID.String(),
		EnvironmentID: fixture.environmentID.String(),
		Permissions:   []auth.Permission{auth.PermissionActorsStart},
	}
	var first api.StartActorResponse
	for attempt := range 2 {
		request := actorStartHTTPPostgresRequest(body, principal, "", "", "operator.v1")
		recorder := httptest.NewRecorder()
		fixture.server.startActorHTTP(recorder, request)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("attempt %d status=%d body=%s", attempt, recorder.Code, recorder.Body.String())
		}
		var response api.StartActorResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if err := ids.Validate(response.SessionID); err != nil {
			t.Fatalf("Actor ID: %v", err)
		}
		if err := ids.Validate(response.RunID); err != nil {
			t.Fatalf("Run ID: %v", err)
		}
		if attempt == 0 {
			first = response
		} else if response != first {
			t.Fatalf("replay response = %+v, first = %+v", response, first)
		}
	}
}

func TestActorStartHTTPPostgresDeniesBeforeAdmission(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	body := fmt.Sprintf(`{"workspace":{"id":%q}}`, fixture.workspaceRefs[0])
	principal := auth.Actor{
		OrgID:         fixture.orgID,
		Kind:          auth.ActorKindAPIKey,
		Role:          auth.RoleDeveloper,
		ProjectID:     fixture.projectID.String(),
		EnvironmentID: fixture.environmentID.String(),
	}
	recorder := httptest.NewRecorder()
	fixture.server.startActorHTTP(
		recorder,
		actorStartHTTPPostgresRequest(body, principal, "", "", "operator.v1"),
	)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var sessions int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Fatalf("sessions = %d, want 0", sessions)
	}
}

func TestActorStartHTTPPostgresAcceptsCanonicalInputBelowLimit(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	input := `[` + strings.Repeat(`0,`, 360_000) + `0]`
	if len(input) >= maxActorInputBytes {
		t.Fatalf("test input canonical size = %d", len(input))
	}
	body := fmt.Sprintf(
		`{"workspace":{"id":%q},"input":%s}`,
		fixture.workspaceRefs[0],
		input,
	)
	principal := auth.Actor{
		OrgID:         fixture.orgID,
		Kind:          auth.ActorKindAPIKey,
		Role:          auth.RoleDeveloper,
		ProjectID:     fixture.projectID.String(),
		EnvironmentID: fixture.environmentID.String(),
		Permissions:   []auth.Permission{auth.PermissionActorsStart},
	}
	recorder := httptest.NewRecorder()
	fixture.server.startActorHTTP(
		recorder,
		actorStartHTTPPostgresRequest(body, principal, "", "", "operator.v1"),
	)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var sessions int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM sessions`).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 {
		t.Fatalf("sessions = %d, want 1", sessions)
	}
}

func TestActorStartHTTPSessionPostgresCreates(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	body := fmt.Sprintf(`{"workspace":{"id":%q}}`, fixture.workspaceRefs[0])
	principal := auth.Actor{
		OrgID: fixture.orgID,
		Kind:  auth.ActorKindSession,
		Role:  auth.RoleDeveloper,
	}
	recorder := httptest.NewRecorder()
	fixture.server.startActorHTTP(
		recorder,
		actorStartHTTPPostgresRequest(
			body,
			principal,
			fixture.projectID.String(),
			fixture.environmentID.String(),
			"operator.v1",
		),
	)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.StartActorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if err := ids.Validate(response.SessionID); err != nil {
		t.Fatalf("Actor ID: %v", err)
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
	var claims, sessions, runs, owned int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.start'),
		    (SELECT count(*) FROM sessions),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start'),
		    (SELECT count(*) FROM workspaces WHERE owner_session_id IS NOT NULL)
	`).Scan(&claims, &sessions, &runs, &owned); err != nil {
		t.Fatal(err)
	}
	if claims != 0 || sessions != 2 || runs != 2 || owned != 2 {
		t.Fatalf("keyless counts claims=%d sessions=%d runs=%d owned=%d", claims, sessions, runs, owned)
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
		    (SELECT count(*) FROM sessions),
		    (SELECT count(*) FROM runs WHERE cause_kind = 'actor_start'),
		    (SELECT count(*) FROM workspaces WHERE owner_session_id IS NOT NULL)
	`).Scan(&claimCount, &actorCount, &runCount, &ownedCount); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || conflicts != 1 ||
		claimCount != 1 || actorCount != 1 || runCount != 1 || ownedCount != 1 {
		t.Fatalf(
			"race successes=%d conflicts=%d claims=%d sessions=%d runs=%d owned=%d",
			successes, conflicts, claimCount, actorCount, runCount, ownedCount,
		)
	}
}

func (fixture actorStartPostgresFixture) request(index int, key *string, idempotencyKey string) actorStartRequest {
	return actorStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		ActorDeclaredID: "operator.v1",
		WorkspaceID:     fixture.workspaceIDs[index],
		Key:             key, IdempotencyKey: idempotencyKey,
	}
}

func actorStartHTTPPostgresRequest(
	body string,
	principal auth.Actor,
	projectID string,
	environmentID string,
	actorDeclaredID string,
) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	route := chi.NewRouteContext()
	if projectID != "" {
		route.URLParams.Add("projectID", projectID)
	}
	if environmentID != "" {
		route.URLParams.Add("environmentID", environmentID)
	}
	route.URLParams.Add("actorDeclaredID", actorDeclaredID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
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
	var resolutionCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT current_run_id, next_input_sequence, committed_input_sequence,
		       run_queue_name, run_queue_concurrency_limit,
		       run_max_active_duration_ms, run_retry_policy
		  FROM sessions
		 WHERE id = $1
	`, result.SessionID).Scan(
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
		SELECT owner_session_id FROM workspaces WHERE id = $1
	`, fixture.workspaceIDs[0]).Scan(&workspaceOwner); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT session_id, cause_kind, session_input_start_sequence, session_input_high_watermark
		  FROM runs
		 WHERE id = $1
	`, result.BootRunID).Scan(&runActor, &runCause, &runStart, &runHigh); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT session_input_start_sequence FROM run_attempts WHERE run_id = $1 AND number = 1
	`, result.BootRunID).Scan(&attemptStart); err != nil {
		t.Fatal(err)
	}
	if result.InitialRecordID != nil {
		if err := fixture.pool.QueryRow(t.Context(), `
			SELECT sequence, source_kind FROM session_records WHERE id = $1
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
		SELECT count(*) FROM secret_resolutions WHERE run_id = $1 AND attempt_number = 1
	`, result.BootRunID).Scan(&resolutionCount); err != nil {
		t.Fatal(err)
	}
	recordValid := (highWatermark == 0 && recordSequence == 0 && recordSource == "") ||
		(highWatermark == 1 && recordSequence == 1 && recordSource == "external")
	queueLimitValid := (wantQueueLimit == nil && actorQueueLimit == nil) ||
		(wantQueueLimit != nil && actorQueueLimit != nil && *wantQueueLimit == *actorQueueLimit)
	if actorCurrentRun != result.BootRunID || workspaceOwner != result.SessionID ||
		runActor != result.SessionID || runCause != "actor_start" ||
		runStart != 0 || runHigh != highWatermark || attemptStart != 0 ||
		actorNextInput != highWatermark+1 || actorCommitted != 0 ||
		actorQueue != wantQueue || !queueLimitValid || actorMaxDuration != 300_000 ||
		string(actorRetry) != `{"enabled": false}` ||
		!recordValid ||
		claimState != "completed" || resolutionCount != 1 {
		t.Fatalf(
			"Actor start tuple actorRun=%s next=%d committed=%d queue=%s/%v max=%d retry=%s owner=%s runActor=%s cause=%s cursor=%d high=%d attempt=%d record=%d/%s claim=%s resolutions=%d",
			actorCurrentRun, actorNextInput, actorCommitted,
			actorQueue, actorQueueLimit, actorMaxDuration, actorRetry,
			workspaceOwner, runActor, runCause,
			runStart, runHigh, attemptStart, recordSequence, recordSource, claimState, resolutionCount,
		)
	}
}

func newActorStartPostgresFixture(t *testing.T, workspaceCount int) actorStartPostgresFixture {
	t.Helper()
	pool := openActorStartPostgres(t)
	fixture := actorStartPostgresFixture{
		pool: pool, orgID: uuid.NewV7(), projectID: uuid.NewV7(),
		environmentID: uuid.NewV7(), workspaceIDs: make([]uuid.UUID, workspaceCount),
		workspaceRefs: make([]string, workspaceCount), workspaceKeys: make([]string, workspaceCount),
	}
	deploymentID := uuid.NewV7()
	fixture.deploymentID = deploymentID
	actorDefinitionID := uuid.NewV7()
	taskDefinitionID := uuid.NewV7()
	workspaceDefinitionID := uuid.NewV7()
	programID, imageID := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO regions (id, display_name)
		VALUES ('us-east-1', 'Actor Start Test')
	`)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO organizations (id, name, slug)
		VALUES ($1, 'Actor Start Test', $2)
	`, fixture.orgID, "actor-start-"+fixture.orgID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO projects (id, org_id, default_region_id, slug, name)
		VALUES ($1, $2, 'us-east-1', $3, 'Actor Start Test')
	`, fixture.projectID, fixture.orgID, "actor-start-"+fixture.projectID.String())
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO environments (id, org_id, project_id, slug, name, color_hex)
		VALUES ($1, $2, $3, $4, 'Actor Start Test', '#3366ff')
	`, fixture.environmentID, fixture.orgID, fixture.projectID,
		"actor-start-"+fixture.environmentID.String())

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
	_, actorManifestDigest, err := deployment.CanonicalManifestAndDigest(actorManifest)
	if err != nil {
		t.Fatal(err)
	}
	_, taskManifestDigest, err := deployment.CanonicalManifestAndDigest(taskManifest)
	if err != nil {
		t.Fatal(err)
	}
	queueConfig := []byte(
		`{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"},{"name":"priority"}]}`,
	)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO cas_objects (org_id, digest, size_bytes, media_type)
		VALUES ($1, $2, 1, 'application/vnd.helmr.deployment-bundle.v0+json'),
		       ($1, $3, 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
		       ($1, $4, 1, 'application/octet-stream'),
		       ($1, $5, 1, 'application/vnd.helmr.runtime.v0+squashfs')
	`, fixture.orgID, digests[0], digests[1], digests[2], digests[3])
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO artifacts (id, org_id, project_id, environment_id, digest, kind, size_bytes, media_type)
		VALUES ($1, $3, $4, $5, $6, 'deployment_program', 1, 'application/vnd.helmr.deployment-program.v0+squashfs'),
		       ($2, $3, $4, $5, $7, 'workspace_image', 1, 'application/octet-stream')
	`, programID, imageID, fixture.orgID, fixture.projectID,
		fixture.environmentID, digests[1], digests[2])
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployments (
		    id, org_id, project_id, environment_id, version, bundle_digest,
		    runtime_artifact_digest, program_artifact_id, program_index_digest, queue_config
		) VALUES (
		    $1, $2, $3, $4, 'actor-start-test', $5, $6, $7,
		    decode(repeat('03', 32), 'hex'), $8::jsonb
		)
	`, deploymentID, fixture.orgID, fixture.projectID,
		fixture.environmentID, digests[0], digests[3], programID, queueConfig)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployment_definitions (
		    id, environment_id, deployment_id, kind, declared_id,
		    manifest_version, manifest, manifest_digest, artifact_id
		) VALUES
		    ($1, $4, $5, 'actor', 'operator.v1', 0, $7::jsonb, $8, NULL),
		    ($2, $4, $5, 'task', 'resize-image', 0, $9::jsonb, $10, NULL),
		    ($3, $4, $5, 'sandbox', 'workspace.v1', 0, '{}'::jsonb, decode(repeat('04', 32), 'hex'), $6)
	`, actorDefinitionID, taskDefinitionID, workspaceDefinitionID,
		fixture.environmentID, deploymentID, imageID,
		actorManifest, actorManifestDigest[:], taskManifest, taskManifestDigest[:])
	dbtest.MustExec(t, t.Context(), pool, `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, deploymentID, fixture.environmentID)

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
	`, secretID, fixture.environmentID, secretVersionID)
	dbtest.MustExec(t, t.Context(), tx, `
		INSERT INTO secret_versions (
		    id, secret_id, version, nonce, ciphertext
		) VALUES ($1, $2, 1, decode(repeat('01', 12), 'hex'),
		          decode(repeat('02', 16), 'hex'))
	`, secretVersionID, secretID)
	for index := range workspaceCount {
		workspaceID, versionID := uuid.NewV7(), uuid.NewV7()
		fixture.workspaceIDs[index] = workspaceID
		fixture.workspaceRefs[index] = workspaceID.String()
		fixture.workspaceKeys[index] = fmt.Sprintf("workspace:%d", index)
		dbtest.MustExec(t, t.Context(), tx, `
			INSERT INTO workspaces (
			    id, environment_id, region_id,
			    sandbox_declared_id, deployment_definition_id, head_version_id, key
			) VALUES ($1, $2, 'us-east-1', 'workspace.v1', $3, $4, $5)
		`, workspaceID, fixture.environmentID, workspaceDefinitionID, versionID,
			fixture.workspaceKeys[index])
		dbtest.MustExec(t, t.Context(), tx, `
			INSERT INTO workspace_versions (
			    id, environment_id, workspace_id,
			    kind, state, content_digest, size_bytes, entry_count,
			    ownership_generation, writer_generation, published_at
			) VALUES ($1, $2, $3, 'system', 'committed',
			          'sha256:d2ce8eece19cb4f6db14e37f6d986da7eec7f654f3b91c5c706e9d74e7d2bc96',
			          0, 0, 0, 0, now())
		`, versionID, fixture.environmentID, workspaceID)
		dbtest.MustExec(t, t.Context(), tx, `
			INSERT INTO workspace_secrets (
			    workspace_id, environment_id, placement_kind, placement_target, secret_id
			) VALUES ($1, $2, 'env', 'API_TOKEN', $3)
		`, workspaceID, fixture.environmentID, secretID)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	fixture.server = &Server{db: db.New(pool), tx: pool}
	return fixture
}

func openActorStartPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	return database.Pool
}
