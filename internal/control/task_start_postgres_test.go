package control

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTaskStartPostgresCommitsAndReplaysOneAdmission(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	workspaceID := fixture.workspaceRefs[0]
	ttl := int64(60_000)
	request := taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload:        json.RawMessage(`{"imageId":"image-1"}`),
		Workspace:      api.WorkspaceTarget{ID: &workspaceID},
		IdempotencyKey: "image-1", QueuedTTLMS: &ttl,
		Metadata: json.RawMessage(`{"source":"test"}`), Tags: []string{"image"},
	}
	created, err := fixture.server.startTask(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed {
		t.Fatalf("created = %+v", created)
	}
	replayed, err := fixture.server.startTask(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.RunID != created.RunID ||
		replayed.RunPublicID != created.RunPublicID {
		t.Fatalf("replayed = %+v created = %+v", replayed, created)
	}
	changed := request
	changed.Payload = json.RawMessage(`{"imageId":"image-2"}`)
	var conflict idempotency.ConflictError
	if _, err := fixture.server.startTask(t.Context(), changed); !errors.As(err, &conflict) {
		t.Fatalf("conflicting replay = %v", err)
	}

	queries := db.New(fixture.pool)
	run, err := queries.GetRun(t.Context(), db.GetRunParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID),
		ID:            pgvalue.UUID(created.RunID),
	})
	if err != nil {
		t.Fatal(err)
	}
	var workspaceOwner pgtype.UUID
	var attempts, resolutions, outboxes int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT w.owner_run_id,
		       (SELECT count(*) FROM run_attempts WHERE run_id = r.id),
		       (SELECT count(*) FROM secret_resolutions WHERE run_id = r.id),
		       (SELECT count(*) FROM outbox_messages
		         WHERE topic = 'run.admit' AND payload->>'runId' = r.id::text)
		  FROM runs r
		  JOIN workspaces w ON w.id = r.workspace_id
		 WHERE r.id = $1
	`, created.RunID).Scan(&workspaceOwner, &attempts, &resolutions, &outboxes); err != nil {
		t.Fatal(err)
	}
	if run.PublicID != created.RunPublicID || run.EntrypointKind != "task" ||
		run.EntrypointDeclaredID != "resize-image" || run.CauseKind != "api" ||
		run.Status != db.RunStatusQueued || workspaceOwner != run.ID ||
		attempts != 1 || resolutions != 1 || outboxes != 1 {
		t.Fatalf(
			"run=%+v owner=%v attempts=%d resolutions=%d outboxes=%d",
			run, workspaceOwner, attempts, resolutions, outboxes,
		)
	}
	snapshot, err := queries.GetRunSnapshot(t.Context(), db.GetRunSnapshotParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), PublicID: created.RunPublicID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PublicID != created.RunPublicID || snapshot.DeploymentPublicID == "" ||
		snapshot.WorkspacePublicID != workspaceID || snapshot.ParentRunPublicID != "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	listed, err := queries.ListRunSnapshots(t.Context(), db.ListRunSnapshotsParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].PublicID != created.RunPublicID {
		t.Fatalf("listed = %+v", listed)
	}
}
