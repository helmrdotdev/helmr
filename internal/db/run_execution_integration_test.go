package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRunExecutionFencingRequeuesExpiredClaimWithoutPoisoningNextClaim(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	oldExecutionID := ids.ToPG(ids.New())

	oldClaim, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    oldExecutionID,
		WorkerID:       "worker-old",
		LeaseExpiresAt: pgTime(time.Now().Add(-time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.RequeueExpiredClaimedRunExecutions(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != db.RunStatusQueued || requeued.CurrentExecutionID.Valid {
		t.Fatalf("requeued run = %+v", requeued)
	}

	fresh, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-fresh",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.RequeueExpiredClaimedRunExecutions(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != db.RunStatusClaimed || claimed.CurrentExecutionID != fresh.ExecutionID {
		t.Fatalf("fresh claim was poisoned by old execution: run=%+v fresh=%+v", claimed, fresh)
	}

	_, err = queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  oldExecutionID,
		WorkerPoolID: oldClaim.ExecutionWorkerPoolID,
		WorkerID:     "worker-old",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale start err = %v", err)
	}
	_, err = queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(ids.DefaultOrgID),
		RunID:                run.ID,
		ExecutionID:          oldExecutionID,
		WorkerPoolID:         oldClaim.ExecutionWorkerPoolID,
		WorkerID:             "worker-old",
		Status:               db.RunStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		TerminalEventKind:    "run.completed",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale release err = %v", err)
	}

	var lostStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM run_executions WHERE id = $1`, oldExecutionID).Scan(&lostStatus); err != nil {
		t.Fatal(err)
	}
	if lostStatus != db.RunExecutionStatusLost {
		t.Fatalf("old execution status = %s", lostStatus)
	}
}

func TestCreateRunRollsBackWhenCreatedEventInsertFails(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	seedRunDependencies(t, ctx, queries)
	deployedTask := getActiveTestDeployedTask(t, ctx, queries)
	runID := ids.ToPG(ids.New())

	_, err := queries.CreateRun(ctx, db.CreateRunParams{
		ID:                          runID,
		OrgID:                       ids.ToPG(ids.DefaultOrgID),
		TaskDeploymentID:            deployedTask.DeploymentID,
		DeployedTaskID:              deployedTask.ID,
		TaskID:                      "deploy",
		Payload:                     []byte(`{}`),
		SecretBindings:              []byte(`{}`),
		WorkspaceRepository:         "helmrdotdev/helmr",
		WorkspaceInstallationID:     123,
		WorkspaceGithubRepositoryID: 456,
		WorkspaceRef:                "main",
		WorkspaceSha:                "0123456789abcdef0123456789abcdef01234567",
		WorkspaceSubpath:            "",
		MaxDurationSeconds:          3600,
		EventPayload:                []byte(`{`),
	})
	if err == nil {
		t.Fatal("CreateRun succeeded with invalid event payload")
	}
	assertNoRowsForRun(t, ctx, pool, runID)
}

func TestMarkGitHubRepositoryDeletedDisablesProjectWorkspaceRepositoryAccess(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	seedRunDependencies(t, ctx, queries)
	scope, err := queries.GetDefaultProjectEnvironment(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := queries.GetActiveProjectWorkspaceRepositoryAccess(ctx, db.GetActiveProjectWorkspaceRepositoryAccessParams{
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		ProjectID:          scope.ProjectID,
		GithubRepositoryID: 456,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkGitHubRepositoryDeleted(ctx, db.MarkGitHubRepositoryDeletedParams{
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		InstallationID:     123,
		GithubRepositoryID: 456,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertGitHubRepository(ctx, db.UpsertGitHubRepositoryParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		InstallationID:     123,
		GithubRepositoryID: 456,
		OwnerLogin:         "helmrdotdev",
		Name:               "helmr",
		FullName:           "helmrdotdev/helmr",
		DefaultBranch:      pgtype.Text{String: "main", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetActiveProjectWorkspaceRepositoryAccess(ctx, db.GetActiveProjectWorkspaceRepositoryAccessParams{
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		ProjectID:          scope.ProjectID,
		GithubRepositoryID: 456,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("active project workspace repository after repository re-add err = %v, want pgx.ErrNoRows", err)
	}
	var activeConnections, activeProjectWorkspaceRepositories int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM github_repository_connections WHERE org_id = $1 AND github_repository_id = 456 AND disabled_at IS NULL`, ids.ToPG(ids.DefaultOrgID)).Scan(&activeConnections); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_workspace_repositories WHERE org_id = $1 AND github_repository_id = 456 AND disabled_at IS NULL`, ids.ToPG(ids.DefaultOrgID)).Scan(&activeProjectWorkspaceRepositories); err != nil {
		t.Fatal(err)
	}
	if activeConnections != 0 || activeProjectWorkspaceRepositories != 0 {
		t.Fatalf("active connections=%d active project workspace repositories=%d", activeConnections, activeProjectWorkspaceRepositories)
	}
}

func TestDisableGitHubRepositoryConnectionDisablesProjectWorkspaceRepositoryAccess(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	seedRunDependencies(t, ctx, queries)
	scope, err := queries.GetDefaultProjectEnvironment(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := queries.DisableGitHubRepositoryConnection(ctx, db.DisableGitHubRepositoryConnectionParams{
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		GithubRepositoryID: 456,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnableGitHubRepositoryConnection(ctx, db.EnableGitHubRepositoryConnectionParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		GithubRepositoryID: 456,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetActiveProjectWorkspaceRepositoryAccess(ctx, db.GetActiveProjectWorkspaceRepositoryAccessParams{
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		ProjectID:          scope.ProjectID,
		GithubRepositoryID: 456,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("active project workspace repository after connection re-enable err = %v, want pgx.ErrNoRows", err)
	}
	var activeProjectWorkspaceRepositories int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM project_workspace_repositories WHERE org_id = $1 AND github_repository_id = 456 AND disabled_at IS NULL`, ids.ToPG(ids.DefaultOrgID)).Scan(&activeProjectWorkspaceRepositories); err != nil {
		t.Fatal(err)
	}
	if activeProjectWorkspaceRepositories != 0 {
		t.Fatalf("active project workspace repositories=%d, want 0", activeProjectWorkspaceRepositories)
	}
}

