package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTaskStartPostgresCommitsAndReplaysOneAdmission(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	workspaceID := fixture.workspaceIDs[0]
	ttl := int64(60_000)
	request := taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload:        json.RawMessage(`{"imageId":"image-1"}`),
		WorkspaceID:    workspaceID,
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
	if !replayed.Replayed || replayed.RunID != created.RunID {
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
	var attempts, resolutions int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT w.owner_run_id,
		       (SELECT count(*) FROM run_attempts WHERE run_id = r.id),
		       (SELECT count(*) FROM secret_resolutions WHERE run_id = r.id)
		  FROM runs r
		  JOIN workspaces w ON w.id = r.workspace_id
		 WHERE r.id = $1
	`, created.RunID).Scan(&workspaceOwner, &attempts, &resolutions); err != nil {
		t.Fatal(err)
	}
	if run.ID != pgvalue.UUID(created.RunID) || run.EntrypointKind != "task" ||
		run.EntrypointDeclaredID != "resize-image" || run.CauseKind != "api" ||
		run.Status != db.RunStatusQueued || workspaceOwner != run.ID ||
		attempts != 1 || resolutions != 1 {
		t.Fatalf(
			"run=%+v owner=%v attempts=%d resolutions=%d",
			run, workspaceOwner, attempts, resolutions,
		)
	}
	snapshot, err := queries.GetRunSnapshot(t.Context(), db.GetRunSnapshotParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ID: pgvalue.UUID(created.RunID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != pgvalue.UUID(created.RunID) || !snapshot.DeploymentID.Valid ||
		snapshot.WorkspaceID != pgvalue.UUID(fixture.workspaceIDs[0]) ||
		snapshot.ParentRunID.Valid {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	listed, err := queries.ListRunListItems(t.Context(), db.ListRunListItemsParams{
		OrgID: pgvalue.UUID(fixture.orgID), ProjectID: pgvalue.UUID(fixture.projectID),
		EnvironmentID: pgvalue.UUID(fixture.environmentID), LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != pgvalue.UUID(created.RunID) {
		t.Fatalf("listed = %+v", listed)
	}
}

func TestTaskStartPostgresConcurrentClaimsDoNotDeadlockDeploymentAuthority(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	type outcome struct {
		result taskStartResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for index := range 2 {
		go func() {
			<-start
			workspaceID := fixture.workspaceIDs[index]
			result, err := fixture.server.startTask(context.Background(), taskStartRequest{
				OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
				TaskDeclaredID: "resize-image", PayloadPresent: true,
				Payload:        json.RawMessage(fmt.Sprintf(`{"imageId":"image-%d"}`, index)),
				WorkspaceID:    workspaceID,
				IdempotencyKey: fmt.Sprintf("concurrent-%d", index),
			})
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)
	runIDs := make(map[uuid.UUID]struct{}, 2)
	for range 2 {
		value := <-outcomes
		if value.err != nil {
			t.Fatalf("concurrent Task start: %v", value.err)
		}
		runIDs[value.result.RunID] = struct{}{}
	}
	if len(runIDs) != 2 {
		t.Fatalf("Run IDs = %v, want two distinct identities", runIDs)
	}
}

func TestCreateKeylessDetachedChildTaskRunFromParentDeployment(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	parentWorkspaceID := fixture.workspaceIDs[0]
	parent, err := fixture.server.startTask(t.Context(), taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload:     json.RawMessage(`{"imageId":"parent"}`),
		WorkspaceID: parentWorkspaceID,
	})
	if err != nil {
		t.Fatal(err)
	}

	var targetVersionID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT head_version_id
		  FROM workspaces
		 WHERE id = $1
	`, fixture.workspaceIDs[1]).Scan(&targetVersionID); err != nil {
		t.Fatal(err)
	}
	runID := uuid.Must(uuid.NewV7())
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	queries := db.New(fixture.pool)
	child, err := queries.CreateChildRunFromParentDeployment(
		t.Context(),
		db.CreateChildRunFromParentDeploymentParams{
			EntrypointDeclaredID:   "resize-image",
			WorkspaceID:            pgvalue.UUID(fixture.workspaceIDs[1]),
			BaseWorkspaceVersionID: pgvalue.UUID(targetVersionID),
			EnvironmentID:          pgvalue.UUID(fixture.environmentID),
			ParentRunID:            pgvalue.UUID(parent.RunID),
			ID:                     pgvalue.UUID(runID),
			ParentOwnsLifecycle:    pgtype.Bool{Bool: false, Valid: true},
			Payload:                json.RawMessage(`{"imageId":"child"}`),
			Metadata:               json.RawMessage(`{"source":"parent"}`),
			Tags:                   []string{"child"},
			QueueName:              "default",
			QueueOriginAt:          pgvalue.Timestamptz(now),
			QueueScoreAt:           pgvalue.Timestamptz(now),
			MaxActiveDurationMs:    300_000,
			RetryPolicy:            json.RawMessage(`{"enabled":false}`),
			RootSpanID:             rootSpanID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if child.ID != pgvalue.UUID(runID) ||
		child.CauseKind != "child" ||
		child.ParentRunID != pgvalue.UUID(parent.RunID) ||
		!child.ParentOwnsLifecycle.Valid ||
		child.ParentOwnsLifecycle.Bool ||
		child.ClaimID.Valid ||
		child.WorkspaceID != pgvalue.UUID(fixture.workspaceIDs[1]) ||
		child.Status != db.RunStatusQueued {
		t.Fatalf("child = %+v", child)
	}
	var attempts int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM run_attempts
		 WHERE run_id = $1
		   AND number = 1
		   AND workspace_id = $2
	`, runID, fixture.workspaceIDs[1]).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d", attempts)
	}
}
