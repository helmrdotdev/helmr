package control

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestActorOutputReadPostgresPagesProvenanceAndRetention(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 2)
	firstKey, secondKey := "output:first", "output:empty"
	first, err := fixture.server.startActor(t.Context(), fixture.request(0, &firstKey, "output-first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixture.server.startActor(t.Context(), fixture.request(1, &secondKey, "output-empty"))
	if err != nil {
		t.Fatal(err)
	}

	var deploymentPublicID string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT public_id FROM deployments WHERE id = $1
	`, fixture.deploymentID).Scan(&deploymentPublicID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE runs
		   SET status = 'succeeded',
		       output = 'null'::jsonb,
		       terminal_at = transaction_timestamp(),
		       updated_at = transaction_timestamp()
		 WHERE id = $1
	`, first.BootRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors
		   SET current_run_id = NULL,
		       next_input_sequence = 2
		 WHERE id = $1
	`, first.ActorID); err != nil {
		t.Fatal(err)
	}
	continuationID := uuid.Must(uuid.NewV7())
	continuationPublicID, err := publicid.New(publicid.Run)
	if err != nil {
		t.Fatal(err)
	}
	queueOriginAt := time.Now().UTC()
	continuation, err := db.New(fixture.pool).CreateActorContinuationRun(
		t.Context(),
		db.CreateActorContinuationRunParams{
			RunID:                 pgvalue.UUID(continuationID),
			PublicID:              continuationPublicID,
			QueueOriginAt:         pgtype.Timestamptz{Time: queueOriginAt, Valid: true},
			RootSpanID:            "0000000000000001",
			EnvironmentID:         pgvalue.UUID(fixture.environmentID),
			ActorID:               pgvalue.UUID(first.ActorID),
			WorkspaceID:           pgvalue.UUID(fixture.workspaceIDs[0]),
			ExpectedRunGeneration: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		INSERT INTO run_attempts (
		    run_id, number, entrypoint_kind, workspace_id,
		    actor_start_input_sequence, base_workspace_version_id
		)
		SELECT run_id, 2, entrypoint_kind, workspace_id,
		       actor_start_input_sequence, base_workspace_version_id
		  FROM run_attempts
		 WHERE run_id = $1
		   AND number = 1
	`, continuation.ID); err != nil {
		t.Fatal(err)
	}

	recordIDs := []uuid.UUID{
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
		uuid.Must(uuid.NewV7()),
	}
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `
		UPDATE actors
		   SET next_output_sequence = 4
		 WHERE id = $1
	`, first.ActorID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO actor_records (
		    id, environment_id, actor_id, direction, sequence, data,
		    content_type, producer_run_id, producer_attempt_number, created_at
		) VALUES
		    ($1, $4, $5, 'output', 1, 'null'::jsonb,
		     'application/json', $6, 1, $7),
		    ($2, $4, $5, 'output', 2, '{"value":2}'::jsonb,
		     'application/json', $8, 1, $7 + interval '1 second'),
		    ($3, $4, $5, 'output', 3, '[3]'::jsonb,
		     'application/vnd.helmr.test+json', $8, 2, $7 + interval '2 seconds')
	`, recordIDs[0], recordIDs[1], recordIDs[2], fixture.environmentID,
		first.ActorID, first.BootRunID, createdAt, continuation.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionActorsRead},
	}
	firstPage := readActorOutputPostgresHTTP(t, fixture, principal, "/?actor_key=output%3Afirst&limit=2")
	if len(firstPage.Records) != 2 ||
		firstPage.Records[0].Sequence != 1 ||
		string(firstPage.Records[0].Data) != "null" ||
		firstPage.Records[1].Sequence != 2 ||
		firstPage.NextAfter != 2 ||
		!firstPage.HasMore {
		t.Fatalf("first page = %+v", firstPage)
	}
	for index, record := range firstPage.Records {
		decoded, err := publicid.DecodeActorRecord(record.ID)
		if err != nil {
			t.Fatalf("record %d ID = %q: %v", index, record.ID, err)
		}
		if decoded != recordIDs[index] {
			t.Fatalf("record %d UUID = %s, want %s", index, decoded, recordIDs[index])
		}
		wantRunID := first.BootRunPublicID
		if index == 1 {
			wantRunID = continuationPublicID
		}
		if record.Provenance.RunID != wantRunID ||
			record.Provenance.AttemptNumber != 1 ||
			record.Provenance.DeploymentID != deploymentPublicID {
			t.Fatalf("record %d provenance = %+v", index, record.Provenance)
		}
	}

	nextPage := readActorOutputPostgresHTTP(t, fixture, principal, "/?actor_id="+first.ActorPublicID+"&after=2&limit=2")
	if len(nextPage.Records) != 1 ||
		nextPage.Records[0].Sequence != 3 ||
		nextPage.Records[0].Provenance.RunID != continuationPublicID ||
		nextPage.Records[0].Provenance.AttemptNumber != 2 ||
		nextPage.NextAfter != 3 ||
		nextPage.HasMore {
		t.Fatalf("next page = %+v", nextPage)
	}
	emptyPage := readActorOutputPostgresHTTP(t, fixture, principal, "/?actor_id="+second.ActorPublicID)
	if emptyPage.Records == nil ||
		len(emptyPage.Records) != 0 ||
		emptyPage.NextAfter != 0 ||
		emptyPage.HasMore {
		t.Fatalf("empty page = %+v", emptyPage)
	}
	futurePage := readActorOutputPostgresHTTP(
		t,
		fixture,
		principal,
		"/?actor_id="+first.ActorPublicID+"&after=9007199254740991",
	)
	if len(futurePage.Records) != 0 ||
		futurePage.NextAfter != maxActorOutputSequence ||
		futurePage.HasMore {
		t.Fatalf("future page = %+v", futurePage)
	}

	retention, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retention.Exec(t.Context(), `
		DELETE FROM actor_records
		 WHERE actor_id = $1
		   AND direction = 'output'
		   AND sequence < 3
	`, first.ActorID); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.Exec(t.Context(), `
		UPDATE actors SET output_retention_floor = 3 WHERE id = $1
	`, first.ActorID); err != nil {
		t.Fatal(err)
	}

	beforeCommit := readActorOutputPostgresHTTP(t, fixture, principal, "/?actor_id="+first.ActorPublicID)
	if len(beforeCommit.Records) != 3 || beforeCommit.Records[0].Sequence != 1 {
		t.Fatalf("uncommitted retention mixed into read = %+v", beforeCommit)
	}
	if err := retention.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors
		   SET state = 'closed',
		       current_run_id = NULL,
		       closed_at = transaction_timestamp()
		 WHERE id = $1
	`, first.ActorID); err != nil {
		t.Fatal(err)
	}

	expiredRequest := actorReadPostgresRequest("/?actor_id="+first.ActorPublicID+"&after=0", principal)
	expiredRecorder := httptest.NewRecorder()
	fixture.server.readActorOutputHTTP(expiredRecorder, expiredRequest)
	if expiredRecorder.Code != http.StatusGone ||
		!strings.Contains(expiredRecorder.Body.String(), `"code":"actor_output_cursor_expired"`) ||
		!strings.Contains(expiredRecorder.Body.String(), `"retryable":false`) {
		t.Fatalf("expired response = %d %s", expiredRecorder.Code, expiredRecorder.Body.String())
	}
	retainedPage := readActorOutputPostgresHTTP(t, fixture, principal, "/?actor_id="+first.ActorPublicID)
	if len(retainedPage.Records) != 1 ||
		retainedPage.Records[0].Sequence != 3 ||
		retainedPage.NextAfter != 3 {
		t.Fatalf("retained page = %+v", retainedPage)
	}
	floorBoundaryPage := readActorOutputPostgresHTTP(
		t,
		fixture,
		principal,
		"/?actor_id="+first.ActorPublicID+"&after=2",
	)
	if len(floorBoundaryPage.Records) != 1 ||
		floorBoundaryPage.Records[0].Sequence != 3 ||
		floorBoundaryPage.NextAfter != 3 {
		t.Fatalf("retention floor boundary page = %+v", floorBoundaryPage)
	}

	missingRequest := actorReadPostgresRequest("/?actor_key=missing", principal)
	missingRecorder := httptest.NewRecorder()
	fixture.server.readActorOutputHTTP(missingRecorder, missingRequest)
	if missingRecorder.Code != http.StatusNotFound ||
		!strings.Contains(missingRecorder.Body.String(), `"code":"actor_not_found"`) {
		t.Fatalf("missing response = %d %s", missingRecorder.Code, missingRecorder.Body.String())
	}
}

func readActorOutputPostgresHTTP(
	t *testing.T,
	fixture actorStartPostgresFixture,
	principal auth.Actor,
	target string,
) api.ActorOutputPage {
	t.Helper()
	request := actorReadPostgresRequest(target, principal)
	recorder := httptest.NewRecorder()
	fixture.server.readActorOutputHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Actor output HTTP = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.ActorOutputPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
