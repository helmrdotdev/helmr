package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/session"
)

func TestActorClosePostgresClosesIdleActorAndReplaysBoundedReceipt(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	settleActorBootRun(t, fixture, started, 0)
	actorID := started.SessionID.String()
	request := actorCloseRequest{
		EnvironmentID: fixture.environmentID, SessionID: started.SessionID,
		WorkspaceID:    fixture.workspaceIDs[0],
		IdempotencyKey: "close-idle-1",
	}
	var priorOwnershipGeneration int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT ownership_generation FROM workspaces WHERE id = $1
	`, fixture.workspaceIDs[0]).Scan(&priorOwnershipGeneration); err != nil {
		t.Fatal(err)
	}

	closed, err := fixture.server.closeActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if closed.SessionID != actorID || closed.AcceptedAt.IsZero() {
		t.Fatalf("closed receipt = %+v", closed)
	}

	var state string
	var currentRunID *uuid.UUID
	var closeSequence int64
	var closedAtValid bool
	var ownerSessionID *uuid.UUID
	var ownershipGeneration int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state, current_run_id, close_sequence, closed_at IS NOT NULL
		  FROM sessions
		 WHERE id = $1
	`, started.SessionID).Scan(&state, &currentRunID, &closeSequence, &closedAtValid); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT owner_session_id, ownership_generation
		  FROM workspaces
		 WHERE id = $1
	`, fixture.workspaceIDs[0]).Scan(&ownerSessionID, &ownershipGeneration); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || currentRunID != nil || closeSequence != 0 ||
		!closedAtValid || ownerSessionID != nil ||
		ownershipGeneration != priorOwnershipGeneration+1 {
		t.Fatalf(
			"closed state=%s current=%v boundary=%d closedAt=%v owner=%v generation=%d",
			state, currentRunID, closeSequence, closedAtValid, ownerSessionID, ownershipGeneration,
		)
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE sessions SET state_version = state_version + 1 WHERE id = $1
	`, started.SessionID); err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.server.closeActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != closed {
		t.Fatalf("replayed receipt = %+v, closed = %+v", replayed, closed)
	}
	var receipt []byte
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT receipt
		  FROM idempotency_claims
		 WHERE operation = 'session.close'
		   AND state = 'completed'
	`).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(receipt, &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members["session_id"] == nil ||
		members["accepted_at"] == nil ||
		len(receipt) > 256 {
		t.Fatalf("claim receipt = %s", receipt)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE idempotency_claims
		   SET receipt = jsonb_set(
		       receipt,
		       '{session_id}',
		       '"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc3b"'::jsonb
		   )
		 WHERE operation = 'session.close'
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.server.closeActor(t.Context(), request); !errors.Is(err, errActorCloseReceipt) {
		t.Fatalf("corrupt replay error = %v", err)
	}
}

func TestActorClosePostgresClaimsOneBacklogContinuationAndReplays(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	request := fixture.request(0, nil, "")
	request.InputPresent = true
	request.Input = json.RawMessage(`{"message":"queued"}`)
	started, err := fixture.server.startActor(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	settleActorBootRun(t, fixture, started, 0)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE sessions SET manual_run_cancelled = true WHERE id = $1
	`, started.SessionID); err != nil {
		t.Fatal(err)
	}
	actorID := started.SessionID.String()
	closeRequest := actorCloseRequest{
		EnvironmentID: fixture.environmentID, SessionID: started.SessionID,
		WorkspaceID:    fixture.workspaceIDs[0],
		IdempotencyKey: "close-backlog-1",
	}

	closing, err := fixture.server.closeActor(t.Context(), closeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if closing.SessionID != actorID || closing.AcceptedAt.IsZero() {
		t.Fatalf("closing receipt = %+v", closing)
	}
	assertActorCloseContinuation(t, fixture, started.SessionID)

	replayed, err := fixture.server.closeActor(t.Context(), closeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != closing {
		t.Fatalf("replayed receipt = %+v, closing = %+v", replayed, closing)
	}
	assertActorCloseContinuation(t, fixture, started.SessionID)
}

func TestActorClosePostgresRejectsFailedActorWithoutClaimResidue(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	settleActorBootRun(t, fixture, started, 0)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE sessions
		   SET state = 'failed',
		       failure = jsonb_build_object(
		           'code', 'run_failed',
		           'message', 'Session run failed',
		           'details', jsonb_build_object('run_id', ($2::uuid)::text)
		       ),
		       failure_run_id = $2::uuid,
		       failed_at = now()
		 WHERE id = $1
	`, started.SessionID, started.BootRunID); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.server.closeActor(t.Context(), actorCloseRequest{
		EnvironmentID: fixture.environmentID, SessionID: started.SessionID,
		WorkspaceID:    fixture.workspaceIDs[0],
		IdempotencyKey: "close-failed-1",
	})
	if !errors.Is(err, errActorCloseConflict) {
		t.Fatalf("close failed Actor error = %v", err)
	}
	var claims int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims WHERE operation = 'session.close'
	`).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 0 {
		t.Fatalf("close claim residue = %d", claims)
	}
}

