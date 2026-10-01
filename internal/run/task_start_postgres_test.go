package run

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/idempotency"
)

// taskStartFixture is an environment whose current deployment declares the
// Task "test-task" with a payload, and a fresh Computer with one bound
// Secret.
type taskStartFixture struct {
	postgresFixture
	computerID uuid.UUID
	secretID   uuid.UUID
}

func newTaskStartFixture(t *testing.T) taskStartFixture {
	t.Helper()
	f := taskStartFixture{postgresFixture: newPostgresFixture(t)}
	declareTestTask(t, f.postgresFixture)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.environmentID, f.base.DeploymentID)
	created, err := computer.NewCreator(nil).Create(ctx, f.pool, computer.Request{
		Scope:      computer.Scope{OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID},
		DeclaredID: "test-computer",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.computerID = created.ComputerID
	f.secretID = uuid.NewV7()
	versionID := uuid.NewV7()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, ctx, tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO secrets (id, environment_id, name, current_version_id) VALUES ($1, $2, 'API_TOKEN', $3)`,
		f.secretID, f.environmentID, versionID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO secret_versions (id, secret_id, version, nonce, ciphertext)
		VALUES ($1, $2, 1, decode(repeat('01', 12), 'hex'), decode(repeat('02', 16), 'hex'))`, versionID, f.secretID)
	dbtest.MustExec(t, ctx, tx, `INSERT INTO computer_secrets (mode, computer_id, environment_id, placement_kind, placement_target, secret_id)
		VALUES ('raw', $1, $2, 'env', 'API_TOKEN', $3)`, f.computerID, f.environmentID, f.secretID)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return f
}

// declareTestTask gives the fixture deployment's Task "test-task" a payload
// and its default queue.
func declareTestTask(t *testing.T, f postgresFixture) {
	t.Helper()
	manifest := []byte(`{"payload":{"kind":"standard_schema"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`)
	_, digest, err := definition.CanonicalManifestAndDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE deployment_definitions SET manifest=$2::jsonb, manifest_digest=$3 WHERE id=$1`,
		f.base.TaskDefinitionID, manifest, digest[:])
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE deployments SET queue_config='{"formatVersion":0,"queues":[{"concurrencyLimit":2,"name":"default"}]}'::jsonb WHERE id=$1`,
		f.base.DeploymentID)
}

func (f taskStartFixture) start(payload string) TaskStart {
	return TaskStart{
		OrgID: f.orgID, ProjectID: f.projectID, EnvironmentID: f.environmentID,
		TaskDeclaredID: "test-task", PayloadPresent: true, Payload: json.RawMessage(payload),
		ComputerID: f.computerID, Metadata: json.RawMessage(`{}`), Tags: []string{},
	}
}

func (f taskStartFixture) claim(t *testing.T, start TaskStart, key string) idempotency.Request {
	t.Helper()
	claim, err := idempotency.NewTaskStartRequest(f.environmentID, start.TaskDeclaredID, key, idempotency.TaskStartFingerprint{
		PayloadPresent: start.PayloadPresent, Payload: start.Payload,
		Computer: json.RawMessage(`{"id":"` + start.ComputerID.String() + `"}`),
		Metadata: start.Metadata, Tags: start.Tags,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func TestStartTaskCommitsAndReplaysOneAdmission(t *testing.T) {
	f := newTaskStartFixture(t)
	var revision int64
	if err := f.pool.QueryRow(t.Context(), `SELECT revision FROM computers WHERE id=$1`, f.computerID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	start := f.start(`{"imageId":"one"}`)
	claim := f.claim(t, start, "one")
	created, err := StartTask(t.Context(), f.pool, claim, start)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := StartTask(t.Context(), f.pool, claim, start)
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed || !replayed.Replayed || replayed.RunID != created.RunID {
		t.Fatalf("created=%+v replayed=%+v", created, replayed)
	}
	var conflict idempotency.ConflictError
	if _, err := StartTask(t.Context(), f.pool, f.claim(t, f.start(`{"imageId":"two"}`), "one"), f.start(`{"imageId":"two"}`)); !errors.As(err, &conflict) {
		t.Fatalf("changed replay = %v", err)
	}
	var status, cause string
	var attempts, resolutions int
	var claimed bool
	var touched int64
	if err := f.pool.QueryRow(t.Context(), `SELECT r.status, r.cause_kind, r.claim_id IS NOT NULL,
		(SELECT count(*) FROM run_attempts WHERE run_id=r.id),
		(SELECT count(*) FROM secret_resolutions WHERE run_id=r.id),
		(SELECT revision FROM computers WHERE id=r.computer_id)
		FROM runs r WHERE r.id=$1 AND r.computer_id=$2`, created.RunID, f.computerID).Scan(&status, &cause, &claimed, &attempts, &resolutions, &touched); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || cause != "api" || !claimed || attempts != 1 || resolutions != 1 || touched <= revision {
		t.Fatalf("status=%s cause=%s claimed=%v attempts=%d resolutions=%d revision %d -> %d", status, cause, claimed, attempts, resolutions, revision, touched)
	}
}

func TestStartTaskWithoutClaimAdmitsEachStart(t *testing.T) {
	f := newTaskStartFixture(t)
	first, err := StartTask(t.Context(), f.pool, nil, f.start(`{"imageId":"one"}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := StartTask(t.Context(), f.pool, nil, f.start(`{"imageId":"one"}`))
	if err != nil {
		t.Fatal(err)
	}
	if first.RunID == second.RunID || first.Replayed || second.Replayed {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestStartTaskRejectsWithoutAdmitting(t *testing.T) {
	f := newTaskStartFixture(t)
	for name, test := range map[string]struct {
		prepare func(t *testing.T, start *TaskStart)
		want    error
	}{
		"undeclared task": {func(_ *testing.T, start *TaskStart) { start.TaskDeclaredID = "missing-task" }, ErrTaskNotDeployed},
		"payload presence": {func(_ *testing.T, start *TaskStart) {
			start.PayloadPresent, start.Payload = false, nil
		}, ErrTaskPayloadPresenceInvalid},
		"unknown computer": {func(_ *testing.T, start *TaskStart) { start.ComputerID = uuid.NewV7() }, ErrTaskComputerUnavailable},
		"other project":    {func(_ *testing.T, start *TaskStart) { start.ProjectID = uuid.NewV7() }, ErrTaskNotDeployed},
		"revoked secret": {func(t *testing.T, _ *TaskStart) {
			dbtest.MustExec(t, t.Context(), f.pool, `UPDATE secrets SET status='revoked', current_version_id=NULL, revoked_at=now(), revocation_generation=1 WHERE id=$1`, f.secretID)
		}, ErrTaskSecretUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			start := f.start(`{"imageId":"rejected"}`)
			test.prepare(t, &start)
			if _, err := StartTask(t.Context(), f.pool, nil, start); !errors.Is(err, test.want) {
				t.Fatalf("start error = %v, want %v", err, test.want)
			}
		})
	}
	var runs int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM runs WHERE computer_id=$1`, f.computerID).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Fatalf("rejected starts admitted %d Runs", runs)
	}
}