func TestProjectWorkspaceRepositoryAccessAllowsMultipleRepositoriesPerProject(t *testing.T) {
	ctx := context.Background()
	queries, _ := newPostgresTestDB(t, ctx)
	seedRunDependencies(t, ctx, queries)
	scope, err := queries.GetDefaultProjectEnvironment(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertGitHubRepository(ctx, db.UpsertGitHubRepositoryParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		InstallationID:     123,
		GithubRepositoryID: 789,
		OwnerLogin:         "helmrdotdev",
		Name:               "worker",
		FullName:           "helmrdotdev/worker",
		DefaultBranch:      pgtype.Text{String: "main", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnableGitHubRepositoryConnection(ctx, db.EnableGitHubRepositoryConnectionParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		GithubRepositoryID: 789,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnableProjectWorkspaceRepositoryAccess(ctx, db.EnableProjectWorkspaceRepositoryAccessParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		ProjectID:          scope.ProjectID,
		GithubRepositoryID: 789,
	}); err != nil {
		t.Fatal(err)
	}
	repositories, err := queries.ListActiveProjectWorkspaceRepositoryAccess(ctx, db.ListActiveProjectWorkspaceRepositoryAccessParams{
		OrgID:     ids.ToPG(ids.DefaultOrgID),
		ProjectID: scope.ProjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 2 {
		t.Fatalf("active project workspace repositories = %d, want 2", len(repositories))
	}
	if _, err := queries.DisableProjectWorkspaceRepositoryAccess(ctx, db.DisableProjectWorkspaceRepositoryAccessParams{
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		ProjectID:          scope.ProjectID,
		GithubRepositoryID: 789,
	}); err != nil {
		t.Fatal(err)
	}
	repositories, err = queries.ListActiveProjectWorkspaceRepositoryAccess(ctx, db.ListActiveProjectWorkspaceRepositoryAccessParams{
		OrgID:     ids.ToPG(ids.DefaultOrgID),
		ProjectID: scope.ProjectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repositories) != 1 {
		t.Fatalf("active project workspace repositories after disable = %d, want 1", len(repositories))
	}
}

func TestWorkerRegistrationCredentialsCanBeRevoked(t *testing.T) {
	ctx := context.Background()
	queries, _ := newPostgresTestDB(t, ctx)
	if err := queries.EnsureDefaultOrganization(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	registrationHash := []byte("registration-hash")
	firstSecretHash := []byte("first-secret-hash")
	secondSecretHash := []byte("second-secret-hash")
	if _, err := queries.EnsureDefaultWorkerPoolRegistrationToken(ctx, db.EnsureDefaultWorkerPoolRegistrationTokenParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     ids.ToPG(ids.DefaultOrgID),
		TokenHash: registrationHash,
	}); err != nil {
		t.Fatal(err)
	}
	credential, err := queries.CreateWorkerCredentialFromRegistration(ctx, db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: registrationHash,
		CredentialID:          ids.ToPG(ids.New()),
		WorkerID:              "worker-1",
		KeyPrefix:             "lmw_secr",
		SecretHash:            firstSecretHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateWorkerCredentialFromRegistration(ctx, db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: registrationHash,
		CredentialID:          ids.ToPG(ids.New()),
		WorkerID:              "worker-2",
		KeyPrefix:             "lmw_secr",
		SecretHash:            secondSecretHash,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = queries.CreateWorkerCredentialFromRegistration(ctx, db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: []byte("wrong-registration-hash"),
		CredentialID:          ids.ToPG(ids.New()),
		WorkerID:              "worker-3",
		KeyPrefix:             "lmw_secr",
		SecretHash:            []byte("third-secret-hash"),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("invalid registration err = %v", err)
	}
	if _, err := queries.AuthenticateWorkerCredential(ctx, db.AuthenticateWorkerCredentialParams{
		WorkerID:   credential.WorkerID,
		SecretHash: firstSecretHash,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AuthorizeWorkerCredential(ctx, db.AuthorizeWorkerCredentialParams{
		CredentialID: credential.ID,
		OrgID:        credential.OrgID,
		WorkerID:     credential.WorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := queries.RevokeWorkerCredential(ctx, db.RevokeWorkerCredentialParams{
		OrgID:        credential.OrgID,
		WorkerID:     credential.WorkerID,
		CredentialID: credential.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("revoked rows = %d", rows)
	}
	if _, err := queries.AuthenticateWorkerCredential(ctx, db.AuthenticateWorkerCredentialParams{
		WorkerID:   credential.WorkerID,
		SecretHash: firstSecretHash,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("authenticate revoked err = %v", err)
	}
	if _, err := queries.AuthorizeWorkerCredential(ctx, db.AuthorizeWorkerCredentialParams{
		CredentialID: credential.ID,
		OrgID:        credential.OrgID,
		WorkerID:     credential.WorkerID,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("authorize revoked err = %v", err)
	}
}

func TestDefaultWorkerPoolRegistrationTokenRotationRevokesPreviousToken(t *testing.T) {
	ctx := context.Background()
	queries, _ := newPostgresTestDB(t, ctx)
	if err := queries.EnsureDefaultOrganization(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	oldHash := []byte("old-registration-hash")
	newHash := []byte("new-registration-hash")
	if _, err := queries.EnsureDefaultWorkerPoolRegistrationToken(ctx, db.EnsureDefaultWorkerPoolRegistrationTokenParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     ids.ToPG(ids.DefaultOrgID),
		TokenHash: oldHash,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnsureDefaultWorkerPoolRegistrationToken(ctx, db.EnsureDefaultWorkerPoolRegistrationTokenParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     ids.ToPG(ids.DefaultOrgID),
		TokenHash: newHash,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateWorkerCredentialFromRegistration(ctx, db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: oldHash,
		CredentialID:          ids.ToPG(ids.New()),
		WorkerID:              "worker-old-token",
		KeyPrefix:             "lmw_secr",
		SecretHash:            []byte("old-secret-hash"),
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old registration token err = %v", err)
	}
	if _, err := queries.CreateWorkerCredentialFromRegistration(ctx, db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: newHash,
		CredentialID:          ids.ToPG(ids.New()),
		WorkerID:              "worker-new-token",
		KeyPrefix:             "lmw_secr",
		SecretHash:            []byte("new-secret-hash"),
	}); err != nil {
		t.Fatalf("new registration token err = %v", err)
	}
}

func TestAbandonClaimedRunExecutionRequeuesBeforeStart(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	executionID := ids.ToPG(ids.New())

	claim, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    executionID,
		WorkerID:       "worker-1",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.AbandonClaimedRunExecution(ctx, db.AbandonClaimedRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != db.RunStatusQueued || requeued.CurrentExecutionID.Valid {
		t.Fatalf("requeued run = %+v", requeued)
	}
	var executionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM run_executions WHERE id = $1`, executionID).Scan(&executionStatus); err != nil {
		t.Fatal(err)
	}
	if executionStatus != db.RunExecutionStatusLost {
		t.Fatalf("execution status = %s", executionStatus)
	}
	_, err = queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  executionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     "worker-1",
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("abandoned start err = %v", err)
	}
}

func TestAbandonClaimedRunExecutionDoesNotRequeueStartedExecution(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)

	if err := queries.AbandonClaimedRunExecution(ctx, db.AbandonClaimedRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	running, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != db.RunStatusRunning || running.CurrentExecutionID != claim.ExecutionID {
		t.Fatalf("running run = %+v", running)
	}
	var executionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM run_executions WHERE id = $1`, claim.ExecutionID).Scan(&executionStatus); err != nil {
		t.Fatal(err)
	}
	if executionStatus != db.RunExecutionStatusRunning {
		t.Fatalf("execution status = %s", executionStatus)
	}
}

func TestReleaseRunExecutionRollsBackWhenTerminalEventInsertFails(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)

	_, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(ids.DefaultOrgID),
		RunID:                run.ID,
		ExecutionID:          claim.ExecutionID,
		WorkerPoolID:         claim.ExecutionWorkerPoolID,
		WorkerID:             claim.ExecutionWorkerID,
		Status:               db.RunStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		TerminalEventKind:    "",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	})
	if err == nil {
		t.Fatal("ReleaseRunExecution succeeded with invalid terminal event kind")
	}

	current, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != db.RunStatusRunning || current.CurrentExecutionID != claim.ExecutionID || current.FinishedAt.Valid {
		t.Fatalf("release was not rolled back: %+v", current)
	}
	var executionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM run_executions WHERE id = $1`, claim.ExecutionID).Scan(&executionStatus); err != nil {
		t.Fatal(err)
	}
	if executionStatus != db.RunExecutionStatusRunning {
		t.Fatalf("execution status = %s", executionStatus)
	}
	events, err := queries.ListRunEvents(ctx, db.ListRunEventsParams{
		OrgID: ids.ToPG(ids.DefaultOrgID),
		RunID: run.ID,
		ID:    0,
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "run.created" {
		t.Fatalf("events = %+v", events)
	}
}

func TestReleaseRunExecutionStoresOutput(t *testing.T) {
	ctx := context.Background()
	queries, _ := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)

	output := []byte(`{"ok":true,"count":2}`)
	released, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(ids.DefaultOrgID),
		RunID:                run.ID,
		ExecutionID:          claim.ExecutionID,
		WorkerPoolID:         claim.ExecutionWorkerPoolID,
		WorkerID:             claim.ExecutionWorkerID,
		Status:               db.RunStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		Output:               output,
		TerminalEventKind:    "run.completed",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, "released output", released.Output, output)
	current, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, "stored output", current.Output, output)
}

func assertJSONEqual(t *testing.T, label string, got, want []byte) {
	t.Helper()

	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s is not JSON: %v", label, err)
	}

	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("expected %s is not JSON: %v", label, err)
	}

	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("%s = %s, want %s", label, got, want)
	}
}

func TestClaimRunExecutionRespectsWorkerSlots(t *testing.T) {
	ctx := context.Background()
	queries, _ := newPostgresTestDB(t, ctx)
	first := seedQueuedRun(t, ctx, queries)
	second := seedQueuedRun(t, ctx, queries)
	workerID := "worker-slots"
	workerPoolID := defaultWorkerPoolID(t, ctx, queries)
	if err := upsertTestWorker(ctx, queries, ids.ToPG(ids.DefaultOrgID), workerPoolID, workerID); err != nil {
		t.Fatal(err)
	}
	claim, err := queries.ClaimRunExecution(ctx, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		WorkerPoolID:   workerPoolID,
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       workerID,
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if claim.ID != first.ID {
		t.Fatalf("first claim = %+v first=%s second=%s", claim, ids.MustFromPG(first.ID), ids.MustFromPG(second.ID))
	}
	_, err = queries.ClaimRunExecution(ctx, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		WorkerPoolID:   workerPoolID,
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       workerID,
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("second claim err = %v", err)
	}
}

func TestWorkerHeartbeatDoesNotReactivateDrainingWorker(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	_ = seedQueuedRun(t, ctx, queries)
	workerID := "worker-draining"
	workerPoolID := defaultWorkerPoolID(t, ctx, queries)
	if err := upsertTestWorker(ctx, queries, ids.ToPG(ids.DefaultOrgID), workerPoolID, workerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workers SET status = 'draining' WHERE org_id = $1 AND id = $2`, ids.ToPG(ids.DefaultOrgID), workerID); err != nil {
		t.Fatal(err)
	}
	if err := upsertTestWorker(ctx, queries, ids.ToPG(ids.DefaultOrgID), workerPoolID, workerID); err != nil {
		t.Fatal(err)
	}
	_, err := queries.ClaimRunExecution(ctx, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		WorkerPoolID:   workerPoolID,
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       workerID,
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("draining claim err = %v", err)
	}
}

func TestRunExecutionWorkerWritesRequireClaimedWorkerPool(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-reenrolled",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongPoolID := createTestWorkerPool(t, ctx, queries, "other-pool").ID
	if err := upsertTestWorker(ctx, queries, ids.ToPG(ids.DefaultOrgID), wrongPoolID, claim.ExecutionWorkerID); err != nil {
		t.Fatal(err)
	}
	assertNoRows := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s err = %v", name, err)
		}
	}

	_, err = queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: wrongPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	})
	assertNoRows("start with wrong pool", err)
	_, err = queries.RenewRunExecutionLease(ctx, db.RenewRunExecutionLeaseParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		RunID:          run.ID,
		ExecutionID:    claim.ExecutionID,
		WorkerPoolID:   wrongPoolID,
		WorkerID:       claim.ExecutionWorkerID,
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	assertNoRows("renew with wrong pool", err)
	_, err = queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: wrongPoolID,
		WorkerID:     claim.ExecutionWorkerID,
		Stream:       db.RunLogStreamStdout,
		ObservedSeq:  1,
		Content:      []byte("blocked"),
		Kind:         "log.stdout",
		Payload:      []byte(`{"blocked":true}`),
	})
	assertNoRows("append log with wrong pool", err)
	_, err = queries.AppendRunEventForExecution(ctx, db.AppendRunEventForExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: wrongPoolID,
		WorkerID:     claim.ExecutionWorkerID,
		Kind:         "emit.blocked",
		Payload:      []byte(`{"blocked":true}`),
	})
	assertNoRows("append event with wrong pool", err)
	_, err = queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     wrongPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     ids.ToPG(ids.New()),
		CheckpointReason: "wait_approval",
		ID:               ids.ToPG(ids.New()),
		CorrelationID:    "wrong-pool",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"prompt":"blocked"}`),
		DisplayText:      "blocked",
	})
	assertNoRows("create waitpoint with wrong pool", err)
	_, err = queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(ids.DefaultOrgID),
		RunID:                run.ID,
		ExecutionID:          claim.ExecutionID,
		WorkerPoolID:         wrongPoolID,
		WorkerID:             claim.ExecutionWorkerID,
		Status:               db.RunStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		TerminalEventKind:    "run.completed",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	})
	assertNoRows("release with wrong pool", err)
	if err := queries.AbandonClaimedRunExecution(ctx, db.AbandonClaimedRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: wrongPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	var status db.RunStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, run.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.RunStatusClaimed {
		t.Fatalf("wrong-pool abandon changed run status to %s", status)
	}
	_, err = queries.RenewRunExecutionLease(ctx, db.RenewRunExecutionLeaseParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		RunID:          run.ID,
		ExecutionID:    claim.ExecutionID,
		WorkerPoolID:   claim.ExecutionWorkerPoolID,
		WorkerID:       claim.ExecutionWorkerID,
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatalf("renew with claimed pool: %v", err)
	}
}

func TestRunExecutionFencingFailsExpiredRunningExecutionAndRejectsStaleRelease(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	executionID := ids.ToPG(ids.New())

	claim, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    executionID,
		WorkerID:       "worker-1",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_executions SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, executionID); err != nil {
		t.Fatal(err)
	}
	if err := queries.FailExpiredRunningRunExecutions(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}

	failed, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != db.RunStatusFailed || failed.CurrentExecutionID.Valid {
		t.Fatalf("failed run = %+v", failed)
	}
	events, err := queries.ListRunEvents(ctx, db.ListRunEventsParams{
		OrgID: ids.ToPG(ids.DefaultOrgID),
		RunID: run.ID,
		ID:    0,
		Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "run.created" || events[1].Kind != "run.failed" || !bytes.Contains(events[1].Payload, []byte("worker_lease_expired")) {
		t.Fatalf("events = %+v", events)
	}
	_, err = queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(ids.DefaultOrgID),
		RunID:                run.ID,
		ExecutionID:          executionID,
		WorkerPoolID:         claim.ExecutionWorkerPoolID,
		WorkerID:             "worker-1",
		Status:               db.RunStatusSucceeded,
		ExitCode:             pgtype.Int4{Int32: 0, Valid: true},
		TerminalEventKind:    "run.completed",
		TerminalEventPayload: []byte(`{"exit_code":0}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale release err = %v", err)
	}
}

func TestExpiredRunningExecutionInvalidatesCreatingCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)
	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     checkpointID,
		CheckpointReason: "wait_approval",
		ID:               waitpointID,
		CorrelationID:    "lease-expired",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"ship it"}`),
		DisplayText:      "ship it",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_executions SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, claim.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := queries.FailExpiredRunningRunExecutions(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	var waitpointStatus db.WaitpointStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM waitpoints WHERE id = $1`, waitpointID).Scan(&waitpointStatus); err != nil {
		t.Fatal(err)
	}
	if waitpointStatus != db.WaitpointStatusCancelled {
		t.Fatalf("waitpoint status = %s", waitpointStatus)
	}
	var checkpointStatus db.CheckpointStatus
	var checkpointError pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT status, error_message FROM checkpoints WHERE id = $1`, checkpointID).Scan(&checkpointStatus, &checkpointError); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusInvalid || !checkpointError.Valid || checkpointError.String != "worker lease expired" {
		t.Fatalf("checkpoint status=%s error=%+v", checkpointStatus, checkpointError)
	}
}

func TestReleaseRunExecutionCancelsOpenWaitpointAndInvalidatesCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)
	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     checkpointID,
		CheckpointReason: "wait_approval",
		ID:               waitpointID,
		CorrelationID:    "release-failed",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"ship it"}`),
		DisplayText:      "ship it",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(ids.DefaultOrgID),
		RunID:                run.ID,
		ExecutionID:          claim.ExecutionID,
		WorkerPoolID:         claim.ExecutionWorkerPoolID,
		WorkerID:             claim.ExecutionWorkerID,
		Status:               db.RunStatusFailed,
		ErrorMessage:         pgtype.Text{String: "snapshot upload failed", Valid: true},
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"worker_failed","detail":{"message":"snapshot upload failed"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	var waitpointStatus db.WaitpointStatus
	var waitpointReason []byte
	if err := pool.QueryRow(ctx, `SELECT status, resolution FROM waitpoints WHERE id = $1`, waitpointID).Scan(&waitpointStatus, &waitpointReason); err != nil {
		t.Fatal(err)
	}
	if waitpointStatus != db.WaitpointStatusCancelled || !strings.Contains(string(waitpointReason), "snapshot upload failed") {
		t.Fatalf("waitpoint status=%s reason=%s", waitpointStatus, waitpointReason)
	}
	var checkpointStatus db.CheckpointStatus
	var checkpointError pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT status, error_message FROM checkpoints WHERE id = $1`, checkpointID).Scan(&checkpointStatus, &checkpointError); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusInvalid || !checkpointError.Valid || checkpointError.String != "snapshot upload failed" {
		t.Fatalf("checkpoint status=%s error=%+v", checkpointStatus, checkpointError)
	}
}

func TestCheckpointReadyDetachesExecutionAndPreservesPendingWaitpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	executionID := ids.ToPG(ids.New())

	claim, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    executionID,
		WorkerID:       "worker-1",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     checkpointID,
		CheckpointReason: "wait_approval",
		ID:               waitpointID,
		CorrelationID:    "1",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"ship it"}`),
		DisplayText:      "ship it",
	}); err != nil {
		t.Fatal(err)
	}
	markCheckpointReadyWithActive(t, ctx, queries, run.ID, claim.ExecutionID, claim.ExecutionWorkerID, waitpointID, checkpointID, 1234)
	if _, err := pool.Exec(ctx, `UPDATE run_executions SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, executionID); err != nil {
		t.Fatal(err)
	}
	if err := queries.FailExpiredRunningRunExecutions(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	waiting, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if waiting.Status != db.RunStatusWaiting || waiting.CurrentExecutionID.Valid || waiting.LatestCheckpointID != checkpointID {
		t.Fatalf("waiting run = %+v", waiting)
	}
	var executionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM run_executions WHERE id = $1`, executionID).Scan(&executionStatus); err != nil {
		t.Fatal(err)
	}
	if executionStatus != db.RunExecutionStatusDetached {
		t.Fatalf("execution status = %s", executionStatus)
	}
	var status db.WaitpointStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM waitpoints WHERE id = $1`, waitpointID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.WaitpointStatusPending {
		t.Fatalf("waitpoint status = %s", status)
	}
	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		RunID:          run.ID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindApproval,
		ResolutionKind: pgtype.Text{String: "approved", Valid: true},
		Resolution:     []byte(`{"approved":true}`),
		Payload:        []byte(`{"resolution_kind":"approved"}`),
	}); err != nil {
		t.Fatal(err)
	}
	queued, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != db.RunStatusQueued || queued.CurrentExecutionID.Valid {
		t.Fatalf("queued run = %+v", queued)
	}
	restored, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-restore",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if restored.ID != run.ID || restored.ExecutionID == executionID {
		t.Fatalf("restored claim = %+v", restored)
	}
	if restored.ActiveDurationMs != 1234 {
		t.Fatalf("restored active duration ms = %d", restored.ActiveDurationMs)
	}
	var restoreCheckpointID pgtype.UUID
	var checkpointStatus db.CheckpointStatus
	if err := pool.QueryRow(ctx, `
		SELECT run_executions.restore_checkpoint_id, checkpoints.status
		  FROM run_executions
		  JOIN checkpoints ON checkpoints.id = run_executions.restore_checkpoint_id
		 WHERE run_executions.id = $1
	`, restored.ExecutionID).Scan(&restoreCheckpointID, &checkpointStatus); err != nil {
		t.Fatal(err)
	}
	if restoreCheckpointID != checkpointID || checkpointStatus != db.CheckpointStatusRestoring {
		t.Fatalf("restore checkpoint=%+v status=%s", restoreCheckpointID, checkpointStatus)
	}
	payload, err := queries.GetRunRestorePayload(ctx, db.GetRunRestorePayloadParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload.CheckpointID != checkpointID || payload.WaitpointID != waitpointID {
		t.Fatalf("restore payload = %+v", payload)
	}
}

func TestClaimRunExecutionFiltersRestoreCheckpointByWorkerRuntime(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run, _, checkpointID := seedResolvedCheckpoint(t, ctx, queries)

	workerPoolID := defaultWorkerPoolID(t, ctx, queries)
	if err := upsertTestWorker(ctx, queries, ids.ToPG(ids.DefaultOrgID), workerPoolID, "worker-mismatch"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workers SET max_vcpus = 4 WHERE id = 'worker-mismatch'`); err != nil {
		t.Fatal(err)
	}
	_, err := queries.ClaimRunExecution(ctx, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		WorkerPoolID:   workerPoolID,
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-mismatch",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mismatched worker claim err = %v", err)
	}

	claim, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-match",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	var restoreCheckpointID pgtype.UUID
	if err := pool.QueryRow(ctx, `SELECT restore_checkpoint_id FROM run_executions WHERE id = $1`, claim.ExecutionID).Scan(&restoreCheckpointID); err != nil {
		t.Fatal(err)
	}
	if claim.ID != run.ID || restoreCheckpointID != checkpointID {
		t.Fatalf("matched restore claim = %+v restore_checkpoint=%+v checkpoint=%+v", claim, restoreCheckpointID, checkpointID)
	}
}