func TestActorClosePostgresReconcilesAfterWorkspaceAuthorityRecovers(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	settleActorBootRun(t, fixture, started, 0)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspaces SET dirty_state = 'dirty' WHERE id = $1
	`, fixture.workspaceIDs[0]); err != nil {
		t.Fatal(err)
	}
	actorID := started.SessionID.String()
	receipt, err := fixture.server.closeActor(t.Context(), actorCloseRequest{
		EnvironmentID: fixture.environmentID, SessionID: started.SessionID,
		WorkspaceID:    fixture.workspaceIDs[0],
		IdempotencyKey: "close-deferred-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SessionID != actorID || receipt.AcceptedAt.IsZero() {
		t.Fatalf("receipt = %+v", receipt)
	}
	var state string
	var ownerSessionID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT sessions.state, workspaces.owner_session_id
		  FROM sessions
		  JOIN workspaces ON workspaces.id = sessions.workspace_id
		 WHERE sessions.id = $1
	`, started.SessionID).Scan(&state, &ownerSessionID); err != nil {
		t.Fatal(err)
	}
	if state != "closing" || ownerSessionID != started.SessionID {
		t.Fatalf("deferred state=%s owner=%s", state, ownerSessionID)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspaces SET dirty_state = 'clean' WHERE id = $1
	`, fixture.workspaceIDs[0]); err != nil {
		t.Fatal(err)
	}
	reconciler, err := session.NewReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := reconciler.ReconcileClose(
		t.Context(),
		fixture.environmentID,
		started.SessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if deferred {
		t.Fatal("reconciliation remained deferred after Workspace recovery")
	}
	var ownerAfter *uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT sessions.state, workspaces.owner_session_id
		  FROM sessions
		  JOIN workspaces ON workspaces.id = sessions.workspace_id
		 WHERE sessions.id = $1
	`, started.SessionID).Scan(&state, &ownerAfter); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || ownerAfter != nil {
		t.Fatalf("reconciled state=%s owner=%v", state, ownerAfter)
	}
}

