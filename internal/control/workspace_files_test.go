package control

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkspaceFilesReadListStatAndVersionsFromReadyArtifacts(t *testing.T) {
	readyVersionID := pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000401"))
	explicitVersionID := pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000402"))
	currentArtifactID := testArtifactID()
	explicitArtifactID := pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000404"))
	currentArtifactDigest := "sha256:" + strings.Repeat("b", 64)
	explicitArtifactDigest := "sha256:" + strings.Repeat("d", 64)
	store := &fakeStore{
		workspace: db.Workspace{
			ID:                  testWorkspaceID(),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			DeploymentSandboxID: testDeploymentSandboxID(),
			SandboxID:           "default",
			SandboxFingerprint:  testSandboxFingerprint(),
			CurrentVersionID:    readyVersionID,
			State:               db.WorkspaceStateActive,
			DesiredState:        db.WorkspaceDesiredStateActive,
			DirtyState:          db.WorkspaceDirtyStateClean,
			Metadata:            []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		workspaceVersions: []db.WorkspaceVersion{
			workspaceFileTestVersion(readyVersionID, testWorkspaceID(), currentArtifactID),
			workspaceFileTestVersion(explicitVersionID, testWorkspaceID(), explicitArtifactID),
			workspaceFileTestVersion(pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000403")), pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000999")), currentArtifactID),
		},
		artifacts: []db.Artifact{
			workspaceFileTestArtifact(currentArtifactID, currentArtifactDigest),
			workspaceFileTestArtifact(explicitArtifactID, explicitArtifactDigest),
		},
	}
	server := newWorkspaceFilesTestServerWithCAS(store, &fakeCAS{bodies: map[string][]byte{
		currentArtifactDigest:  validDeploymentSourceTar(t),
		explicitArtifactDigest: workspaceFileSourceTar(t, []workspaceFileTarFile{{path: "tasks/version-two.ts", contents: "export const version = 2\n"}}),
	}})

	requests := []struct {
		path      string
		versionID string
		handler   func(http.ResponseWriter, *http.Request)
		check     func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			path:    "/files/content?path=helmr.config.ts",
			handler: server.readWorkspaceFile,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
				}
				if got := rec.Body.String(); got != "export default {}\n" {
					t.Fatalf("body = %q", got)
				}
				if got := rec.Header().Get("x-helmr-workspace-version-id"); got != "00000000-0000-0000-0000-000000000401" {
					t.Fatalf("version header = %q", got)
				}
			},
		},
		{
			path:    "/files?path=tasks&source=version&version_id=00000000-0000-0000-0000-000000000402",
			handler: server.listWorkspaceFiles,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
				}
				var response api.ListWorkspaceFilesResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Entries) != 1 || response.Entries[0].Path != "tasks/version-two.ts" {
					t.Fatalf("entries = %+v", response.Entries)
				}
			},
		},
		{
			path:    "/files/stat?path=tasks/version-two.ts&source=version&version_id=00000000-0000-0000-0000-000000000402",
			handler: server.statWorkspaceFile,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
				}
				var response api.WorkspaceFileStatResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Entry.Kind != "file" || response.Entry.SizeBytes == 0 {
					t.Fatalf("entry = %+v", response.Entry)
				}
			},
		},
		{
			path:      "/versions/00000000-0000-0000-0000-000000000402",
			versionID: "00000000-0000-0000-0000-000000000402",
			handler:   server.getWorkspaceVersion,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
				}
				var response api.WorkspaceVersionEnvelope
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if response.Version.ID != "00000000-0000-0000-0000-000000000402" || response.Version.State != "ready" {
					t.Fatalf("version = %+v", response.Version)
				}
			},
		},
		{
			path:    "/versions",
			handler: server.listWorkspaceVersions,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				t.Helper()
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
				}
				var response api.ListWorkspaceVersionsResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Versions) != 2 {
					t.Fatalf("versions = %+v", response.Versions)
				}
			},
		},
	}

	for _, request := range requests {
		req := workspaceFilesTestRequest(http.MethodGet, request.path, request.versionID)
		rec := httptest.NewRecorder()
		request.handler(rec, req)
		request.check(t, rec)
	}
	if store.ensureWorkspaceMountCalls != 0 || store.claimWorkspaceMountCalls != 0 {
		t.Fatalf("mount calls = ensure %d claim %d", store.ensureWorkspaceMountCalls, store.claimWorkspaceMountCalls)
	}
}