func TestRunObservabilitySpansCheckpointRestoreExecutions(t *testing.T) {
	ctx := context.Background()
	queries, _ := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)

	if _, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
		Stream:       "stdout",
		ObservedSeq:  1,
		Content:      []byte("before\n"),
		Kind:         "log.stdout",
		Payload:      []byte(`{"phase":"before"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AppendRunEventForExecution(ctx, db.AppendRunEventForExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
		Kind:         "emit.before",
		Payload:      []byte(`{"phase":"before"}`),
	}); err != nil {
		t.Fatal(err)
	}

	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     checkpointID,
		CheckpointReason: "wait_approval",
		ID:               waitpointID,
		CorrelationID:    "observability",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"ship it"}`),
		DisplayText:      "ship it",
	}); err != nil {
		t.Fatal(err)
	}
	markCheckpointReady(t, ctx, queries, run.ID, claim.ExecutionID, claim.ExecutionWorkerID, waitpointID, checkpointID)

	_, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
		Stream:       "stdout",
		ObservedSeq:  2,
		Content:      []byte("stale\n"),
		Kind:         "log.stdout",
		Payload:      []byte(`{"phase":"stale"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale log append err = %v", err)
	}
	_, err = queries.AppendRunEventForExecution(ctx, db.AppendRunEventForExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
		Kind:         "emit.stale",
		Payload:      []byte(`{"phase":"stale"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale event append err = %v", err)
	}

	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		RunID:          run.ID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindApproval,
		ResolutionKind: pgtype.Text{String: "approved", Valid: true},
		Resolution:     []byte(`{"approved":true}`),
		Payload:        []byte(`{"resolution_kind":"approved"}`),
	}); err != nil {
		t.Fatal(err)
	}
	restored, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-restore",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
		Stream:       "stdout",
		ObservedSeq:  3,
		Content:      []byte("after\n"),
		Kind:         "log.stdout",
		Payload:      []byte(`{"phase":"after"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
		Stream:       "stderr",
		ObservedSeq:  4,
		Content:      []byte("after-error\n"),
		Kind:         "log.stderr",
		Payload:      []byte(`{"phase":"after"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.AppendRunEventForExecution(ctx, db.AppendRunEventForExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
		Kind:         "emit.after",
		Payload:      []byte(`{"phase":"after"}`),
	}); err != nil {
		t.Fatal(err)
	}
	logs, err := queries.GetRunLogSnapshot(ctx, db.GetRunLogSnapshotParams{
		StdoutLimit: 1024,
		StderrLimit: 1024,
		OrgID:       ids.ToPG(ids.DefaultOrgID),
		RunID:       run.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout := string(logs.Stdout); stdout != "before\nafter\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr := string(logs.Stderr); stderr != "after-error\n" {
		t.Fatalf("stderr = %q", stderr)
	}
	events, err := queries.ListRunEvents(ctx, db.ListRunEventsParams{
		OrgID: ids.ToPG(ids.DefaultOrgID),
		RunID: run.ID,
		Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	kindCounts := map[string]int{}
	for _, event := range events {
		kindCounts[event.Kind]++
	}
	if kindCounts["run.created"] != 1 || kindCounts["log.stdout"] != 2 || kindCounts["emit.before"] != 1 || kindCounts["emit.after"] != 1 {
		t.Fatalf("event counts = %+v", kindCounts)
	}
	if kindCounts["emit.stale"] != 0 {
		t.Fatalf("stale event was persisted: %+v", kindCounts)
	}
}

func TestRunLogSnapshotReadsTailAcrossChunks(t *testing.T) {
	ctx := context.Background()
	queries, _ := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)

	appendLog := func(stream db.RunLogStream, observedSeq int64, content string) {
		t.Helper()
		if _, err := queries.AppendRunLogChunk(ctx, db.AppendRunLogChunkParams{
			OrgID:        ids.ToPG(ids.DefaultOrgID),
			RunID:        run.ID,
			ExecutionID:  claim.ExecutionID,
			WorkerPoolID: claim.ExecutionWorkerPoolID,
			WorkerID:     claim.ExecutionWorkerID,
			Stream:       stream,
			ObservedSeq:  observedSeq,
			Content:      []byte(content),
			Kind:         "log." + string(stream),
			Payload:      []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendLog(db.RunLogStreamStdout, 1, "abc")
	appendLog(db.RunLogStreamStdout, 2, "def")
	appendLog(db.RunLogStreamStdout, 3, "ghi")
	appendLog(db.RunLogStreamStderr, 4, "ERR1")
	appendLog(db.RunLogStreamStderr, 5, "ERR2")

	logs, err := queries.GetRunLogSnapshot(ctx, db.GetRunLogSnapshotParams{
		StdoutLimit: 5,
		StderrLimit: 4,
		OrgID:       ids.ToPG(ids.DefaultOrgID),
		RunID:       run.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout := string(logs.Stdout); stdout != "efghi" {
		t.Fatalf("stdout = %q", stdout)
	}
	if stderr := string(logs.Stderr); stderr != "ERR2" {
		t.Fatalf("stderr = %q", stderr)
	}
	if !logs.Truncated.Bool || logs.StdoutCursor != 9 || logs.StderrCursor != 8 {
		t.Fatalf("snapshot metadata = %+v", logs)
	}
}

func TestRestoreClaimAbandonRevertsCheckpointToReady(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run, waitpointID, checkpointID := seedResolvedCheckpoint(t, ctx, queries)
	_ = waitpointID
	restored, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-restore",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.AbandonClaimedRunExecution(ctx, db.AbandonClaimedRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	var checkpointStatus db.CheckpointStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM checkpoints WHERE id = $1`, checkpointID).Scan(&checkpointStatus); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusReady {
		t.Fatalf("checkpoint status = %s", checkpointStatus)
	}
	requeued, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != db.RunStatusQueued || requeued.CurrentExecutionID.Valid {
		t.Fatalf("run = %+v", requeued)
	}
}

func TestExpiredRestoreClaimRevertsCheckpointToReady(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run, _, checkpointID := seedResolvedCheckpoint(t, ctx, queries)
	restored, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-restore",
		LeaseExpiresAt: pgTime(time.Now().Add(-time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.RequeueExpiredClaimedRunExecutions(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	requeued, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if requeued.Status != db.RunStatusQueued || requeued.CurrentExecutionID.Valid {
		t.Fatalf("run = %+v", requeued)
	}
	var executionStatus db.RunExecutionStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM run_executions WHERE id = $1`, restored.ExecutionID).Scan(&executionStatus); err != nil {
		t.Fatal(err)
	}
	if executionStatus != db.RunExecutionStatusLost {
		t.Fatalf("execution status = %s", executionStatus)
	}
	var checkpointStatus db.CheckpointStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM checkpoints WHERE id = $1`, checkpointID).Scan(&checkpointStatus); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusReady {
		t.Fatalf("checkpoint status = %s", checkpointStatus)
	}
}

func TestRestoreReleaseFailureInvalidatesCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run, _, checkpointID := seedResolvedCheckpoint(t, ctx, queries)
	restored, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-restore",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(ids.DefaultOrgID),
		RunID:                run.ID,
		ExecutionID:          restored.ExecutionID,
		WorkerPoolID:         restored.ExecutionWorkerPoolID,
		WorkerID:             restored.ExecutionWorkerID,
		Status:               db.RunStatusFailed,
		ErrorMessage:         pgtype.Text{String: "restore guest runtime: bad snapshot", Valid: true},
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"worker_failed","detail":{"message":"restore guest runtime: bad snapshot"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	var checkpointStatus db.CheckpointStatus
	var checkpointError pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT status, error_message FROM checkpoints WHERE id = $1`, checkpointID).Scan(&checkpointStatus, &checkpointError); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusInvalid || !checkpointError.Valid || checkpointError.String != "restore guest runtime: bad snapshot" {
		t.Fatalf("checkpoint status=%s error=%+v", checkpointStatus, checkpointError)
	}
}

func TestExpiredRestoreExecutionInvalidatesCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run, _, checkpointID := seedResolvedCheckpoint(t, ctx, queries)
	restored, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-restore",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE run_executions SET lease_expires_at = now() - interval '1 minute' WHERE id = $1`, restored.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := queries.FailExpiredRunningRunExecutions(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	var checkpointStatus db.CheckpointStatus
	var checkpointError pgtype.Text
	if err := pool.QueryRow(ctx, `SELECT status, error_message FROM checkpoints WHERE id = $1`, checkpointID).Scan(&checkpointStatus, &checkpointError); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusInvalid || !checkpointError.Valid || checkpointError.String != "worker lease expired" {
		t.Fatalf("checkpoint status=%s error=%+v", checkpointStatus, checkpointError)
	}
}

func TestRestoreTaskExitKeepsCheckpointReady(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run, _, checkpointID := seedResolvedCheckpoint(t, ctx, queries)
	restored, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-restore",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  restored.ExecutionID,
		WorkerPoolID: restored.ExecutionWorkerPoolID,
		WorkerID:     restored.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(ids.DefaultOrgID),
		RunID:                run.ID,
		ExecutionID:          restored.ExecutionID,
		WorkerPoolID:         restored.ExecutionWorkerPoolID,
		WorkerID:             restored.ExecutionWorkerID,
		Status:               db.RunStatusFailed,
		ExitCode:             pgtype.Int4{Int32: 1, Valid: true},
		TerminalEventKind:    "run.failed",
		TerminalEventPayload: []byte(`{"failure_kind":"task_failed","detail":{"exit_code":1}}`),
	}); err != nil {
		t.Fatal(err)
	}
	var checkpointStatus db.CheckpointStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM checkpoints WHERE id = $1`, checkpointID).Scan(&checkpointStatus); err != nil {
		t.Fatal(err)
	}
	if checkpointStatus != db.CheckpointStatusReady {
		t.Fatalf("checkpoint status = %s", checkpointStatus)
	}
}

func TestExpirePendingWaitpointRequeuesDetachedRun(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	executionID := ids.ToPG(ids.New())
	claim, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    executionID,
		WorkerID:       "worker-1",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     checkpointID,
		CheckpointReason: "wait_message",
		ID:               waitpointID,
		CorrelationID:    "1",
		Kind:             db.WaitpointKindMessage,
		Request:          []byte(`{"prompt":"next"}`),
		DisplayText:      "next",
		TimeoutSeconds:   pgtype.Int4{Int32: 1, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	markCheckpointReady(t, ctx, queries, run.ID, claim.ExecutionID, claim.ExecutionWorkerID, waitpointID, checkpointID)
	if _, err := pool.Exec(ctx, `UPDATE waitpoints SET requested_at = now() - interval '2 seconds' WHERE id = $1`, waitpointID); err != nil {
		t.Fatal(err)
	}
	expired, err := queries.ExpirePendingWaitpoint(ctx, db.ExpirePendingWaitpointParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ID:           waitpointID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if expired.Status != db.WaitpointStatusResolved || !expired.ResolutionKind.Valid || expired.ResolutionKind.String != "timed_out" {
		t.Fatalf("expired waitpoint = %+v", expired)
	}
	queued, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != db.RunStatusQueued || queued.CurrentExecutionID.Valid {
		t.Fatalf("queued run = %+v", queued)
	}
}

func TestWaitpointCreateRetryKeepsOriginalCheckpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)

	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	first, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     checkpointID,
		CheckpointReason: "wait_approval",
		ID:               waitpointID,
		CorrelationID:    "retryable",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"first"}`),
		DisplayText:      "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	retried, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     ids.ToPG(ids.New()),
		CheckpointReason: "wait_approval",
		ID:               ids.ToPG(ids.New()),
		CorrelationID:    "retryable",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"second"}`),
		DisplayText:      "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != first.ID || retried.CheckpointID != first.CheckpointID {
		t.Fatalf("retried waitpoint swapped identity: first=%+v retried=%+v", first, retried)
	}
	var checkpointCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM checkpoints WHERE run_id = $1`, run.ID).Scan(&checkpointCount); err != nil {
		t.Fatal(err)
	}
	if checkpointCount != 1 {
		t.Fatalf("checkpoint count = %d", checkpointCount)
	}
}

func TestWaitpointCreateAfterCheckpointFailedCanReuseCorrelation(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)

	firstWaitpointID := ids.ToPG(ids.New())
	firstCheckpointID := ids.ToPG(ids.New())
	first, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     firstCheckpointID,
		CheckpointReason: "wait_approval",
		ID:               firstWaitpointID,
		CorrelationID:    "retry-after-failure",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"first"}`),
		DisplayText:      "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.MarkWaitpointCheckpointFailed(ctx, db.MarkWaitpointCheckpointFailedParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        run.ID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
		WaitpointID:  firstWaitpointID,
		CheckpointID: firstCheckpointID,
		ErrorMessage: pgtype.Text{String: "snapshot failed", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	secondCheckpointID := ids.ToPG(ids.New())
	second, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     secondCheckpointID,
		CheckpointReason: "wait_approval",
		ID:               ids.ToPG(ids.New()),
		CorrelationID:    "retry-after-failure",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"second"}`),
		DisplayText:      "second",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.CheckpointID != secondCheckpointID {
		t.Fatalf("retry waitpoint = %+v first=%+v", second, first)
	}
	var creatingCheckpoints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM checkpoints WHERE run_id = $1 AND status = 'creating'`, run.ID).Scan(&creatingCheckpoints); err != nil {
		t.Fatal(err)
	}
	if creatingCheckpoints != 1 {
		t.Fatalf("creating checkpoints = %d", creatingCheckpoints)
	}
	var openWaitpoints int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM waitpoints WHERE run_id = $1 AND status IN ('creating', 'pending')`, run.ID).Scan(&openWaitpoints); err != nil {
		t.Fatal(err)
	}
	if openWaitpoints != 1 {
		t.Fatalf("open waitpoints = %d", openWaitpoints)
	}
}

