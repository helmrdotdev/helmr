package controlplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWorkerRunLeaseClaimAuthorizesTransitionsAndReplays(t *testing.T) {
	server, store, worker, requestBody := newWorkerRunLeaseClaimHTTPFixture(t)
	handler := requireWorkerRole(
		auth.WorkerRoleRun,
		http.HandlerFunc(server.workerClaimRunLease),
	)

	unauthorized := runWorkerLeaseClaimRequest(
		handler,
		workerActor{Roles: []string{auth.WorkerRoleBuild}},
		requestBody,
	)
	if unauthorized.Code != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d body=%s", unauthorized.Code, unauthorized.Body)
	}
	if len(store.calls) != 0 {
		t.Fatalf("unauthorized request reached claim store: %v", store.calls)
	}

	worker.Roles = []string{auth.WorkerRoleRun}
	first := runWorkerLeaseClaimRequest(handler, worker, requestBody)
	if first.Code != http.StatusOK {
		t.Fatalf("first claim status = %d body=%s", first.Code, first.Body)
	}
	if store.authority.runLease.State != db.RunLeaseStateStarting {
		t.Fatalf("lease state = %q, want starting", store.authority.runLease.State)
	}
	if got := countCall(store.calls, "mark_starting"); got != 1 {
		t.Fatalf("mark starting calls = %d, want 1: %v", got, store.calls)
	}
	if commit := slices.Index(store.calls, "commit"); commit < 0 ||
		slices.Index(store.calls, "program") < commit {
		t.Fatalf("Program projection did not occur after commit: %v", store.calls)
	}

	store.authority.worker.State = db.WorkerInstanceStateDraining
	replay := runWorkerLeaseClaimRequest(handler, worker, requestBody)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d body=%s", replay.Code, replay.Body)
	}
	if !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatalf("replay response changed:\nfirst=%s\nreplay=%s", first.Body, replay.Body)
	}
	if got := countCall(store.calls, "mark_starting"); got != 1 {
		t.Fatalf("replay transitioned Lease again: %v", store.calls)
	}
}

func TestWorkerRunLeaseClaimRemainsReplayableAfterProjectionFailure(t *testing.T) {
	server, store, worker, requestBody := newWorkerRunLeaseClaimHTTPFixture(t)
	handler := requireWorkerRole(
		auth.WorkerRoleRun,
		http.HandlerFunc(server.workerClaimRunLease),
	)
	worker.Roles = []string{auth.WorkerRoleRun}
	store.projectionErr = errors.New("projection unavailable")

	failed := runWorkerLeaseClaimRequest(handler, worker, requestBody)
	if failed.Code != http.StatusInternalServerError {
		t.Fatalf("failed projection status = %d body=%s", failed.Code, failed.Body)
	}
	if store.authority.runLease.State != db.RunLeaseStateStarting {
		t.Fatalf("lease state after projection failure = %q, want starting", store.authority.runLease.State)
	}
	if got := countCall(store.calls, "mark_starting"); got != 1 {
		t.Fatalf("mark starting calls = %d, want 1: %v", got, store.calls)
	}

	store.projectionErr = nil
	replay := runWorkerLeaseClaimRequest(handler, worker, requestBody)
	if replay.Code != http.StatusOK {
		t.Fatalf("replay status = %d body=%s", replay.Code, replay.Body)
	}
	if got := countCall(store.calls, "mark_starting"); got != 1 {
		t.Fatalf("replay transitioned Lease again: %v", store.calls)
	}
}

func newWorkerRunLeaseClaimHTTPFixture(
	t *testing.T,
) (*Server, *runLeaseClaimStore, workerActor, []byte) {
	t.Helper()
	worker, locators, authority := validRunLeaseClaimFixture()
	logicalRun, logicalAttempt, definition := validTaskProgramStart(
		t,
		deployment.SchemaKindNone,
	)
	authority.run.EnvironmentID = locators.EnvironmentID
	authority.run.EntrypointDeclaredID = logicalRun.EntrypointDeclaredID
	authority.run.CauseKind = logicalRun.CauseKind
	authority.run.MaxActiveDurationMs = 300_000
	authority.run.ActiveElapsedMs = 12
	authority.attempt.EntrypointKind = logicalAttempt.EntrypointKind
	authority.workspaceMount.RuntimeInstanceID = authority.runtime.ID
	authority.workspaceLease.WorkspaceID = authority.workspace.ID
	authority.workspaceLease.WorkspaceMountID = authority.workspaceMount.ID
	authority.workspaceLease.RuntimeInstanceID = authority.runtime.ID
	authority.runLease.StartDeadlineAt = pgtype.Timestamptz{
		Time:  time.Unix(1_700_000_000, 0).UTC(),
		Valid: true,
	}
	authority.runLease.ExpiresAt = pgtype.Timestamptz{
		Time:  time.Unix(1_700_000_300, 0).UTC(),
		Valid: true,
	}
	definition.ID = authority.run.DeploymentDefinitionID
	definition.EnvironmentID = authority.run.EnvironmentID
	definition.DeploymentID = authority.run.DeploymentID
	definition.DeclaredID = authority.run.EntrypointDeclaredID

	key, err := workspace.NewFencingKey(make([]byte, workspace.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	capability, err := deriveWorkspaceCapabilityInput(key, authority.workspaceLease)
	if err != nil {
		t.Fatal(err)
	}
	authority.workspaceLease.FencingTokenHash = capability.Hash

	runtime := claimResponseRuntimeDescriptor()
	runtimeDigest, err := deployment.RuntimeDigestBytes(runtime.Digest)
	if err != nil {
		t.Fatal(err)
	}
	store := &runLeaseClaimStore{
		authority: authority,
		locators:  locators,
		program: db.GetDeploymentProgramAuthorityRow{
			DeploymentID:             authority.run.DeploymentID,
			EnvironmentID:            authority.run.EnvironmentID,
			DeploymentVersion:        "v42",
			BuildRuntimeDigest:       runtimeDigest,
			ProgramArtifactDigest:    validDigest('a'),
			ProgramArtifactSizeBytes: 100,
			ProgramArtifactMediaType: deployment.ProgramArtifactMediaType,
			BuildContract:            deployment.ProgramBuildContract,
			ProgramIndexDigest:       validDigestBytes(t, 'b'),
		},
		definition: definition,
		resetTarget: validWorkspaceResetTargetAuthority(runLeaseProjectionAuthority{
			workspaceLease: authority.workspaceLease,
		}),
	}
	server := &Server{
		log:                 discardTestLogger(),
		db:                  store,
		buildPolicy:         controlPlaneBuildPolicy(t),
		platformStore:       claimResponsePlatformStore{},
		secretDelivery:      &recordingSecretDeliveryOpener{},
		workspaceFencingKey: key,
	}
	requestBody := []byte(
		`{"lease_id":"` +
			pgvalue.UUIDString(authority.runLease.ID) +
			`","lease_sequence":1}`,
	)
	return server, store, worker, requestBody
}

func runWorkerLeaseClaimRequest(
	handler http.Handler,
	worker workerActor,
	body []byte,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(
		http.MethodPost,
		"/api/worker/v0/run/leases/claim",
		bytes.NewReader(body),
	)
	request = request.WithContext(context.WithValue(
		request.Context(),
		workerContextKey{},
		worker,
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func countCall(calls []string, target string) int {
	count := 0
	for _, call := range calls {
		if call == target {
			count++
		}
	}
	return count
}