func TestWorkspaceFilesErrorMappingAndPagination(t *testing.T) {
	readyVersionID := pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000401"))
	store := &fakeStore{
		workspace: db.Workspace{
			ID:                  testWorkspaceID(),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			DeploymentSandboxID: testDeploymentSandboxID(),
			SandboxID:           "default",
			SandboxFingerprint:  testSandboxFingerprint(),
			CurrentVersionID:    readyVersionID,
			State:               db.WorkspaceStateActive,
			DesiredState:        db.WorkspaceDesiredStateActive,
			DirtyState:          db.WorkspaceDirtyStateClean,
			Metadata:            []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		workspaceVersions: []db.WorkspaceVersion{workspaceFileTestVersion(readyVersionID, testWorkspaceID(), testArtifactID())},
		artifacts:         []db.Artifact{workspaceFileTestArtifact(testArtifactID(), "sha256:"+strings.Repeat("e", 64))},
	}
	server := newWorkspaceFilesTestServerWithCAS(store, &fakeCAS{bodies: map[string][]byte{
		"sha256:" + strings.Repeat("e", 64): workspaceFileSourceTar(t, []workspaceFileTarFile{
			{path: "tasks/a.ts", contents: "a\n"},
			{path: "tasks/b.ts", contents: "b\n"},
			{path: "tasks/c.ts", contents: "c\n"},
		}),
	}})

	pageOne := httptest.NewRecorder()
	server.listWorkspaceFiles(pageOne, workspaceFilesTestRequest(http.MethodGet, "/files?path=tasks&limit=2", ""))
	if pageOne.Code != http.StatusOK {
		t.Fatalf("page one status = %d body=%s", pageOne.Code, pageOne.Body.String())
	}
	var first api.ListWorkspaceFilesResponse
	if err := json.Unmarshal(pageOne.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) != 2 || first.Entries[0].Path != "tasks/a.ts" || first.Entries[1].Path != "tasks/b.ts" || first.NextCursor != "tasks/b.ts" {
		t.Fatalf("page one = %+v", first)
	}
	pageTwo := httptest.NewRecorder()
	server.listWorkspaceFiles(pageTwo, workspaceFilesTestRequest(http.MethodGet, "/files?path=tasks&limit=2&cursor=tasks/b.ts", ""))
	if pageTwo.Code != http.StatusOK {
		t.Fatalf("page two status = %d body=%s", pageTwo.Code, pageTwo.Body.String())
	}
	var second api.ListWorkspaceFilesResponse
	if err := json.Unmarshal(pageTwo.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Entries) != 1 || second.Entries[0].Path != "tasks/c.ts" || second.NextCursor != "" {
		t.Fatalf("page two = %+v", second)
	}

	cases := []struct {
		name       string
		target     string
		handler    func(http.ResponseWriter, *http.Request)
		statusCode int
		code       string
	}{
		{name: "missing", target: "/files/stat?path=tasks/missing.ts", handler: server.statWorkspaceFile, statusCode: http.StatusNotFound, code: "workspace_file_not_found"},
		{name: "read dir", target: "/files/content?path=.", handler: server.readWorkspaceFile, statusCode: http.StatusUnprocessableEntity, code: "workspace_file_not_regular"},
		{name: "list file", target: "/files?path=tasks/a.ts", handler: server.listWorkspaceFiles, statusCode: http.StatusUnprocessableEntity, code: "workspace_file_not_directory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.handler(rec, workspaceFilesTestRequest(http.MethodGet, tc.target, ""))
			if rec.Code != tc.statusCode {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			requireErrorCode(t, rec.Body.Bytes(), tc.code)
		})
	}

	server = newWorkspaceFilesTestServerWithCAS(store, &fakeCAS{bodies: map[string][]byte{
		"sha256:" + strings.Repeat("e", 64): oversizedWorkspaceFileTar(t),
	}})
	tooLarge := httptest.NewRecorder()
	server.readWorkspaceFile(tooLarge, workspaceFilesTestRequest(http.MethodGet, "/files/content?path=huge.bin", ""))
	if tooLarge.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too large status = %d body=%s", tooLarge.Code, tooLarge.Body.String())
	}
	requireErrorCode(t, tooLarge.Body.Bytes(), "workspace_file_too_large")

	store.workspace.CurrentVersionID = pgtype.UUID{}
	noCurrent := httptest.NewRecorder()
	server.statWorkspaceFile(noCurrent, workspaceFilesTestRequest(http.MethodGet, "/files/stat?path=tasks/a.ts", ""))
	if noCurrent.Code != http.StatusNotFound {
		t.Fatalf("no current status = %d body=%s", noCurrent.Code, noCurrent.Body.String())
	}
	requireErrorCode(t, noCurrent.Body.Bytes(), "workspace_no_current_version")
}