func TestCheckpointReadyRequiresMatchingCreatingWaitpoint(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)

	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     checkpointID,
		CheckpointReason: "wait_message",
		ID:               waitpointID,
		CorrelationID:    "ready-gate",
		Kind:             db.WaitpointKindMessage,
		Request:          []byte(`{"prompt":"next"}`),
		DisplayText:      "next",
	}); err != nil {
		t.Fatal(err)
	}
	staleDigest := "sha256:" + strings.Repeat("9", 64)
	_, err := queries.MarkWaitpointCheckpointReady(ctx, db.MarkWaitpointCheckpointReadyParams{
		OrgID:             ids.ToPG(ids.DefaultOrgID),
		RunID:             run.ID,
		ExecutionID:       claim.ExecutionID,
		WorkerPoolID:      claim.ExecutionWorkerPoolID,
		WorkerID:          claim.ExecutionWorkerID,
		CasObjects:        []byte(`[{"digest":"` + staleDigest + `","size_bytes":1,"media_type":"application/vnd.helmr.checkpoint.vm-state"}]`),
		Manifest:          []byte(`{"mode":"test"}`),
		RuntimeBackend:    pgtype.Text{String: "test", Valid: true},
		RuntimeArch:       pgtype.Text{String: "amd64", Valid: true},
		RuntimeABI:        pgtype.Text{String: "helmr.test.v0", Valid: true},
		MemoryDigests:     []byte(`[]`),
		CheckpointID:      checkpointID,
		WaitpointID:       ids.ToPG(ids.New()),
		CheckpointPayload: []byte(`{"backend":"test"}`),
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong waitpoint ready err = %v", err)
	}
	var status db.CheckpointStatus
	if err := pool.QueryRow(ctx, `SELECT status FROM checkpoints WHERE id = $1`, checkpointID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != db.CheckpointStatusCreating {
		t.Fatalf("checkpoint status after wrong ready = %s", status)
	}
	var casRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM cas_objects WHERE digest = $1`, staleDigest).Scan(&casRows); err != nil {
		t.Fatal(err)
	}
	if casRows != 0 {
		t.Fatalf("stale checkpoint-ready published %d CAS rows", casRows)
	}
	markCheckpointReady(t, ctx, queries, run.ID, claim.ExecutionID, claim.ExecutionWorkerID, waitpointID, checkpointID)
}

func TestRunExecutionFencingAllowsOnlyOneConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	queries, pool := newPostgresTestDB(t, ctx)
	run := seedQueuedRun(t, ctx, queries)
	const workers = 8

	var successes atomic.Int32
	var currentExecution pgtype.UUID
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claimed, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
				OrgID:          ids.ToPG(ids.DefaultOrgID),
				ExecutionID:    ids.ToPG(ids.New()),
				WorkerID:       fmt.Sprintf("worker-%d", i),
				LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return
			}
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			successes.Add(1)
			mu.Lock()
			currentExecution = claimed.ExecutionID
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful claims = %d", successes.Load())
	}

	var activeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_executions WHERE run_id = $1 AND status IN ('claimed', 'running')`, run.ID).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	claimed, err := queries.GetRun(ctx, db.GetRunParams{OrgID: ids.ToPG(ids.DefaultOrgID), ID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 || claimed.Status != db.RunStatusClaimed || claimed.CurrentExecutionID != currentExecution {
		t.Fatalf("active_count=%d run=%+v current_execution=%+v", activeCount, claimed, currentExecution)
	}
}

func seedResolvedCheckpoint(t *testing.T, ctx context.Context, queries *db.Queries) (db.Run, pgtype.UUID, pgtype.UUID) {
	t.Helper()
	run := seedQueuedRun(t, ctx, queries)
	claim := claimAndStartRun(t, ctx, queries, run.ID)
	waitpointID := ids.ToPG(ids.New())
	checkpointID := ids.ToPG(ids.New())
	if _, err := queries.CreateWaitpointForExecution(ctx, db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(ids.DefaultOrgID),
		RunID:            run.ID,
		ExecutionID:      claim.ExecutionID,
		WorkerPoolID:     claim.ExecutionWorkerPoolID,
		WorkerID:         claim.ExecutionWorkerID,
		CheckpointID:     checkpointID,
		CheckpointReason: "wait_approval",
		ID:               waitpointID,
		CorrelationID:    "restore",
		Kind:             db.WaitpointKindApproval,
		Request:          []byte(`{"message":"ship it"}`),
		DisplayText:      "ship it",
	}); err != nil {
		t.Fatal(err)
	}
	markCheckpointReady(t, ctx, queries, run.ID, claim.ExecutionID, claim.ExecutionWorkerID, waitpointID, checkpointID)
	if _, err := queries.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		RunID:          run.ID,
		ID:             waitpointID,
		Kind:           db.WaitpointKindApproval,
		ResolutionKind: pgtype.Text{String: "approved", Valid: true},
		Resolution:     []byte(`{"approved":true}`),
		Payload:        []byte(`{"resolution_kind":"approved"}`),
	}); err != nil {
		t.Fatal(err)
	}
	return run, waitpointID, checkpointID
}

