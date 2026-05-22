package control

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateDeploymentCreatesDeployedCatalog(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 12, MediaType: api.TaskSourceArtifactMediaType}}
	server := &Server{
		db:  store,
		cas: artifactStore,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, api.CreateDeploymentRequest{
		Tasks: []api.DeploymentTaskCreate{{
			TaskID:     "review-pr",
			ModulePath: "tasks/review-pr.ts",
			ExportName: "reviewPr",
		}},
	}, validTaskSourceTar(t),
	)
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.deployment.SourceDigest != artifactStore.object.Digest {
		t.Fatalf("deployment = %+v", store.deployment)
	}
	if store.deployment.Status != db.DeploymentStatusDeployed {
		t.Fatalf("deployment status = %s", store.deployment.Status)
	}
	if len(store.deploymentTasks) != 1 || store.deploymentTasks[0].TaskID != "review-pr" || store.deploymentTasks[0].ModulePath != "tasks/review-pr.ts" {
		t.Fatalf("deployment tasks = %+v", store.deploymentTasks)
	}
	if store.deploymentTasks[0].RequestedMilliCpu != 2000 || store.deploymentTasks[0].RequestedMemoryMib != 2048 {
		t.Fatalf("deployment task resources = %+v", store.deploymentTasks[0])
	}
	var response api.DeploymentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SourceArtifact.Digest != artifactStore.object.Digest || response.SourceArtifact.MediaType != api.TaskSourceArtifactMediaType {
		t.Fatalf("response = %+v", response)
	}
}

func TestCreateDeploymentReportsTaskIndexValidation(t *testing.T) {
	store := &fakeStore{}
	server := &Server{
		db:  store,
		cas: &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("b", 64), MediaType: api.TaskSourceArtifactMediaType}},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, api.CreateDeploymentRequest{
		Tasks: []api.DeploymentTaskCreate{{TaskID: "bad space", ModulePath: "tasks/review-pr.ts", ExportName: "reviewPr"}},
	}, validTaskSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("task_id")) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestDeploymentRouteAllowsAPIKeyWithProjectManage(t *testing.T) {
	store := &fakeStore{}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{
			kind: auth.ActorKindAPIKey,
			permissions: []auth.PermissionGrant{{
				ProjectID:     auth.DefaultProjectID,
				EnvironmentID: auth.DefaultEnvironmentID,
				Permissions:   []auth.Permission{auth.PermissionTasksDeploy},
			}},
		}),
		WithCAS(&fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("c", 64), MediaType: api.TaskSourceArtifactMediaType}}),
	)
	body, contentType := deploymentMultipart(t, api.CreateDeploymentRequest{
		Tasks: []api.DeploymentTaskCreate{{TaskID: "review-pr", ModulePath: "tasks/review-pr.ts", ExportName: "reviewPr"}},
	}, validTaskSourceTar(t))
	req := httptest.NewRequest(http.MethodPost, "/api/deployments", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer machine-key")
	req.Header.Set("content-type", contentType)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.deployment.SourceDigest == "" {
		t.Fatalf("deployment = %+v", store.deployment)
	}
}

func TestDeploymentRouteAuthorizesBeforeReadingSourceTar(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("f", 64), MediaType: api.TaskSourceArtifactMediaType}}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithAuthenticator(fakeAuth{kind: auth.ActorKindAPIKey}),
		WithCAS(artifactStore),
	)
	boundary := "helmr-test-boundary"
	body := strings.Join([]string{
		"--" + boundary,
		`Content-Disposition: form-data; name="metadata"`,
		"",
		`{"tasks":[{"task_id":"review-pr","module_path":"tasks/review-pr.ts","export_name":"reviewPr"}]}`,
		"--" + boundary,
		`Content-Disposition: form-data; name="source_tar"; filename="source.tar"`,
		"Content-Type: application/x-tar",
		"",
		"truncated source archive without closing boundary",
	}, "\r\n")
	req := httptest.NewRequest(http.MethodPost, "/api/deployments", strings.NewReader(body))
	req.Header.Set("authorization", "Bearer machine-key")
	req.Header.Set("content-type", "multipart/form-data; boundary="+boundary)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if artifactStore.body != nil || artifactStore.deletedDigest != "" {
		t.Fatalf("source archive was processed: body=%d deleted=%q", len(artifactStore.body), artifactStore.deletedDigest)
	}
}