func TestWorkspaceFilesRejectInvalidSourceContracts(t *testing.T) {
	readyVersionID := pgvalue.UUID(uuid.MustParse("00000000-0000-0000-0000-000000000401"))
	store := &fakeStore{
		workspace: db.Workspace{
			ID:                  testWorkspaceID(),
			OrgID:               pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID:           testProjectID(),
			EnvironmentID:       testEnvironmentID(),
			DeploymentSandboxID: testDeploymentSandboxID(),
			SandboxID:           "default",
			SandboxFingerprint:  testSandboxFingerprint(),
			CurrentVersionID:    readyVersionID,
			State:               db.WorkspaceStateActive,
			DesiredState:        db.WorkspaceDesiredStateActive,
			DirtyState:          db.WorkspaceDirtyStateClean,
			Metadata:            []byte(`{}`),
			CreatedAt:           testTime(),
			UpdatedAt:           testTime(),
		},
		workspaceVersions: []db.WorkspaceVersion{workspaceFileTestVersion(readyVersionID, testWorkspaceID(), testArtifactID())},
	}
	server := newWorkspaceFilesTestServer(store, validDeploymentSourceTar(t))

	cases := []struct {
		path       string
		statusCode int
		code       string
	}{
		{
			path:       "/files/stat?path=helmr.config.ts&version_id=00000000-0000-0000-0000-000000000401",
			statusCode: http.StatusBadRequest,
			code:       "workspace_version_id_unexpected",
		},
		{
			path:       "/files/stat?path=helmr.config.ts&source=version",
			statusCode: http.StatusBadRequest,
			code:       "workspace_version_id_required",
		},
		{
			path:       "/files/stat?path=helmr.config.ts&workspace_mount_id=00000000-0000-0000-0000-000000000501",
			statusCode: http.StatusBadRequest,
			code:       "workspace_mount_id_unexpected",
		},
		{
			path:       "/files/stat?path=helmr.config.ts&source=version&version_id=00000000-0000-0000-0000-000000000401&workspace_mount_id=00000000-0000-0000-0000-000000000501",
			statusCode: http.StatusBadRequest,
			code:       "workspace_mount_id_unexpected",
		},
		{
			path:       "/files/stat?path=helmr.config.ts&source=live",
			statusCode: http.StatusNotImplemented,
			code:       "workspace_source_live_unsupported",
		},
		{
			path:       "/files/stat?path=helmr.config.ts&source=version&version_id=00000000-0000-0000-0000-000000000999",
			statusCode: http.StatusNotFound,
			code:       "workspace_version_not_readable",
		},
	}
	for _, tc := range cases {
		req := workspaceFilesTestRequest(http.MethodGet, tc.path, "")
		rec := httptest.NewRecorder()
		server.statWorkspaceFile(rec, req)
		if rec.Code != tc.statusCode {
			t.Fatalf("%s status = %d body=%s", tc.path, rec.Code, rec.Body.String())
		}
		requireErrorCode(t, rec.Body.Bytes(), tc.code)
	}
}

func newWorkspaceFilesTestServer(store *fakeStore, body []byte) *Server {
	return newWorkspaceFilesTestServerWithCAS(store, &fakeCAS{body: body})
}

func newWorkspaceFilesTestServerWithCAS(store *fakeStore, cas *fakeCAS) *Server {
	return &Server{
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		workerGroupID: dbtest.DefaultWorkerGroupID,
		db:            store,
		cas:           cas,
	}
}

func workspaceFilesTestRequest(method, target, versionID string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", testProjectIDString())
	routeContext.URLParams.Add("environmentID", testEnvironmentIDString())
	routeContext.URLParams.Add("workspaceID", "00000000-0000-0000-0000-000000000308")
	if versionID != "" {
		routeContext.URLParams.Add("versionID", versionID)
	}
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{
		OrgID:         dbtest.DefaultOrgID,
		ProjectID:     testProjectIDString(),
		EnvironmentID: testEnvironmentIDString(),
		UserID:        uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		Role:          auth.RoleOwner,
		Kind:          auth.ActorKindSession,
		Permissions: []auth.Permission{
			auth.PermissionFilesRead,
			auth.PermissionVersionsRead,
		},
	})
	return req.WithContext(ctx)
}

func workspaceFileTestVersion(id, workspaceID, artifactID pgtype.UUID) db.WorkspaceVersion {
	return db.WorkspaceVersion{
		ID:                 id,
		OrgID:              pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:          testProjectID(),
		EnvironmentID:      testEnvironmentID(),
		WorkspaceID:        workspaceID,
		Kind:               db.WorkspaceVersionKindUser,
		State:              db.WorkspaceVersionStateReady,
		ArtifactID:         artifactID,
		ArtifactEncoding:   workspace.ArtifactEncoding,
		ArtifactEntryCount: 2,
		ContentDigest:      "sha256:" + strings.Repeat("c", 64),
		SizeBytes:          1024,
		Message:            "captured workspace",
		PromotedAt:         testTime(),
		CreatedAt:          testTime(),
	}
}

func workspaceFileTestArtifact(id pgtype.UUID, digest string) db.Artifact {
	return db.Artifact{
		ID:            id,
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		Digest:        digest,
		Kind:          db.ArtifactKindWorkspaceVersion,
		SizeBytes:     1024,
		MediaType:     workspace.ArtifactMediaType,
		CreatedAt:     testTime(),
	}
}

type workspaceFileTarFile struct {
	path     string
	contents string
}

func workspaceFileSourceTar(t *testing.T, files []workspaceFileTarFile) []byte {
	t.Helper()
	headers := make([]tar.Header, 0, len(files))
	contents := make([]string, 0, len(files))
	for _, file := range files {
		headers = append(headers, tar.Header{Name: file.path, Mode: 0o644, Size: int64(len(file.contents))})
		contents = append(contents, file.contents)
	}
	return deploymentSourceTar(t, headers, contents)
}

func oversizedWorkspaceFileTar(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "huge.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: workspaceFileReadMaxBytes + 1}); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}
