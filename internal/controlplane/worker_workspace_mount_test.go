package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestWorkerCaptureWorkspaceMountRejectsLegacyAndInvalidIdentityShapes(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "legacy flat artifact",
			body: `{"org_id":"org-1","workspace_mount_id":"mount-1","artifact_digest":"` + digest + `","artifact_size_bytes":1024,"artifact_media_type":"application/vnd.helmr.workspace.v0.tar","artifact_encoding":"tar","artifact_entry_count":1}`,
			want: "unknown field",
		},
		{
			name: "missing tree",
			body: `{"org_id":"org-1","workspace_mount_id":"mount-1","artifact":{"digest":"` + digest + `","media_type":"application/vnd.helmr.workspace.v0.tar","encoding":"tar","size_bytes":1024,"entry_count":1}}`,
			want: "tree is invalid",
		},
		{
			name: "tree artifact entry count mismatch",
			body: `{"org_id":"org-1","workspace_mount_id":"mount-1","tree":{"digest":"` + digest + `","size_bytes":4,"entry_count":2},"artifact":{"digest":"` + digest + `","media_type":"application/vnd.helmr.workspace.v0.tar","encoding":"tar","size_bytes":1024,"entry_count":1}}`,
			want: "tree and artifact entry counts differ",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/workspace-mounts/capture", strings.NewReader(test.body))
			response := httptest.NewRecorder()
			(&Server{}).workerCaptureWorkspaceMount(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("response = %d %s, want 400 containing %q", response.Code, response.Body.String(), test.want)
			}
		})
	}
}

