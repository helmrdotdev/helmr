package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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

	deploymentID := fixture.deploymentID.String()
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
		UPDATE sessions
		   SET current_run_id = NULL,
		       next_input_sequence = 2
		 WHERE id = $1
	`, first.SessionID); err != nil {
		t.Fatal(err)
	}
	continuationID := uuid.NewV7()
	queueOriginAt := time.Now().UTC()
	continuation, err := db.New(fixture.pool).CreateActorContinuationRun(
		t.Context(),
		db.CreateActorContinuationRunParams{
			RunID:                 pgvalue.UUID(continuationID),
			QueueOriginAt:         pgtype.Timestamptz{Time: queueOriginAt, Valid: true},
			RootSpanID:            "0000000000000001",
			EnvironmentID:         pgvalue.UUID(fixture.environmentID),
			SessionID:             pgvalue.UUID(first.SessionID),
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
		    session_input_start_sequence, base_workspace_version_id
		)
		SELECT run_id, 2, entrypoint_kind, workspace_id,
		       session_input_start_sequence, base_workspace_version_id
		  FROM run_attempts
		 WHERE run_id = $1
		   AND number = 1
	`, continuation.ID); err != nil {
		t.Fatal(err)
	}

	recordIDs := []uuid.UUID{
		uuid.NewV7(),
		uuid.NewV7(),
		uuid.NewV7(),
	}
	createdAt := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `
		UPDATE sessions
		   SET next_output_sequence = 4
		 WHERE id = $1
	`, first.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		INSERT INTO session_records (
		    id, environment_id, session_id, direction, sequence, data,
		    content_type, producer_run_id, producer_attempt_number, created_at
		) VALUES
		    ($1, $4, $5, 'output', 1, 'null'::jsonb,
		     'application/json', $6, 1, $7),
		    ($2, $4, $5, 'output', 2, '{"value":2}'::jsonb,
		     'application/json', $8, 1, $7 + interval '1 second'),
		    ($3, $4, $5, 'output', 3, '[3]'::jsonb,
		     'application/vnd.helmr.test+json', $8, 2, $7 + interval '2 seconds')
	`, recordIDs[0], recordIDs[1], recordIDs[2], fixture.environmentID,
		first.SessionID, first.BootRunID, createdAt, continuation.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
		Permissions: []auth.Permission{auth.PermissionSessionsRead},
	}
	firstPage := readSessionOutputPostgresHTTP(t, fixture, principal, first.SessionID.String(), "/?limit=2")
	if len(firstPage.Records) != 2 ||
		firstPage.Records[0].Sequence != 1 ||
		string(firstPage.Records[0].Data) != "null" ||
		firstPage.Records[1].Sequence != 2 ||
		firstPage.NextAfter != 2 ||
		!firstPage.HasMore {
		t.Fatalf("first page = %+v", firstPage)
	}
	for index, record := range firstPage.Records {
		if record.ID != recordIDs[index].String() {
			t.Fatalf("record %d UUID = %s, want %s", index, record.ID, recordIDs[index])
		}
		wantRunID := first.BootRunID.String()
		if index == 1 {
			wantRunID = continuationID.String()
		}
		if record.Provenance.RunID != wantRunID ||
			record.Provenance.AttemptNumber != 1 ||
			record.Provenance.DeploymentID != deploymentID {
			t.Fatalf("record %d provenance = %+v", index, record.Provenance)
		}
	}

	nextPage := readSessionOutputPostgresHTTP(t, fixture, principal, first.SessionID.String(), "/?after=2&limit=2")
	if len(nextPage.Records) != 1 ||
		nextPage.Records[0].Sequence != 3 ||
		nextPage.Records[0].Provenance.RunID != continuationID.String() ||
		nextPage.Records[0].Provenance.AttemptNumber != 2 ||
		nextPage.NextAfter != 3 ||
		nextPage.HasMore {
		t.Fatalf("next page = %+v", nextPage)
	}
	emptyPage := readSessionOutputPostgresHTTP(t, fixture, principal, second.SessionID.String(), "/")
	if emptyPage.Records == nil ||
		len(emptyPage.Records) != 0 ||
		emptyPage.NextAfter != 0 ||
		emptyPage.HasMore {
		t.Fatalf("empty page = %+v", emptyPage)
	}
	futurePage := readSessionOutputPostgresHTTP(
		t,
		fixture,
		principal,
		first.SessionID.String(),
		"/?after=9007199254740991",
	)
	if len(futurePage.Records) != 0 ||
		futurePage.NextAfter != maxSessionOutputSequence ||
		futurePage.HasMore {
		t.Fatalf("future page = %+v", futurePage)
	}

	retention, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retention.Exec(t.Context(), `
		DELETE FROM session_records
		 WHERE session_id = $1
		   AND direction = 'output'
		   AND sequence < 3
	`, first.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := retention.Exec(t.Context(), `
		UPDATE sessions SET output_retention_floor = 3 WHERE id = $1
	`, first.SessionID); err != nil {
		t.Fatal(err)
	}

	beforeCommit := readSessionOutputPostgresHTTP(t, fixture, principal, first.SessionID.String(), "/")
	if len(beforeCommit.Records) != 3 || beforeCommit.Records[0].Sequence != 1 {
		t.Fatalf("uncommitted retention mixed into read = %+v", beforeCommit)
	}
	if err := retention.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE sessions
		   SET state = 'closed',
		       current_run_id = NULL,
		       closed_at = transaction_timestamp()
		 WHERE id = $1
	`, first.SessionID); err != nil {
		t.Fatal(err)
	}

	expiredRequest := sessionReadPostgresRequest("/?after=0", first.SessionID.String(), principal)
	expiredRecorder := httptest.NewRecorder()
	fixture.server.readSessionOutputHTTP(expiredRecorder, expiredRequest)
	if expiredRecorder.Code != http.StatusGone ||
		decodeHTTPError(t, expiredRecorder.Body.Bytes()).Code != "session_output_cursor_expired" {
		t.Fatalf("expired response = %d %s", expiredRecorder.Code, expiredRecorder.Body.String())
	}
	retainedPage := readSessionOutputPostgresHTTP(t, fixture, principal, first.SessionID.String(), "/")
	if len(retainedPage.Records) != 1 ||
		retainedPage.Records[0].Sequence != 3 ||
		retainedPage.NextAfter != 3 {
		t.Fatalf("retained page = %+v", retainedPage)
	}
	floorBoundaryPage := readSessionOutputPostgresHTTP(
		t,
		fixture,
		principal,
		first.SessionID.String(),
		"/?after=2",
	)
	if len(floorBoundaryPage.Records) != 1 ||
		floorBoundaryPage.Records[0].Sequence != 3 ||
		floorBoundaryPage.NextAfter != 3 {
		t.Fatalf("retention floor boundary page = %+v", floorBoundaryPage)
	}

	missingRequest := sessionReadPostgresRequest("/", uuid.NewV7().String(), principal)
	missingRecorder := httptest.NewRecorder()
	fixture.server.readSessionOutputHTTP(missingRecorder, missingRequest)
	if missingRecorder.Code != http.StatusNotFound ||
		!strings.Contains(missingRecorder.Body.String(), `"code":"session_not_found"`) {
		t.Fatalf("missing response = %d %s", missingRecorder.Code, missingRecorder.Body.String())
	}
}

func readSessionOutputPostgresHTTP(
	t *testing.T,
	fixture actorStartPostgresFixture,
	principal auth.Actor,
	sessionID string,
	target string,
) api.SessionOutputPage {
	t.Helper()
	request := sessionReadPostgresRequest(target, sessionID, principal)
	recorder := httptest.NewRecorder()
	fixture.server.readSessionOutputHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("Session output HTTP = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response api.SessionOutputPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