func claimAndStartRun(t *testing.T, ctx context.Context, queries *db.Queries, runID pgtype.UUID) db.ClaimRunExecutionRow {
	t.Helper()
	claim, err := claimRunExecution(ctx, queries, db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(ids.DefaultOrgID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       "worker-1",
		LeaseExpiresAt: pgTime(time.Now().Add(time.Hour)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.StartRunExecution(ctx, db.StartRunExecutionParams{
		OrgID:        ids.ToPG(ids.DefaultOrgID),
		RunID:        runID,
		ExecutionID:  claim.ExecutionID,
		WorkerPoolID: claim.ExecutionWorkerPoolID,
		WorkerID:     claim.ExecutionWorkerID,
	}); err != nil {
		t.Fatal(err)
	}
	return claim
}

func claimRunExecution(ctx context.Context, queries *db.Queries, arg db.ClaimRunExecutionParams) (db.ClaimRunExecutionRow, error) {
	if !arg.WorkerPoolID.Valid {
		pool, err := queries.GetDefaultWorkerPool(ctx, arg.OrgID)
		if err != nil {
			return db.ClaimRunExecutionRow{}, err
		}
		arg.WorkerPoolID = pool.ID
	}
	if err := upsertTestWorker(ctx, queries, arg.OrgID, arg.WorkerPoolID, arg.WorkerID); err != nil {
		return db.ClaimRunExecutionRow{}, err
	}
	return queries.ClaimRunExecution(ctx, arg)
}

func defaultWorkerPoolID(t *testing.T, ctx context.Context, queries *db.Queries) pgtype.UUID {
	t.Helper()
	pool, err := queries.GetDefaultWorkerPool(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		t.Fatal(err)
	}
	return pool.ID
}

func createTestWorkerPool(t *testing.T, ctx context.Context, queries *db.Queries, slug string) db.WorkerPool {
	t.Helper()
	scope, err := queries.GetDefaultProjectEnvironment(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		t.Fatal(err)
	}
	pool, err := queries.CreateWorkerPool(ctx, db.CreateWorkerPoolParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		Slug:          slug,
		Name:          slug,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func upsertTestWorker(ctx context.Context, queries *db.Queries, orgID pgtype.UUID, workerPoolID pgtype.UUID, workerID string) error {
	_, err := queries.UpsertScopedWorkerHeartbeat(ctx, db.UpsertScopedWorkerHeartbeatParams{
		OrgID:          orgID,
		WorkerPoolID:   workerPoolID,
		ID:             workerID,
		RuntimeArch:    "amd64",
		RuntimeABI:     "helmr.firecracker.snapshot.v0",
		KernelDigest:   "sha256:" + strings.Repeat("3", 64),
		RootfsDigest:   "sha256:" + strings.Repeat("4", 64),
		CniProfile:     "helmr/v1",
		MaxVcpus:       2,
		MaxMemoryMib:   2048,
		SlotsAvailable: 1,
	})
	return err
}

func markCheckpointReady(t *testing.T, ctx context.Context, queries *db.Queries, runID pgtype.UUID, executionID pgtype.UUID, workerID string, waitpointID pgtype.UUID, checkpointID pgtype.UUID) {
	t.Helper()
	markCheckpointReadyWithActive(t, ctx, queries, runID, executionID, workerID, waitpointID, checkpointID, 0)
}

func markCheckpointReadyWithActive(t *testing.T, ctx context.Context, queries *db.Queries, runID pgtype.UUID, executionID pgtype.UUID, workerID string, waitpointID pgtype.UUID, checkpointID pgtype.UUID, activeDurationMs int64) {
	t.Helper()
	pool, err := queries.GetDefaultWorkerPool(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		t.Fatal(err)
	}
	vmStateDigest := "sha256:" + strings.Repeat("1", 64)
	memoryDigest := "sha256:" + strings.Repeat("2", 64)
	_, err = queries.MarkWaitpointCheckpointReady(ctx, db.MarkWaitpointCheckpointReadyParams{
		OrgID:               ids.ToPG(ids.DefaultOrgID),
		RunID:               runID,
		ExecutionID:         executionID,
		WorkerPoolID:        pool.ID,
		WorkerID:            workerID,
		CasObjects:          []byte(`[{"digest":"` + vmStateDigest + `","size_bytes":128,"media_type":"application/vnd.helmr.checkpoint.vm-state"},{"digest":"` + memoryDigest + `","size_bytes":256,"media_type":"application/vnd.helmr.checkpoint.memory"}]`),
		Manifest:            []byte(`{"runtime":{"vcpu_count":2,"memory_mib":2048,"network":{"profile":"helmr/v1"}}}`),
		RuntimeBackend:      pgtype.Text{String: "firecracker", Valid: true},
		RuntimeArch:         pgtype.Text{String: "amd64", Valid: true},
		RuntimeABI:          pgtype.Text{String: "helmr.firecracker.snapshot.v0", Valid: true},
		KernelDigest:        pgtype.Text{String: "sha256:" + strings.Repeat("3", 64), Valid: true},
		RootfsDigest:        pgtype.Text{String: "sha256:" + strings.Repeat("4", 64), Valid: true},
		RuntimeVcpus:        pgtype.Int4{Int32: 2, Valid: true},
		RuntimeMemoryMib:    pgtype.Int4{Int32: 2048, Valid: true},
		RuntimeConfigDigest: pgtype.Text{String: "sha256:" + strings.Repeat("5", 64), Valid: true},
		VMStateDigest:       pgtype.Text{String: vmStateDigest, Valid: true},
		MemoryDigests:       []byte(`["` + memoryDigest + `"]`),
		ActiveDurationMs:    activeDurationMs,
		CheckpointID:        checkpointID,
		WaitpointID:         waitpointID,
		CheckpointPayload:   []byte(`{"backend":"firecracker"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
}

func seedQueuedRun(t *testing.T, ctx context.Context, queries *db.Queries) db.Run {
	t.Helper()
	seedRunDependencies(t, ctx, queries)
	deployedTask := getActiveTestDeployedTask(t, ctx, queries)
	created, err := queries.CreateRun(ctx, db.CreateRunParams{
		ID:                          ids.ToPG(ids.New()),
		OrgID:                       ids.ToPG(ids.DefaultOrgID),
		TaskDeploymentID:            deployedTask.DeploymentID,
		DeployedTaskID:              deployedTask.ID,
		TaskID:                      "deploy",
		Payload:                     []byte(`{}`),
		SecretBindings:              []byte(`{}`),
		WorkspaceRepository:         "helmrdotdev/helmr",
		WorkspaceInstallationID:     123,
		WorkspaceGithubRepositoryID: 456,
		WorkspaceRef:                "main",
		WorkspaceSha:                "0123456789abcdef0123456789abcdef01234567",
		WorkspaceSubpath:            "",
		MaxDurationSeconds:          3600,
		EventPayload:                []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := queries.GetRun(ctx, db.GetRunParams{OrgID: created.OrgID, ID: created.ID})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func seedRunDependencies(t *testing.T, ctx context.Context, queries *db.Queries) {
	t.Helper()
	if err := queries.EnsureDefaultOrganization(ctx, ids.ToPG(ids.DefaultOrgID)); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertGitHubInstallation(ctx, db.UpsertGitHubInstallationParams{
		ID:                  ids.ToPG(ids.New()),
		OrgID:               ids.ToPG(ids.DefaultOrgID),
		InstallationID:      123,
		AccountLogin:        "helmrdotdev",
		AccountType:         "Organization",
		RepositorySelection: pgtype.Text{String: "selected", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.UpsertGitHubRepository(ctx, db.UpsertGitHubRepositoryParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		InstallationID:     123,
		GithubRepositoryID: 456,
		OwnerLogin:         "helmrdotdev",
		Name:               "helmr",
		FullName:           "helmrdotdev/helmr",
		DefaultBranch:      pgtype.Text{String: "main", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	scope, err := queries.GetDefaultProjectEnvironment(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnableGitHubRepositoryConnection(ctx, db.EnableGitHubRepositoryConnectionParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		GithubRepositoryID: 456,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.EnableProjectWorkspaceRepositoryAccess(ctx, db.EnableProjectWorkspaceRepositoryAccessParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(ids.DefaultOrgID),
		ProjectID:          scope.ProjectID,
		GithubRepositoryID: 456,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.GetActiveDeployedTask(ctx, db.GetActiveDeployedTaskParams{
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		TaskID:        "deploy",
	}); err == nil {
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	taskSourceDigest := "sha256:" + strings.Repeat("a", 64)
	if _, err := queries.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		Digest:    taskSourceDigest,
		SizeBytes: 1,
		MediaType: "application/vnd.helmr.task-source.v1.tar",
	}); err != nil {
		t.Fatal(err)
	}
	deploymentID := ids.ToPG(ids.New())
	if _, err := queries.CreateTaskDeployment(ctx, db.CreateTaskDeploymentParams{
		ID:            deploymentID,
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		SourceDigest:  taskSourceDigest,
		Status:        db.TaskDeploymentStatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateDeployedTask(ctx, db.CreateDeployedTaskParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		DeploymentID:  deploymentID,
		TaskID:        "deploy",
		ModulePath:    "tasks/deploy.ts",
		ExportName:    "deploy",
	}); err != nil {
		t.Fatal(err)
	}
}

func getActiveTestDeployedTask(t *testing.T, ctx context.Context, queries *db.Queries) db.GetActiveDeployedTaskRow {
	t.Helper()
	scope, err := queries.GetDefaultProjectEnvironment(ctx, ids.ToPG(ids.DefaultOrgID))
	if err != nil {
		t.Fatal(err)
	}
	deployedTask, err := queries.GetActiveDeployedTask(ctx, db.GetActiveDeployedTaskParams{
		OrgID:         ids.ToPG(ids.DefaultOrgID),
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		TaskID:        "deploy",
	})
	if err != nil {
		t.Fatal(err)
	}
	return deployedTask
}

func assertNoRowsForRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID pgtype.UUID) {
	t.Helper()
	var runCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE id = $1`, runID).Scan(&runCount); err != nil {
		t.Fatal(err)
	}
	if runCount != 0 {
		t.Fatalf("run rows = %d", runCount)
	}
	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id = $1`, runID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 0 {
		t.Fatalf("event rows = %d", eventCount)
	}
}

func newPostgresTestDB(t *testing.T, ctx context.Context) (*db.Queries, *pgxpool.Pool) {
	t.Helper()
	if dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL")); dsn != "" {
		return newExternalPostgresTestDB(t, ctx, dsn, "schema/migrations/*.up.sql")
	}
	for _, name := range []string{"initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s not found; skipping Postgres integration test", name)
		}
	}
	tmp, err := os.MkdirTemp("", "helmr-pg-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(tmp)
	})
	dataDir := filepath.Join(tmp, "data")
	if output, err := exec.Command("initdb", "-D", dataDir, "-A", "trust").CombinedOutput(); err != nil {
		t.Fatalf("initdb: %v\n%s", err, output)
	}
	port := freePort(t)
	logPath := filepath.Join(tmp, "postgres.log")
	start := exec.Command("pg_ctl", "-D", dataDir, "-l", logPath, "-o", fmt.Sprintf("-p %d -c listen_addresses=127.0.0.1", port), "-w", "start")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("pg_ctl start: %v\n%s", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command("pg_ctl", "-D", dataDir, "-m", "fast", "-w", "stop").Run()
	})

	dsn := fmt.Sprintf("postgres://%s@127.0.0.1:%d/postgres?sslmode=disable", os.Getenv("USER"), port)
	dbctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(dbctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var serverVersion int
	if err := pool.QueryRow(dbctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d does not provide uuidv7(); skipping Postgres integration test", serverVersion)
	}
	applyPostgresTestMigrations(t, dbctx, pool, "schema/migrations/*.up.sql")
	pool.Close()
	registeredPool, err := pgxpool.New(dbctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registeredPool.Close)
	return db.New(registeredPool), registeredPool
}

func newExternalPostgresTestDB(t *testing.T, ctx context.Context, dsn string, migrationsGlob string) (*db.Queries, *pgxpool.Pool) {
	t.Helper()
	adminCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(adminCtx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "helmr_test_" + strings.ReplaceAll(ids.New().String(), "-", "")
	dbIdentifier := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(adminCtx, "CREATE DATABASE "+dbIdentifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+dbIdentifier)
		admin.Close()
	})

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = dbName
	dbctx, dbcancel := context.WithTimeout(ctx, 10*time.Second)
	defer dbcancel()
	pool, err := pgxpool.NewWithConfig(dbctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	applyPostgresTestMigrations(t, dbctx, pool, migrationsGlob)
	pool.Close()
	registeredPool, err := pgxpool.NewWithConfig(dbctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registeredPool.Close)
	return db.New(registeredPool), registeredPool
}

func applyPostgresTestMigrations(t *testing.T, ctx context.Context, pool *pgxpool.Pool, migrationsGlob string) {
	t.Helper()
	var serverVersion int
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion < 180000 {
		t.Skipf("Postgres %d does not provide uuidv7(); skipping Postgres integration test", serverVersion)
	}
	migrations, err := filepath.Glob(migrationsGlob)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(migrations)
	for _, path := range migrations {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func pgTime(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: value, Valid: true}
}