func TestActorCloseHTTPPostgresAuthorizesBeforeLookupAndCloses(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	settleActorBootRun(t, fixture, started, 0)
	actorID := started.SessionID.String()
	body := `{"idempotency_key":"http-close-1"}`
	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
	}
	recorder := httptest.NewRecorder()
	fixture.server.closeSessionHTTP(
		recorder,
		sessionClosePostgresRequest(body, principal, "", "", actorID),
	)
	if recorder.Code != http.StatusForbidden ||
		!strings.Contains(recorder.Body.String(), `"code":"permission_required"`) {
		t.Fatalf("denied status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var deniedState string
	var deniedClaims int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT sessions.state,
		       (SELECT count(*) FROM idempotency_claims WHERE operation = 'session.close')
		  FROM sessions
		 WHERE sessions.id = $1
	`, started.SessionID).Scan(&deniedState, &deniedClaims); err != nil {
		t.Fatal(err)
	}
	if deniedState != "open" || deniedClaims != 0 {
		t.Fatalf("denied residue state=%s claims=%d", deniedState, deniedClaims)
	}
	principal.Permissions = []auth.Permission{auth.PermissionSessionsClose}
	recorder = httptest.NewRecorder()
	fixture.server.closeSessionHTTP(
		recorder,
		sessionClosePostgresRequest(body, principal, "", "", actorID),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var receipt api.SessionCloseReceipt
	if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.SessionID != actorID || receipt.AcceptedAt.IsZero() {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestActorCloseHTTPSessionPostgresRejectsInvalidScopeAsCallerError(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	principal := auth.Actor{
		OrgID: fixture.orgID,
		Kind:  auth.ActorKindSession,
		Role:  auth.RoleDeveloper,
	}
	recorder := httptest.NewRecorder()
	fixture.server.closeSessionHTTP(
		recorder,
		sessionClosePostgresRequest(
			`{}`,
			principal,
			uuid.Must(uuid.NewV7()).String(),
			uuid.Must(uuid.NewV7()).String(),
			uuid.Must(uuid.NewV7()).String(),
		),
	)
	if recorder.Code != http.StatusBadRequest ||
		decodeHTTPError(t, recorder.Body.Bytes()).Code != "invalid_session_close" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func sessionClosePostgresRequest(
	body string,
	principal auth.Actor,
	projectID string,
	environmentID string,
	sessionID string,
) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	route := chi.NewRouteContext()
	if projectID != "" {
		route.URLParams.Add("projectID", projectID)
	}
	if environmentID != "" {
		route.URLParams.Add("environmentID", environmentID)
	}
	route.URLParams.Add("sessionID", sessionID)
	ctx := context.WithValue(request.Context(), chi.RouteCtxKey, route)
	ctx = context.WithValue(ctx, actorContextKey{}, principal)
	return request.WithContext(ctx)
}

func settleActorBootRun(
	t *testing.T,
	fixture actorStartPostgresFixture,
	started actorStartResult,
	committedInputSequence int64,
) {
	t.Helper()
	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE run_attempts
		   SET entrypoint_entered_at = now(),
		       terminal_session_input_sequence = $2,
		       terminal_outcome = 'succeeded',
		       terminal_reason_code = 'completed',
		       terminal_at = now()
		 WHERE run_id = $1
		   AND number = 1
	`, started.BootRunID, committedInputSequence); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE runs
		   SET status = 'succeeded',
		       terminal_at = now(),
		       updated_at = now()
		 WHERE id = $1
	`, started.BootRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE sessions
		   SET current_run_id = NULL,
		       committed_input_sequence = $2,
		       run_generation = run_generation + 1,
		       state_version = state_version + 1,
		       updated_at = now()
		 WHERE id = $1
	`, started.SessionID, committedInputSequence); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func assertActorCloseContinuation(
	t *testing.T,
	fixture actorStartPostgresFixture,
	actorID uuid.UUID,
) {
	t.Helper()
	var state string
	var closeSequence int64
	var manualRunCancelled bool
	var currentRunID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state, close_sequence, manual_run_cancelled, current_run_id
		  FROM sessions
		 WHERE id = $1
	`, actorID).Scan(&state, &closeSequence, &manualRunCancelled, &currentRunID); err != nil {
		t.Fatal(err)
	}
	var continuations, closeIntents int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM runs WHERE session_id = $1 AND cause_kind = 'continuation'),
		    (SELECT count(*) FROM outbox_messages
		      WHERE topic = 'session.close.reconcile'
		        AND payload->>'sessionId' = $1::text)
	`, actorID).Scan(
		&continuations,
		&closeIntents,
	); err != nil {
		t.Fatal(err)
	}
	if state != "closing" || closeSequence != 1 || manualRunCancelled ||
		currentRunID == uuid.Nil || continuations != 1 || closeIntents != 1 {
		t.Fatalf(
			"closing state=%s boundary=%d manual=%v current=%s continuations=%d close=%d",
			state, closeSequence, manualRunCancelled, currentRunID,
			continuations, closeIntents,
		)
	}
}
