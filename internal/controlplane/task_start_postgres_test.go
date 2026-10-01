package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

func createFreshTaskComputer(t *testing.T, fixture sessiontest.Fixture) uuid.UUID {
	t.Helper()
	created, err := computer.NewCreator(nil).Create(t.Context(), fixture.Pool, computer.Request{
		Scope:      computer.Scope{OrgID: fixture.OrgID, ProjectID: fixture.ProjectID, EnvironmentID: fixture.EnvironmentID},
		DeclaredID: "computer.v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	var status string
	if err := fixture.Pool.QueryRow(t.Context(), `
		SELECT v.status FROM computers c JOIN computer_disk_versions v ON v.id = c.head_disk_version_id
		WHERE c.id = $1
	`, created.ComputerID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "initializing" {
		t.Fatalf("fresh version = %s", status)
	}
	return created.ComputerID
}

func TestTaskStartPostgresFreshComputer(t *testing.T) {
	fixture := sessiontest.New(t, 0)
	computerID := createFreshTaskComputer(t, fixture)
	request := taskStartRequest{
		OrgID: fixture.OrgID, ProjectID: fixture.ProjectID, EnvironmentID: fixture.EnvironmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload: json.RawMessage(`{"imageId":"fresh"}`), ComputerID: computerID,
		IdempotencyKey: "fresh-task",
	}
	started, err := startTaskRun(t.Context(), fixture, request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := startTaskRun(t.Context(), fixture, request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.RunID != started.RunID {
		t.Fatalf("replay = %+v", replayed)
	}
	var status, versionStatus string
	var owner uuid.UUID
	var attempts int
	if err := fixture.Pool.QueryRow(t.Context(), `
		SELECT r.status, v.status, r.computer_id,
		       (SELECT count(*) FROM run_attempts a WHERE a.run_id = r.id AND a.base_computer_disk_version_id = v.id)
		FROM runs r JOIN computers c ON c.id = r.computer_id
		JOIN computer_disk_versions v ON v.id = r.base_computer_disk_version_id
		WHERE r.id = $1
	`, started.RunID).Scan(&status, &versionStatus, &owner, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || versionStatus != "initializing" || owner != computerID || attempts != 1 {
		t.Fatalf("status=%s version=%s owner=%s attempts=%d", status, versionStatus, owner, attempts)
	}
}

func TestTaskStartPostgresCommitsAndReplaysOneAdmission(t *testing.T) {
	fixture := sessiontest.New(t, 2)
	computerID := fixture.ComputerIDs[0]
	ttl := int64(60_000)
	request := taskStartRequest{
		OrgID: fixture.OrgID, ProjectID: fixture.ProjectID, EnvironmentID: fixture.EnvironmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload:        json.RawMessage(`{"imageId":"image-1"}`),
		ComputerID:     computerID,
		IdempotencyKey: "image-1", QueuedTTLMS: &ttl,
		Metadata: json.RawMessage(`{"source":"test"}`), Tags: []string{"image"},
	}
	created, err := startTaskRun(t.Context(), fixture, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed {
		t.Fatalf("created = %+v", created)
	}
	replayed, err := startTaskRun(t.Context(), fixture, request)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.RunID != created.RunID {
		t.Fatalf("replayed = %+v created = %+v", replayed, created)
	}
	changed := request
	changed.Payload = json.RawMessage(`{"imageId":"image-2"}`)
	var conflict idempotency.ConflictError
	if _, err := startTaskRun(t.Context(), fixture, changed); !errors.As(err, &conflict) {
		t.Fatalf("conflicting replay = %v", err)
	}

	queries := db.New(fixture.Pool)
	run, err := queries.GetRun(t.Context(), db.GetRunParams{
		EnvironmentID: pgvalue.UUID(fixture.EnvironmentID),
		ID:            pgvalue.UUID(created.RunID),
	})
	if err != nil {
		t.Fatal(err)
	}
	var storedComputerID pgtype.UUID
	var attempts, resolutions int
	if err := fixture.Pool.QueryRow(t.Context(), `
		SELECT r.computer_id,
		       (SELECT count(*) FROM run_attempts WHERE run_id = r.id),
		       (SELECT count(*) FROM secret_resolutions WHERE run_id = r.id)
		  FROM runs r
		  JOIN computers w ON w.id = r.computer_id
		 WHERE r.id = $1
	`, created.RunID).Scan(&storedComputerID, &attempts, &resolutions); err != nil {
		t.Fatal(err)
	}
	if run.ID != pgvalue.UUID(created.RunID) || run.EntrypointKind != "task" ||
		run.EntrypointDeclaredID != "resize-image" || run.CauseKind != "api" ||
		run.Status != db.RunStatusQueued || storedComputerID != run.ComputerID ||
		attempts != 1 || resolutions != 1 {
		t.Fatalf(
			"run=%+v owner=%v attempts=%d resolutions=%d",
			run, storedComputerID, attempts, resolutions,
		)
	}
	snapshot, err := queries.GetRunSnapshot(t.Context(), db.GetRunSnapshotParams{
		OrgID: pgvalue.UUID(fixture.OrgID), ProjectID: pgvalue.UUID(fixture.ProjectID),
		EnvironmentID: pgvalue.UUID(fixture.EnvironmentID), ID: pgvalue.UUID(created.RunID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != pgvalue.UUID(created.RunID) || !snapshot.DeploymentID.Valid ||
		snapshot.ComputerID != pgvalue.UUID(fixture.ComputerIDs[0]) ||
		snapshot.ParentRunID.Valid {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	listed, err := queries.ListRunListItems(t.Context(), db.ListRunListItemsParams{
		OrgID: pgvalue.UUID(fixture.OrgID), ProjectID: pgvalue.UUID(fixture.ProjectID),
		EnvironmentID: pgvalue.UUID(fixture.EnvironmentID), LimitCount: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != pgvalue.UUID(created.RunID) {
		t.Fatalf("listed = %+v", listed)
	}
}

func TestTaskStartPostgresConcurrentClaimsDoNotDeadlockDeploymentAuthority(t *testing.T) {
	fixture := sessiontest.New(t, 2)
	type outcome struct {
		result run.TaskStarted
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for index := range 2 {
		go func() {
			<-start
			computerID := fixture.ComputerIDs[index]
			result, err := startTaskRun(context.Background(), fixture, taskStartRequest{
				OrgID: fixture.OrgID, ProjectID: fixture.ProjectID, EnvironmentID: fixture.EnvironmentID,
				TaskDeclaredID: "resize-image", PayloadPresent: true,
				Payload:        json.RawMessage(fmt.Sprintf(`{"imageId":"image-%d"}`, index)),
				ComputerID:     computerID,
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
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh=%t", fresh), func(t *testing.T) {
			fixture := sessiontest.New(t, 2)
			if fresh {
				fixture.ComputerIDs[1] = createFreshTaskComputer(t, fixture)
			}
			parentComputerID := fixture.ComputerIDs[0]
			parent, err := startTaskRun(t.Context(), fixture, taskStartRequest{
				OrgID: fixture.OrgID, ProjectID: fixture.ProjectID, EnvironmentID: fixture.EnvironmentID,
				TaskDeclaredID: "resize-image", PayloadPresent: true,
				Payload:    json.RawMessage(`{"imageId":"parent"}`),
				ComputerID: parentComputerID,
			})
			if err != nil {
				t.Fatal(err)
			}

			var targetVersionID uuid.UUID
			if err := fixture.Pool.QueryRow(t.Context(), `
		SELECT head_disk_version_id
		  FROM computers
		 WHERE id = $1
	`, fixture.ComputerIDs[1]).Scan(&targetVersionID); err != nil {
				t.Fatal(err)
			}
			runID := uuid.NewV7()
			rootSpanID, err := tracing.NewSpanID()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			queries := db.New(fixture.Pool)
			child, err := queries.CreateChildRunFromParentDeployment(
				t.Context(),
				db.CreateChildRunFromParentDeploymentParams{
					EntrypointDeclaredID:      "resize-image",
					ComputerID:                pgvalue.UUID(fixture.ComputerIDs[1]),
					BaseComputerDiskVersionID: pgvalue.UUID(targetVersionID),
					EnvironmentID:             pgvalue.UUID(fixture.EnvironmentID),
					ParentRunID:               pgvalue.UUID(parent.RunID),
					ID:                        pgvalue.UUID(runID),
					ParentOwnsLifecycle:       pgtype.Bool{Bool: false, Valid: true},
					Payload:                   json.RawMessage(`{"imageId":"child"}`),
					Metadata:                  json.RawMessage(`{"source":"parent"}`),
					Tags:                      []string{"child"},
					QueueName:                 "default",
					QueueOriginAt:             pgvalue.Timestamptz(now),
					QueueScoreAt:              pgvalue.Timestamptz(now),
					MaxActiveDurationMs:       300_000,
					RetryPolicy:               json.RawMessage(`{"enabled":false}`),
					RootSpanID:                rootSpanID,
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
				child.ComputerID != pgvalue.UUID(fixture.ComputerIDs[1]) ||
				child.Status != db.RunStatusQueued {
				t.Fatalf("child = %+v", child)
			}
			var attempts int
			if err := fixture.Pool.QueryRow(t.Context(), `
		SELECT count(*)
		  FROM run_attempts
		 WHERE run_id = $1
		   AND number = 1
		   AND computer_id = $2
	`, runID, fixture.ComputerIDs[1]).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 {
				t.Fatalf("attempts = %d", attempts)
			}
		})
	}
}

// A Task start through NewServer admits the Run with 201 and replays the same
// Run with 201 for the same idempotency key.
func TestTaskStartHTTPAdmitsAndReplays(t *testing.T) {
	fixture := sessiontest.New(t, 1)
	server := newSessionHTTP(t, fixture)
	token := server.memberSession(t, db.OrgMemberRoleOwner)
	path := fmt.Sprintf("/api/projects/%s/environments/%s/tasks/resize-image/start", fixture.ProjectID, fixture.EnvironmentID)
	body := `{"payload":{"imageId":"http"},"computer":{"id":"` + fixture.ComputerIDs[0].String() + `"},"idempotency_key":"http-start"}`
	var runIDs []string
	for range 2 {
		response := server.request(t, http.MethodPost, path, token, body)
		if response.Code != http.StatusCreated {
			t.Fatalf("start = %d %s", response.Code, response.Body.String())
		}
		var started api.StartTaskResponse
		if err := json.Unmarshal(response.Body.Bytes(), &started); err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, started.RunID)
	}
	if runIDs[0] == "" || runIDs[0] != runIDs[1] {
		t.Fatalf("started Runs = %v", runIDs)
	}
	missing := server.request(t, http.MethodPost, fmt.Sprintf("/api/projects/%s/environments/%s/tasks/missing-task/start", fixture.ProjectID, fixture.EnvironmentID), token,
		`{"payload":{},"computer":{"id":"`+fixture.ComputerIDs[0].String()+`"}}`)
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"task_not_deployed"`) {
		t.Fatalf("undeployed start = %d %s", missing.Code, missing.Body.String())
	}
}