func TestWorkerCaptureWorkspaceMountPersistsTreeAndArtifactIdentities(t *testing.T) {
	fixture := runtest.New(t)
	work := fixture.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	var workspaceID, baseVersionID, runtimeID, mountID, workspaceLeaseID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT runs.workspace_id, runs.base_workspace_version_id,
       run_leases.runtime_instance_id, workspace_leases.workspace_mount_id,
       workspace_leases.id
  FROM runs
  JOIN run_leases ON run_leases.id = runs.current_run_lease_id
  JOIN workspace_leases ON workspace_leases.owner_run_lease_id = run_leases.id
 WHERE runs.id = $1`, work.RunID).Scan(
		&workspaceID, &baseVersionID, &runtimeID, &mountID, &workspaceLeaseID,
	); err != nil {
		t.Fatal(err)
	}
	claimID := uuid.Must(uuid.NewV7())
	processID := uuid.Must(uuid.NewV7())
	creatorID := uuid.Must(uuid.NewV7())
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO idempotency_claims (
    id, environment_id, operation, slot_hash, request_fingerprint,
    accepted_at, expires_at
) VALUES ($1, $2, 'workspace.exec', decode(repeat('11', 32), 'hex'),
          decode(repeat('22', 32), 'hex'), now(), now() + interval '30 days')`,
		claimID, fixture.EnvironmentID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
INSERT INTO workspace_processes (
    id, org_id, project_id, environment_id, workspace_id, base_version_id,
    restore_desired_state, region_id, worker_group_id, worker_instance_id,
    worker_epoch, runtime_instance_id, workspace_mount_id, state, request,
    stdin, stdout, stderr, claim_id, created_by_subject_type,
    created_by_subject_id
) VALUES (
    $1, $2, $3, $4, $5, $6, 'active', $7, $8, $9, 1, $10, $11,
    'exit_requested', '{}'::jsonb, ''::bytea, ''::bytea, ''::bytea, $12,
    'api_key', $13
)`, processID, fixture.OrgID, fixture.ProjectID, fixture.EnvironmentID,
		workspaceID, baseVersionID, runtest.Region, runtest.WorkerGroup,
		fixture.WorkerID, runtimeID, mountID, claimID, creatorID.String())
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE workspace_leases
   SET owner_run_lease_id = NULL, owner_process_id = $1
 WHERE id = $2`, processID, workspaceLeaseID)
	dbtest.MustExec(t, t.Context(), fixture.Pool, `
UPDATE workspace_mounts
   SET state = 'unmounting', finalization_kind = 'capture',
       finalization_reason_code = 'workspace_exec_completed', stopped_at = now()
 WHERE id = $1`, mountID)

	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	makeCapture := func(name, content string) workerapi.WorkspaceMountCaptureRequest {
		t.Helper()
		trustedRoot := t.TempDir()
		root := filepath.Join(trustedRoot, "root")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(root, trustedRoot, trustedRoot)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		tree, err := workspace.InspectArtifactTreeContext(t.Context(), artifact.Path, artifact.SizeBytes)
		if err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(artifact.Path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		object, err := store.Put(t.Context(), workspace.ArtifactMediaType, file)
		if err != nil {
			t.Fatal(err)
		}
		if object.Digest != artifact.Digest || object.SizeBytes != artifact.SizeBytes {
			t.Fatalf("stored object = %+v, artifact = %+v", object, artifact)
		}
		return workerapi.WorkspaceMountCaptureRequest{
			OrgID: fixture.OrgID.String(), WorkspaceMountID: mountID.String(),
			Tree: workerapi.WorkspaceTreeIdentity{
				Digest: tree.Digest, SizeBytes: tree.SizeBytes, EntryCount: int32(tree.EntryCount),
			},
			Artifact: workerapi.WorkspaceArtifact{
				Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
				SizeBytes: artifact.SizeBytes, EntryCount: int32(artifact.EntryCount),
			},
		}
	}
	first := makeCapture("marker.txt", "first logical workspace")
	if first.Tree.Digest == first.Artifact.Digest || first.Tree.SizeBytes == first.Artifact.SizeBytes {
		t.Fatalf("fixture conflates tree and artifact: %+v", first)
	}
	server := &Server{
		db: db.New(fixture.Pool), tx: fixture.Pool, cas: store,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	capture := func(request workerapi.WorkspaceMountCaptureRequest) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		httpRequest := httptest.NewRequest(http.MethodPost, "/worker/v1/run/workspace-mounts/capture", strings.NewReader(string(body)))
		httpRequest = httpRequest.WithContext(context.WithValue(httpRequest.Context(), workerContextKey{}, workerActor{
			WorkerInstanceID: fixture.WorkerID, WorkerGroupID: runtest.WorkerGroup, WorkerEpoch: 1,
		}))
		response := httptest.NewRecorder()
		server.workerCaptureWorkspaceMount(response, httpRequest)
		return response
	}

	response := capture(first)
	if response.Code != http.StatusOK {
		t.Fatalf("first capture = %d %s", response.Code, response.Body.String())
	}
	var receipt workerapi.WorkspaceMountCaptureResponse
	if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	versionID := uuid.MustParse(receipt.VersionID)
	var versionKind db.WorkspaceVersionKind
	var versionDigest, artifactDigest string
	var versionSize, artifactSize int64
	var versionEntries int32
	if err := fixture.Pool.QueryRow(t.Context(), `
SELECT workspace_versions.kind, workspace_versions.content_digest, workspace_versions.size_bytes,
       workspace_versions.entry_count, artifacts.digest, artifacts.size_bytes
  FROM workspace_versions
  JOIN artifacts ON artifacts.id = workspace_versions.artifact_id
 WHERE workspace_versions.id = $1`, versionID).Scan(
		&versionKind, &versionDigest, &versionSize, &versionEntries, &artifactDigest, &artifactSize,
	); err != nil {
		t.Fatal(err)
	}
	if versionKind != db.WorkspaceVersionKindUser ||
		versionDigest != first.Tree.Digest || versionSize != first.Tree.SizeBytes || versionEntries != first.Tree.EntryCount ||
		artifactDigest != first.Artifact.Digest || artifactSize != first.Artifact.SizeBytes {
		t.Fatalf("version=%s/%s/%d/%d artifact=%s/%d", versionKind, versionDigest, versionSize, versionEntries, artifactDigest, artifactSize)
	}
	authority, err := server.db.GetWorkspaceResetTargetAuthority(t.Context(), db.GetWorkspaceResetTargetAuthorityParams{
		OrgID: pgvalue.UUID(fixture.OrgID), ProjectID: pgvalue.UUID(fixture.ProjectID),
		EnvironmentID: pgvalue.UUID(fixture.EnvironmentID), WorkspaceID: pgvalue.UUID(workspaceID),
		VersionID: pgvalue.UUID(versionID),
	})
	if err != nil {
		t.Fatal(err)
	}
	resetTarget, err := projectWorkspaceResetTarget(db.WorkspaceLease{BaseVersionID: pgvalue.UUID(versionID)}, authority)
	if err != nil {
		t.Fatalf("captured version is not a valid reset target: %v", err)
	}
	if resetTarget.BaseWorkspaceVersionID != versionID.String() || resetTarget.Artifact == nil ||
		resetTarget.Artifact.Digest != first.Artifact.Digest || resetTarget.Tree.Digest != first.Tree.Digest {
		t.Fatalf("captured reset target = %+v", resetTarget)
	}

	replayed := capture(first)
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), versionID.String()) {
		t.Fatalf("same-tree replay = %d %s", replayed.Code, replayed.Body.String())
	}
	second := makeCapture("marker.txt", "different logical workspace")
	conflicted := capture(second)
	if conflicted.Code != http.StatusConflict || !strings.Contains(conflicted.Body.String(), "workspace capture replay differs") {
		t.Fatalf("different-tree replay = %d %s", conflicted.Code, conflicted.Body.String())
	}

	var stagedID uuid.UUID
	if err := fixture.Pool.QueryRow(t.Context(), `SELECT staged_version_id FROM workspace_mounts WHERE id = $1`, mountID).Scan(&stagedID); err != nil {
		t.Fatal(err)
	}
	if stagedID != versionID {
		t.Fatalf("staged version = %s, want %s", stagedID, versionID)
	}
}
