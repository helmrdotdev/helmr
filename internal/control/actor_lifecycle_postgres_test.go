package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/runadmission"
)

func TestActorClosePostgresClosesIdleActorAndReplaysBoundedReceipt(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	settleActorBootRun(t, fixture, started, 0)
	actorPublicID := actorPublicIDForTest(t, fixture, started.ActorID)
	request := actorCloseRequest{
		EnvironmentID: fixture.environmentID, ActorID: started.ActorID,
		ActorPublicID: actorPublicID, WorkspaceID: fixture.workspaceIDs[0],
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
	if closed.ActorID != actorPublicID || closed.AcceptedAt.IsZero() {
		t.Fatalf("closed receipt = %+v", closed)
	}

	var state string
	var currentRunID *uuid.UUID
	var closeSequence int64
	var closedAtValid bool
	var ownerActorID *uuid.UUID
	var ownershipGeneration int64
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT state, current_run_id, close_sequence, closed_at IS NOT NULL
		  FROM actors
		 WHERE id = $1
	`, started.ActorID).Scan(&state, &currentRunID, &closeSequence, &closedAtValid); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT owner_actor_id, ownership_generation
		  FROM workspaces
		 WHERE id = $1
	`, fixture.workspaceIDs[0]).Scan(&ownerActorID, &ownershipGeneration); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || currentRunID != nil || closeSequence != 0 ||
		!closedAtValid || ownerActorID != nil ||
		ownershipGeneration != priorOwnershipGeneration+1 {
		t.Fatalf(
			"closed state=%s current=%v boundary=%d closedAt=%v owner=%v generation=%d",
			state, currentRunID, closeSequence, closedAtValid, ownerActorID, ownershipGeneration,
		)
	}

	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors SET state_version = state_version + 1 WHERE id = $1
	`, started.ActorID); err != nil {
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
		 WHERE operation = 'actor.close'
		   AND state = 'completed'
	`).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(receipt, &members); err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members["actor_id"] == nil ||
		members["accepted_at"] == nil ||
		len(receipt) > 256 {
		t.Fatalf("claim receipt = %s", receipt)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE idempotency_claims
		   SET receipt = jsonb_set(
		       receipt,
		       '{actor_id}',
		       '"act_bbbbbbbbbbbbbbbbbbbbbbbbbb"'::jsonb
		   )
		 WHERE operation = 'actor.close'
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
		UPDATE actors SET manual_run_cancelled = true WHERE id = $1
	`, started.ActorID); err != nil {
		t.Fatal(err)
	}
	actorPublicID := actorPublicIDForTest(t, fixture, started.ActorID)
	closeRequest := actorCloseRequest{
		EnvironmentID: fixture.environmentID, ActorID: started.ActorID,
		ActorPublicID: actorPublicID, WorkspaceID: fixture.workspaceIDs[0],
		IdempotencyKey: "close-backlog-1",
	}

	closing, err := fixture.server.closeActor(t.Context(), closeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if closing.ActorID != actorPublicID || closing.AcceptedAt.IsZero() {
		t.Fatalf("closing receipt = %+v", closing)
	}
	assertActorCloseContinuation(t, fixture, started.ActorID)

	replayed, err := fixture.server.closeActor(t.Context(), closeRequest)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != closing {
		t.Fatalf("replayed receipt = %+v, closing = %+v", replayed, closing)
	}
	assertActorCloseContinuation(t, fixture, started.ActorID)
}

func TestActorClosePostgresRejectsFailedActorWithoutClaimResidue(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	settleActorBootRun(t, fixture, started, 0)
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE actors
		   SET state = 'failed',
		       failure_code = 'run-failed',
		       failure_run_id = $2,
		       failed_at = now()
		 WHERE id = $1
	`, started.ActorID, started.BootRunID); err != nil {
		t.Fatal(err)
	}
	actorPublicID := actorPublicIDForTest(t, fixture, started.ActorID)
	_, err = fixture.server.closeActor(t.Context(), actorCloseRequest{
		EnvironmentID: fixture.environmentID, ActorID: started.ActorID,
		ActorPublicID: actorPublicID, WorkspaceID: fixture.workspaceIDs[0],
		IdempotencyKey: "close-failed-1",
	})
	if !errors.Is(err, errActorCloseConflict) {
		t.Fatalf("close failed Actor error = %v", err)
	}
	var claims int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.close'
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
	actorPublicID := actorPublicIDForTest(t, fixture, started.ActorID)
	receipt, err := fixture.server.closeActor(t.Context(), actorCloseRequest{
		EnvironmentID: fixture.environmentID, ActorID: started.ActorID,
		ActorPublicID: actorPublicID, WorkspaceID: fixture.workspaceIDs[0],
		IdempotencyKey: "close-deferred-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.ActorID != actorPublicID || receipt.AcceptedAt.IsZero() {
		t.Fatalf("receipt = %+v", receipt)
	}
	var state string
	var ownerActorID uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT actors.state, workspaces.owner_actor_id
		  FROM actors
		  JOIN workspaces ON workspaces.id = actors.workspace_id
		 WHERE actors.id = $1
	`, started.ActorID).Scan(&state, &ownerActorID); err != nil {
		t.Fatal(err)
	}
	if state != "closing" || ownerActorID != started.ActorID {
		t.Fatalf("deferred state=%s owner=%s", state, ownerActorID)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE workspaces SET dirty_state = 'clean' WHERE id = $1
	`, fixture.workspaceIDs[0]); err != nil {
		t.Fatal(err)
	}
	reconciler, err := runadmission.NewActorInputReconciler(fixture.pool)
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := reconciler.ReconcileLifecycle(
		t.Context(),
		fixture.environmentID,
		started.ActorID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if deferred {
		t.Fatal("reconciliation remained deferred after Workspace recovery")
	}
	var ownerAfter *uuid.UUID
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT actors.state, workspaces.owner_actor_id
		  FROM actors
		  JOIN workspaces ON workspaces.id = actors.workspace_id
		 WHERE actors.id = $1
	`, started.ActorID).Scan(&state, &ownerAfter); err != nil {
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
	actorPublicID := actorPublicIDForTest(t, fixture, started.ActorID)
	body := `{"actor_id":"` + actorPublicID + `","idempotency_key":"http-close-1"}`
	principal := auth.Actor{
		OrgID: fixture.orgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
		ProjectID: fixture.projectID.String(), EnvironmentID: fixture.environmentID.String(),
	}
	recorder := httptest.NewRecorder()
	fixture.server.closeActorHTTP(
		recorder,
		actorStartHTTPPostgresRequest(body, principal, "", "", "operator.v1"),
	)
	if recorder.Code != http.StatusForbidden ||
		!strings.Contains(recorder.Body.String(), `"code":"permission_required"`) {
		t.Fatalf("denied status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var deniedState string
	var deniedClaims int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT actors.state,
		       (SELECT count(*) FROM idempotency_claims WHERE operation = 'actor.close')
		  FROM actors
		 WHERE actors.id = $1
	`, started.ActorID).Scan(&deniedState, &deniedClaims); err != nil {
		t.Fatal(err)
	}
	if deniedState != "open" || deniedClaims != 0 {
		t.Fatalf("denied residue state=%s claims=%d", deniedState, deniedClaims)
	}
	principal.Permissions = []auth.Permission{auth.PermissionActorsLifecycleManage}
	recorder = httptest.NewRecorder()
	fixture.server.closeActorHTTP(
		recorder,
		actorStartHTTPPostgresRequest(body, principal, "", "", "operator.v1"),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var receipt api.ActorOperationReceipt
	if err := json.Unmarshal(recorder.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ActorID != actorPublicID || receipt.AcceptedAt.IsZero() {
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
	fixture.server.closeActorHTTP(
		recorder,
		actorStartHTTPPostgresRequest(
			`{"actor_key":"missing"}`,
			principal,
			uuid.Must(uuid.NewV7()).String(),
			uuid.Must(uuid.NewV7()).String(),
			"operator.v1",
		),
	)
	if recorder.Code != http.StatusBadRequest ||
		!strings.Contains(recorder.Body.String(), `"code":"invalid_actor_close"`) ||
		!strings.Contains(recorder.Body.String(), `"retryable":false`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
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
		       terminal_actor_input_sequence = $2,
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
		UPDATE actors
		   SET current_run_id = NULL,
		       committed_input_sequence = $2,
		       run_generation = run_generation + 1,
		       state_version = state_version + 1,
		       updated_at = now()
		 WHERE id = $1
	`, started.ActorID, committedInputSequence); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func actorPublicIDForTest(
	t *testing.T,
	fixture actorStartPostgresFixture,
	actorID uuid.UUID,
) string {
	t.Helper()
	var publicID string
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT public_id FROM actors WHERE id = $1
	`, actorID).Scan(&publicID); err != nil {
		t.Fatal(err)
	}
	return publicID
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
		  FROM actors
		 WHERE id = $1
	`, actorID).Scan(&state, &closeSequence, &manualRunCancelled, &currentRunID); err != nil {
		t.Fatal(err)
	}
	var continuations, admissionIntents, lifecycleIntents int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT
		    (SELECT count(*) FROM runs WHERE actor_id = $1 AND cause_kind = 'continuation'),
		    (SELECT count(*) FROM outbox_messages
		      WHERE topic = 'run.admit' AND payload->>'runId' = $2::text),
		    (SELECT count(*) FROM outbox_messages
		      WHERE topic = 'actor.lifecycle.reconcile'
		        AND payload->>'actorId' = $1::text)
	`, actorID, currentRunID).Scan(
		&continuations,
		&admissionIntents,
		&lifecycleIntents,
	); err != nil {
		t.Fatal(err)
	}
	if state != "closing" || closeSequence != 1 || manualRunCancelled ||
		currentRunID == uuid.Nil || continuations != 1 ||
		admissionIntents != 1 || lifecycleIntents != 1 {
		t.Fatalf(
			"closing state=%s boundary=%d manual=%v current=%s continuations=%d admission=%d lifecycle=%d",
			state, closeSequence, manualRunCancelled, currentRunID,
			continuations, admissionIntents, lifecycleIntents,
		)
	}
}
