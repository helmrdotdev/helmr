package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestWorkerClaimWorkspaceExecPostgresRequiresCurrentFrontier(t *testing.T) {
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var workspaceID, baseWorkspaceVersionID, runtimeID, mountID, workspaceLeaseID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT runs.workspace_id, runs.base_workspace_version_id,
       run_leases.runtime_instance_id, workspace_leases.workspace_mount_id,
       workspace_leases.id
  FROM runs
  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`, work.RunID).Scan(
		&workspaceID, &baseWorkspaceVersionID, &runtimeID, &mountID, &workspaceLeaseID,
	); err != nil {
		t.Fatal(err)
	}
	claimID := uuid.NewV7()
	processID := uuid.NewV7()
	creatorID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint,
    accepted_at, expires_at
) VALUES ($1, $2, 'workspace.exec', decode(repeat('11', 32), 'hex'),
          decode(repeat('22', 32), 'hex'), now(), now() + interval '30 days')`,
		claimID, fixture.EnvironmentID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO workspace_processes (
    id, org_id, project_id, environment_id, workspace_id, base_workspace_version_id,
    restore_desired_state, region_id, worker_group_id, worker_instance_id,
    worker_epoch, runtime_instance_id, workspace_mount_id, status, request,
    stdin, stdout, stderr, claim_id, created_by_subject_type,
    created_by_subject_id
) VALUES (
    $1, $2, $3, $4, $5, $6, 'active', $7, $8, $9, 1, $10, $11,
    'running', '{"command":["true"]}'::jsonb, ''::bytea, ''::bytea, ''::bytea, $12,
    'api_key', $13
)`, processID, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID,
		workspaceID, baseWorkspaceVersionID, runtest.Region, runtest.WorkerGroup,
		fixture.WorkerID, runtimeID, mountID, claimID, creatorID.String())
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE workspace_leases
   SET owner_run_lease_id = NULL, owner_process_id = $1
 WHERE id = $2`, processID, workspaceLeaseID)

	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_mounts SET status='mounted',materialized_version_id=$2 WHERE id=$1`, mountID, baseWorkspaceVersionID)
	key, err := workspace.NewFencingKey(bytes.Repeat([]byte{42}, workspace.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	var owner, writer, mount int64
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT ownership_generation,writer_generation,mount_fencing_generation FROM workspace_leases WHERE id=$1`, workspaceLeaseID).Scan(&owner, &writer, &mount); err != nil {
		t.Fatal(err)
	}
	capability, err := key.Derive(workspace.FenceInput{LeaseID: workspaceLeaseID, WorkspaceID: workspaceID, OwnershipGeneration: owner, WriterGeneration: writer, MountFencingGeneration: mount})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_leases SET fencing_token_hash=$2,expires_at=now()+interval '30 minutes' WHERE id=$1`, workspaceLeaseID, capability.Hash)
	secretStore, err := secret.New(db.New(fixture.Pool), fixture.Pool, bytes.Repeat([]byte{17}, 32))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{secretDelivery: secretStore, db: db.New(fixture.Pool), tx: fixture.Pool, workspaceFencingKey: key, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	claim := func() *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(workerapi.WorkspaceExecClaimRequest{OrgID: fixture.OrgID.String(), WorkspaceMountID: mountID.String()})
		req := httptest.NewRequest(http.MethodPost, "/worker/v1/run/workspace-execs/claim", bytes.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, workerActor{WorkerInstanceID: fixture.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1}))
		response := httptest.NewRecorder()
		server.workerClaimWorkspaceExec(response, req)
		return response
	}
	assertClaim := func(want uuid.UUID) {
		t.Helper()
		response := claim()
		if response.Code != http.StatusOK {
			t.Fatalf("claim %d %s", response.Code, response.Body.String())
		}
		var body workerapi.WorkspaceExecClaimResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Exec == nil || body.Exec.BaseWorkspaceVersionID != want.String() || body.Exec.OwnershipGeneration != owner || body.Exec.WriterGeneration != writer || body.Exec.FencingGeneration != mount || body.Exec.WriteCapability != capability.Token {
			t.Fatalf("claim = %+v", body.Exec)
		}
	}
	assertClaim(baseWorkspaceVersionID)
	// A promoted frontier differs from the original Run/mount target. All three
	// locked owner rows must agree before CP projects it to the trusted Worker.
	promoted := uuid.NewV7()
	artifactID := uuid.NewV7()
	digest := dbtest.Digest("exec-frontier-capture")
	dbtest.MustExec(t, t.Context(), fixture.Pool, `WITH lifetime AS (INSERT INTO cas_blobs (digest, size_bytes) VALUES ($2, 1) ON CONFLICT DO NOTHING) INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,1,$3)`, fixture.OrgID, digest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `INSERT INTO artifacts(id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type) VALUES($1,$2,$3,$4,$5,'workspace_version',1,$6)`, artifactID, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID, digest, workspace.ArtifactMediaType)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
 INSERT INTO computer_versions(id,environment_id,computer_id,parent_version_id,status,root_pack_digest,logical_bytes,source_workspace_lease_id,ownership_generation,writer_generation,published_at)
 VALUES($1,$2,$3,$4,'committed',$5,1,$6,$7,$8,now())`, promoted, fixture.EnvironmentID, workspaceID, baseWorkspaceVersionID, digest, workspaceLeaseID, owner, writer)

	for _, test := range []struct {
		name, sql string
		id        uuid.UUID
	}{
		{"lease", `UPDATE workspace_leases SET base_workspace_version_id=$2 WHERE id=$1`, workspaceLeaseID},
		{"process", `UPDATE workspace_processes SET base_workspace_version_id=$2 WHERE id=$1`, processID},
		{"mount", `UPDATE workspace_mounts SET materialized_version_id=$2 WHERE id=$1`, mountID},
	} {
		t.Run(test.name, func(t *testing.T) {
			dbtest.MustExec(t, t.Context(), fixture.Pool, test.sql, test.id, promoted)
			response := claim()
			if response.Code != http.StatusConflict {
				t.Fatalf("mismatched frontier admitted: %d %s", response.Code, response.Body.String())
			}
			dbtest.MustExec(t, t.Context(), fixture.Pool, test.sql, test.id, baseWorkspaceVersionID)
		})
	}
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_leases SET base_workspace_version_id=$2 WHERE id=$1`, workspaceLeaseID, promoted)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_processes SET base_workspace_version_id=$2 WHERE id=$1`, processID, promoted)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_mounts SET materialized_version_id=$2 WHERE id=$1`, mountID, promoted)
	assertClaim(promoted)
	// Completed work is not a new grant. Preserve the empty claim while capture
	// promotion and stop are finishing, even once the mount frontier has advanced.
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_processes SET status='exit_requested' WHERE id=$1`, processID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `UPDATE workspace_mounts SET materialized_version_id=$2 WHERE id=$1`, mountID, baseWorkspaceVersionID)
	response := claim()
	if response.Code != http.StatusOK {
		t.Fatalf("finalizing claim: %d %s", response.Code, response.Body.String())
	}
	var empty workerapi.WorkspaceExecClaimResponse
	if err := json.Unmarshal(response.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.Exec != nil {
		t.Fatal("finalizing process received another grant")
	}
}