func TestProjectManagementRejectsDeletingDefaultProject(t *testing.T) {
	projectID := ids.New()
	store := &projectManagementStore{
		project: db.Project{
			ID:        ids.ToPG(projectID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			Slug:      "main",
			Name:      "Main",
			IsDefault: true,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	server := &Server{db: store}
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID: ids.DefaultOrgID,
		Role:  auth.RoleOwner,
		Kind:  auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.archiveProject(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.archivedProject {
		t.Fatal("default project was archived")
	}
}

func TestProjectRoutesAcceptBearerSession(t *testing.T) {
	authSecret := "abcdefghijabcdefghijabcdefghij12"
	rawSession := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNO"
	sessionHash, err := auth.HashToken([]byte(authSecret), rawSession)
	if err != nil {
		t.Fatal(err)
	}
	projectID := ids.New()
	store := &projectManagementStore{
		sessionHash: sessionHash,
		session: db.GetSessionByTokenHashRow{
			ID:        ids.ToPG(ids.New()),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			UserID:    ids.ToPG(ids.New()),
			Role:      string(db.OrgMemberRoleOwner),
			ExpiresAt: pgTimeToPG(time.Now().Add(time.Hour)),
		},
		project: db.Project{
			ID:        ids.ToPG(projectID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			Slug:      "main",
			Name:      "Main",
			IsDefault: true,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(store),
		WithUserAuth(authSecret, "https://helmr.example.test"),
		WithSessionTTL(time.Hour),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("authorization", "Bearer "+rawSession)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.ListProjectsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Projects) != 1 || response.Projects[0].Slug != "main" {
		t.Fatalf("response = %+v", response)
	}
}

func TestProjectRoutesRejectAPIKeyBearer(t *testing.T) {
	server := New(
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithDB(&projectManagementStore{}),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProjectManagementUpdatesEnvironment(t *testing.T) {
	projectID := ids.New()
	environmentID := ids.New()
	store := &projectManagementStore{
		project: db.Project{
			ID:    ids.ToPG(projectID),
			OrgID: ids.ToPG(ids.DefaultOrgID),
			Slug:  "main",
			Name:  "Main",
		},
		environment: db.Environment{
			ID:        ids.ToPG(environmentID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			ProjectID: ids.ToPG(projectID),
			Slug:      "staging",
			Name:      "Staging",
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	server := &Server{db: store}
	req := httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID.String()+"/environments/"+environmentID.String(), strings.NewReader(`{"slug":"qa","name":"QA"}`))
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID: ids.DefaultOrgID,
		Role:  auth.RoleOwner,
		Kind:  auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID.String())
	routeContext.URLParams.Add("environmentID", environmentID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.updateEnvironment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.updatedEnvironment.Slug != "qa" || store.updatedEnvironment.Name != "QA" {
		t.Fatalf("update = %+v", store.updatedEnvironment)
	}
	var response api.EnvironmentSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Slug != "qa" || response.Name != "QA" {
		t.Fatalf("response = %+v", response)
	}
}

func TestProjectManagementRejectsDeletingDefaultEnvironment(t *testing.T) {
	projectID := ids.New()
	environmentID := ids.New()
	store := &projectManagementStore{
		environment: db.Environment{
			ID:        ids.ToPG(environmentID),
			OrgID:     ids.ToPG(ids.DefaultOrgID),
			ProjectID: ids.ToPG(projectID),
			Slug:      "production",
			Name:      "Production",
			IsDefault: true,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	server := &Server{db: store}
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID.String()+"/environments/"+environmentID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID: ids.DefaultOrgID,
		Role:  auth.RoleOwner,
		Kind:  auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID.String())
	routeContext.URLParams.Add("environmentID", environmentID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.archiveEnvironment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.archivedEnvironment {
		t.Fatal("default environment was archived")
	}
}

func TestGetCurrentDeploymentReturnsCatalog(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	store := &fakeStore{
		deployment: db.Deployment{
			ID:            testDeploymentID(),
			OrgID:         ids.ToPG(ids.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			SourceDigest:  digest,
			Status:        db.DeploymentStatusDeployed,
			CreatedAt:     testTime(),
			DeployedAt:    testTime(),
		},
		deploymentTasks: []db.DeploymentTask{
			{
				ID:            testDeploymentTaskID(),
				OrgID:         ids.ToPG(ids.DefaultOrgID),
				ProjectID:     testProjectID(),
				EnvironmentID: testEnvironmentID(),
				DeploymentID:  testDeploymentID(),
				TaskID:        "review-pr",
				ModulePath:    "tasks/review-pr.ts",
				ExportName:    "reviewPr",
				CreatedAt:     testTime(),
			},
		},
	}
	server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := currentDeploymentRequest()
	rec := httptest.NewRecorder()

	server.getCurrentDeployment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.GetCurrentDeploymentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Deployment == nil {
		t.Fatal("deployment is nil")
	}
	if response.Deployment.SourceArtifact.Digest != digest {
		t.Fatalf("source artifact = %+v", response.Deployment.SourceArtifact)
	}
	if len(response.Deployment.Tasks) != 1 || response.Deployment.Tasks[0].TaskID != "review-pr" {
		t.Fatalf("tasks = %+v", response.Deployment.Tasks)
	}
}

func TestGetCurrentDeploymentReturnsEmptyWhenNotDeployed(t *testing.T) {
	server := &Server{db: &fakeStore{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := currentDeploymentRequest()
	rec := httptest.NewRecorder()

	server.getCurrentDeployment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.GetCurrentDeploymentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Deployment != nil {
		t.Fatalf("deployment = %+v", response.Deployment)
	}
}

func TestCreateDeploymentRejectsUnsafeSourceTar(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("d", 64), MediaType: api.TaskSourceArtifactMediaType}}
	server := &Server{
		db:  store,
		cas: artifactStore,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, api.CreateDeploymentRequest{
		Tasks: []api.DeploymentTaskCreate{{TaskID: "review-pr", ModulePath: "tasks/review-pr.ts", ExportName: "reviewPr"}},
	}, unsafeTaskSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if artifactStore.body != nil {
		t.Fatal("unsafe task source artifact was stored")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("unsafe")) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestCreateDeploymentDeletesUnreferencedArtifactAfterDBFailure(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	store := &fakeStore{
		createDeploymentErr: errors.New("insert deployment"),
		getCasObjectErr:     pgx.ErrNoRows,
	}
	artifactStore := &fakeCAS{object: cas.Object{Digest: digest, MediaType: api.TaskSourceArtifactMediaType}}
	server := &Server{
		db:  store,
		cas: artifactStore,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, api.CreateDeploymentRequest{
		Tasks: []api.DeploymentTaskCreate{{TaskID: "review-pr", ModulePath: "tasks/review-pr.ts", ExportName: "reviewPr"}},
	}, validTaskSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if artifactStore.deletedDigest != digest {
		t.Fatalf("deleted digest = %q", artifactStore.deletedDigest)
	}
}

func idsMustString(value pgtype.UUID) string {
	return ids.MustFromPG(value).String()
}

func deploymentRequest(body []byte, contentType string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/deployments", bytes.NewReader(body))
	req.Header.Set("content-type", contentType)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", idsMustString(testProjectID()))
	routeContext.URLParams.Add("environmentID", idsMustString(testEnvironmentID()))
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{OrgID: ids.DefaultOrgID, Role: auth.RoleOwner, Kind: auth.ActorKindSession})
	return req.WithContext(ctx)
}

func currentDeploymentRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/deployments/current?project_id=default&environment_id=default", nil)
	ctx := context.WithValue(req.Context(), actorContextKey{}, auth.Actor{OrgID: ids.DefaultOrgID, Role: auth.RoleViewer, Kind: auth.ActorKindSession})
	return req.WithContext(ctx)
}

func deploymentMultipart(t *testing.T, metadata api.CreateDeploymentRequest, source []byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataBody, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("metadata", string(metadataBody)); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("source_tar", "source.tar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(source); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func validTaskSourceTar(t *testing.T) []byte {
	t.Helper()
	return taskSourceTar(t, []tar.Header{
		{Name: "helmr.config.ts", Mode: 0o644, Size: int64(len("export default {}\n"))},
		{Name: "tasks/review-pr.ts", Mode: 0o644, Size: int64(len("export const reviewPr = {}\n"))},
	}, []string{
		"export default {}\n",
		"export const reviewPr = {}\n",
	})
}

func unsafeTaskSourceTar(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "tasks/outside", Typeflag: tar.TypeSymlink, Linkname: "../../outside"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func taskSourceTar(t *testing.T, headers []tar.Header, contents []string) []byte {
	t.Helper()
	if len(headers) != len(contents) {
		t.Fatalf("headers/content length mismatch")
	}
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for i := range headers {
		header := headers[i]
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if contents[i] != "" {
			if _, err := writer.Write([]byte(contents[i])); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

type fakeCAS struct {
	object        cas.Object
	body          []byte
	deletedDigest string
}

type projectManagementStore struct {
	db.Querier
	project             db.Project
	environment         db.Environment
	updatedEnvironment  db.UpdateEnvironmentDetailsParams
	archivedProject     bool
	archivedEnvironment bool
	sessionHash         []byte
	session             db.GetSessionByTokenHashRow
	refreshedSession    pgtype.UUID
}

func (s *projectManagementStore) GetProject(_ context.Context, arg db.GetProjectParams) (db.Project, error) {
	if s.project.ID == (pgtype.UUID{}) || s.project.OrgID != arg.OrgID || s.project.ID != arg.ID {
		return db.Project{}, pgx.ErrNoRows
	}
	return s.project, nil
}

func (s *projectManagementStore) ListProjects(_ context.Context, orgID pgtype.UUID) ([]db.Project, error) {
	if s.project.ID == (pgtype.UUID{}) || s.project.OrgID != orgID {
		return nil, nil
	}
	return []db.Project{s.project}, nil
}

func (s *projectManagementStore) ArchiveProjectWithEnvironments(_ context.Context, arg db.ArchiveProjectWithEnvironmentsParams) (db.ArchiveProjectWithEnvironmentsRow, error) {
	if s.project.ID == (pgtype.UUID{}) || s.project.OrgID != arg.OrgID || s.project.ID != arg.ID {
		return db.ArchiveProjectWithEnvironmentsRow{}, pgx.ErrNoRows
	}
	s.archivedProject = true
	return db.ArchiveProjectWithEnvironmentsRow{
		ID:         s.project.ID,
		OrgID:      s.project.OrgID,
		Slug:       s.project.Slug,
		Name:       s.project.Name,
		IsDefault:  s.project.IsDefault,
		ArchivedAt: s.project.ArchivedAt,
		CreatedAt:  s.project.CreatedAt,
		UpdatedAt:  s.project.UpdatedAt,
	}, nil
}

func (s *projectManagementStore) GetEnvironment(_ context.Context, arg db.GetEnvironmentParams) (db.Environment, error) {
	if s.environment.ID == (pgtype.UUID{}) ||
		s.environment.OrgID != arg.OrgID ||
		s.environment.ProjectID != arg.ProjectID ||
		s.environment.ID != arg.ID {
		return db.Environment{}, pgx.ErrNoRows
	}
	return s.environment, nil
}

func (s *projectManagementStore) ListEnvironments(_ context.Context, arg db.ListEnvironmentsParams) ([]db.Environment, error) {
	if s.environment.ID == (pgtype.UUID{}) || s.environment.OrgID != arg.OrgID || s.environment.ProjectID != arg.ProjectID {
		return nil, nil
	}
	return []db.Environment{s.environment}, nil
}

func (s *projectManagementStore) UpdateEnvironmentDetails(_ context.Context, arg db.UpdateEnvironmentDetailsParams) (db.Environment, error) {
	if s.environment.ID == (pgtype.UUID{}) ||
		s.environment.OrgID != arg.OrgID ||
		s.environment.ProjectID != arg.ProjectID ||
		s.environment.ID != arg.ID {
		return db.Environment{}, pgx.ErrNoRows
	}
	s.updatedEnvironment = arg
	updated := s.environment
	updated.Slug = arg.Slug
	updated.Name = arg.Name
	return updated, nil
}

func (s *projectManagementStore) ArchiveEnvironment(_ context.Context, arg db.ArchiveEnvironmentParams) (db.Environment, error) {
	if s.environment.ID == (pgtype.UUID{}) ||
		s.environment.OrgID != arg.OrgID ||
		s.environment.ProjectID != arg.ProjectID ||
		s.environment.ID != arg.ID {
		return db.Environment{}, pgx.ErrNoRows
	}
	s.archivedEnvironment = true
	return s.environment, nil
}

func (s *projectManagementStore) GetSessionByTokenHash(_ context.Context, hash []byte) (db.GetSessionByTokenHashRow, error) {
	if !bytes.Equal(hash, s.sessionHash) {
		return db.GetSessionByTokenHashRow{}, pgx.ErrNoRows
	}
	return s.session, nil
}

func (s *projectManagementStore) RefreshSession(_ context.Context, arg db.RefreshSessionParams) error {
	if s.session.ID != arg.ID {
		return pgx.ErrNoRows
	}
	s.refreshedSession = arg.ID
	return nil
}

func (f *fakeCAS) Put(_ context.Context, mediaType string, body io.Reader) (cas.Object, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return cas.Object{}, err
	}
	f.body = content
	if f.object.MediaType == "" {
		f.object.MediaType = mediaType
	}
	if f.object.SizeBytes == 0 {
		f.object.SizeBytes = int64(len(content))
	}
	return f.object, nil
}

func (f *fakeCAS) Stat(context.Context, string) (cas.Object, error) {
	return f.object, nil
}

func (f *fakeCAS) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func (f *fakeCAS) Delete(_ context.Context, digest string) error {
	f.deletedDigest = digest
	return nil
}
