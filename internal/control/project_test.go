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
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateDeploymentQueuesDeploymentSourceForBuild(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 12, MediaType: api.DeploymentSourceArtifactMediaType}}
	server := &Server{
		db:  store,
		cas: artifactStore,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, defaultDeploymentMetadata(), validDeploymentSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.deployment.DeploymentSourceDigest != artifactStore.object.Digest {
		t.Fatalf("deployment = %+v", store.deployment)
	}
	if store.deployment.ContentHash != cas.DigestBytes(validDeploymentSourceTar(t)) {
		t.Fatalf("deployment content_hash = %q", store.deployment.ContentHash)
	}
	if store.deployment.Status != db.DeploymentStatusQueued {
		t.Fatalf("deployment status = %s", store.deployment.Status)
	}
	if len(store.deploymentTasks) != 0 {
		t.Fatalf("deployment tasks = %+v", store.deploymentTasks)
	}
	var response api.DeploymentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["deployment_source"]; !ok {
		t.Fatalf("deployment_source missing from response: %s", rec.Body.String())
	}
	for _, oldField := range []string{"source_artifact", "indexed_at"} {
		if _, ok := raw[oldField]; ok {
			t.Fatalf("legacy field %q present in response: %s", oldField, rec.Body.String())
		}
	}
	if response.ContentHash != cas.DigestBytes(validDeploymentSourceTar(t)) {
		t.Fatalf("content hash = %q", response.ContentHash)
	}
	if response.DeploymentSource.Digest != artifactStore.object.Digest || response.DeploymentSource.MediaType != api.DeploymentSourceArtifactMediaType {
		t.Fatalf("response = %+v", response)
	}
}

func TestCreateDeploymentRejectsMissingProject(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 12, MediaType: api.DeploymentSourceArtifactMediaType}}
	server := &Server{
		db:  store,
		cas: artifactStore,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, api.CreateDeploymentRequest{}, validDeploymentSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if artifactStore.body != nil {
		t.Fatal("deployment source artifact was stored")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("project_id is required")) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestCreateDeploymentRejectsStandaloneScopeFields(t *testing.T) {
	server := &Server{
		db:  &fakeStore{},
		cas: &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 12, MediaType: api.DeploymentSourceArtifactMediaType}},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("project_id", auth.DefaultProjectID); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("metadata", `{"content_hash":"sha256:`+strings.Repeat("0", 64)+`"}`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := deploymentRequest(body.Bytes(), writer.FormDataContentType())
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("unexpected deployment multipart field")) || !bytes.Contains(rec.Body.Bytes(), []byte("project_id")) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestCreateDeploymentReusesDeployedContentHashWithoutPromotion(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	store := &fakeStore{
		createDeploymentResult: &db.Deployment{
			ID:                     testDeploymentID(),
			OrgID:                  ids.ToPG(ids.DefaultOrgID),
			ProjectID:              testProjectID(),
			EnvironmentID:          testEnvironmentID(),
			ContentHash:            digest,
			DeploymentSourceDigest: digest,
			Status:                 db.DeploymentStatusDeployed,
			CreatedAt:              testTime(),
			BuildingAt:             testTime(),
			BuiltAt:                testTime(),
			DeployedAt:             testTime(),
		},
	}
	server := &Server{
		db:  store,
		cas: &fakeCAS{object: cas.Object{Digest: digest, SizeBytes: 12, MediaType: api.DeploymentSourceArtifactMediaType}},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, defaultDeploymentMetadata(), validDeploymentSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.deploymentPromotions) != 0 {
		t.Fatalf("deployment promotions = %+v", store.deploymentPromotions)
	}
	var response api.DeploymentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != string(db.DeploymentStatusDeployed) {
		t.Fatalf("response status = %s", response.Status)
	}
}

func TestCreateDeploymentDoesNotValidateTaskIndexMetadata(t *testing.T) {
	store := &fakeStore{}
	server := &Server{
		db:  store,
		cas: &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("b", 64), MediaType: api.DeploymentSourceArtifactMediaType}},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, defaultDeploymentMetadata(), validDeploymentSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
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
		WithCAS(&fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("c", 64), MediaType: api.DeploymentSourceArtifactMediaType}}),
	)
	body, contentType := deploymentMultipart(t, defaultDeploymentMetadata(), validDeploymentSourceTar(t))
	req := httptest.NewRequest(http.MethodPost, "/api/deployments", bytes.NewReader(body))
	req.Header.Set("authorization", "Bearer machine-key")
	req.Header.Set("content-type", contentType)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.deployment.DeploymentSourceDigest == "" {
		t.Fatalf("deployment = %+v", store.deployment)
	}
}

func TestDeploymentRouteAuthorizesBeforeReadingDeploymentSource(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("f", 64), MediaType: api.DeploymentSourceArtifactMediaType}}
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
		`{"project_id":"default"}`,
		"--" + boundary,
		`Content-Disposition: form-data; name="deployment_source"; filename="deployment-source.tar"`,
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

func TestProjectManagementDeletesProject(t *testing.T) {
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

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !store.deletedProject {
		t.Fatal("project was not deleted")
	}
	if store.completedDeletionJob.ID == (pgtype.UUID{}) || store.createdDeletionJob.TargetSlug != "main" {
		t.Fatalf("deletion job not completed: created=%+v completed=%+v", store.createdDeletionJob, store.completedDeletionJob)
	}
}

func TestProjectManagementPromotesSiblingWhenDeletingDefaultProject(t *testing.T) {
	defaultProjectID := ids.New()
	siblingProjectID := ids.New()
	store := &projectManagementStore{
		projects: []db.Project{
			{
				ID:        ids.ToPG(defaultProjectID),
				OrgID:     ids.ToPG(ids.DefaultOrgID),
				Slug:      "main",
				Name:      "Main",
				IsDefault: true,
				CreatedAt: testTime(),
				UpdatedAt: testTime(),
			},
			{
				ID:        ids.ToPG(siblingProjectID),
				OrgID:     ids.ToPG(ids.DefaultOrgID),
				Slug:      "next",
				Name:      "Next",
				IsDefault: false,
				CreatedAt: testTime(),
				UpdatedAt: testTime(),
			},
		},
	}
	server := &Server{db: store}
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+defaultProjectID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID: ids.DefaultOrgID,
		Role:  auth.RoleOwner,
		Kind:  auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", defaultProjectID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.archiveProject(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !store.deletedProject {
		t.Fatal("project was not deleted")
	}
	if store.projects[0].IsDefault {
		t.Fatal("deleted project remained default")
	}
	if !store.projects[1].IsDefault {
		t.Fatal("sibling project was not promoted")
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
			Slug:      "dev",
			Name:      "Dev",
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

func TestProjectManagementRejectsDeletingProtectedEnvironment(t *testing.T) {
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
	if store.deletedEnvironment {
		t.Fatal("protected environment was deleted")
	}
}

func TestGetCurrentDeploymentReturnsCatalog(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                     testDeploymentID(),
			OrgID:                  ids.ToPG(ids.DefaultOrgID),
			ProjectID:              testProjectID(),
			EnvironmentID:          testEnvironmentID(),
			DeploymentSourceDigest: digest,
			Status:                 db.DeploymentStatusDeployed,
			CreatedAt:              testTime(),
			DeployedAt:             testTime(),
		},
		deploymentTasks: []db.DeploymentTask{
			{
				ID:            testDeploymentTaskID(),
				OrgID:         ids.ToPG(ids.DefaultOrgID),
				ProjectID:     testProjectID(),
				EnvironmentID: testEnvironmentID(),
				DeploymentID:  testDeploymentID(),
				TaskID:        "review-pr",
				FilePath:      "tasks/review-pr.ts",
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
	if response.Deployment.DeploymentSource.Digest != digest {
		t.Fatalf("deployment source = %+v", response.Deployment.DeploymentSource)
	}
	if len(response.Deployment.Tasks) != 1 || response.Deployment.Tasks[0].TaskID != "review-pr" {
		t.Fatalf("tasks = %+v", response.Deployment.Tasks)
	}
}

func TestGetCurrentDeploymentReturnsEmptyWhenNotDeployed(t *testing.T) {
	server := &Server{db: &fakeStore{currentDeploymentMissing: true}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

func TestGetDeploymentReturnsFailedDeploymentError(t *testing.T) {
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                     testDeploymentID(),
			OrgID:                  ids.ToPG(ids.DefaultOrgID),
			ProjectID:              testProjectID(),
			EnvironmentID:          testEnvironmentID(),
			DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
			Status:                 db.DeploymentStatusFailed,
			Failure:                []byte(`{"message":"build failed"}`),
			CreatedAt:              testTime(),
			FailedAt:               testTime(),
		},
	}
	server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := deploymentStatusRequest(testDeploymentID())
	rec := httptest.NewRecorder()

	server.getDeployment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.DeploymentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != string(db.DeploymentStatusFailed) {
		t.Fatalf("status = %s", response.Status)
	}
	if response.Error == nil || response.Error.Message != "build failed" {
		t.Fatalf("error = %+v", response.Error)
	}
	if response.DeploymentSource.Digest != store.deployment.DeploymentSourceDigest {
		t.Fatalf("deployment source = %+v", response.DeploymentSource)
	}
	if len(response.Tasks) != 0 {
		t.Fatalf("tasks = %+v", response.Tasks)
	}
}

func TestGetDeploymentAllowsDeployPermission(t *testing.T) {
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                     testDeploymentID(),
			OrgID:                  ids.ToPG(ids.DefaultOrgID),
			ProjectID:              testProjectID(),
			EnvironmentID:          testEnvironmentID(),
			DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
			Status:                 db.DeploymentStatusQueued,
			Failure:                []byte(`{}`),
			CreatedAt:              testTime(),
		},
	}
	server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := deploymentStatusRequest(testDeploymentID())
	ctx := context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID: ids.DefaultOrgID,
		Role:  auth.RoleDeveloper,
		Kind:  auth.ActorKindAPIKey,
		Permissions: []auth.PermissionGrant{
			{ProjectID: auth.DefaultProjectID, EnvironmentID: auth.DefaultEnvironmentID, Permissions: []auth.Permission{auth.PermissionTasksDeploy}},
		},
	})
	rec := httptest.NewRecorder()

	server.getDeployment(rec, req.WithContext(ctx))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.DeploymentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("error = %+v", response.Error)
	}
}

func TestGetDeploymentReturnsTasksWhenDeployed(t *testing.T) {
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                     testDeploymentID(),
			OrgID:                  ids.ToPG(ids.DefaultOrgID),
			ProjectID:              testProjectID(),
			EnvironmentID:          testEnvironmentID(),
			DeploymentSourceDigest: "sha256:" + strings.Repeat("b", 64),
			Status:                 db.DeploymentStatusDeployed,
			CreatedAt:              testTime(),
			DeployedAt:             testTime(),
		},
		deploymentTasks: []db.DeploymentTask{
			{
				ID:            testDeploymentTaskID(),
				OrgID:         ids.ToPG(ids.DefaultOrgID),
				ProjectID:     testProjectID(),
				EnvironmentID: testEnvironmentID(),
				DeploymentID:  testDeploymentID(),
				TaskID:        "deploy",
				FilePath:      "tasks/deploy.ts",
				ExportName:    "deploy",
				CreatedAt:     testTime(),
			},
		},
	}
	server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := deploymentStatusRequest(testDeploymentID())
	rec := httptest.NewRecorder()

	server.getDeployment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var response api.DeploymentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Tasks) != 1 || response.Tasks[0].TaskID != "deploy" {
		t.Fatalf("tasks = %+v", response.Tasks)
	}
}

func TestPromoteDeploymentResolvesUniqueVersionWithoutScope(t *testing.T) {
	environmentID := ids.ToPG(uuid.MustParse("00000000-0000-0000-0000-000000000399"))
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                     testDeploymentID(),
			OrgID:                  ids.ToPG(ids.DefaultOrgID),
			ProjectID:              testProjectID(),
			EnvironmentID:          environmentID,
			Version:                "20260101.2",
			DeploymentSourceDigest: "sha256:" + strings.Repeat("b", 64),
			Status:                 db.DeploymentStatusDeployed,
			CreatedAt:              testTime(),
			DeployedAt:             testTime(),
		},
	}
	server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	req := promoteDeploymentRequest("20260101.2", `{}`)
	rec := httptest.NewRecorder()

	server.promoteDeployment(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(store.deploymentPromotions) != 1 {
		t.Fatalf("promotions = %+v", store.deploymentPromotions)
	}
	promotion := store.deploymentPromotions[0]
	if promotion.DeploymentID != testDeploymentID() || promotion.EnvironmentID != environmentID {
		t.Fatalf("promotion = %+v", promotion)
	}
}

func TestCreateDeploymentRejectsUnsafeSourceTar(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("d", 64), MediaType: api.DeploymentSourceArtifactMediaType}}
	server := &Server{
		db:  store,
		cas: artifactStore,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, defaultDeploymentMetadata(), unsafeDeploymentSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if artifactStore.body != nil {
		t.Fatal("unsafe deployment source artifact was stored")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("unsafe")) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestCreateDeploymentRejectsContentHashMismatch(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("d", 64), MediaType: api.DeploymentSourceArtifactMediaType}}
	server := &Server{
		db:  store,
		cas: artifactStore,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, api.CreateDeploymentRequest{ProjectID: auth.DefaultProjectID, ContentHash: "sha256:" + strings.Repeat("0", 64)}, validDeploymentSourceTar(t))
	req := deploymentRequest(body, contentType)
	rec := httptest.NewRecorder()

	server.createDeployment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if artifactStore.body != nil {
		t.Fatal("mismatched deployment source artifact was stored")
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("content_hash")) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestCreateDeploymentDeletesUnreferencedArtifactAfterDBFailure(t *testing.T) {
	digest := "sha256:" + strings.Repeat("e", 64)
	store := &fakeStore{
		createDeploymentErr: errors.New("insert deployment"),
		getCasObjectErr:     pgx.ErrNoRows,
	}
	artifactStore := &fakeCAS{object: cas.Object{Digest: digest, MediaType: api.DeploymentSourceArtifactMediaType}}
	server := &Server{
		db:  store,
		cas: artifactStore,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	body, contentType := deploymentMultipart(t, defaultDeploymentMetadata(), validDeploymentSourceTar(t))
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

func deploymentStatusRequest(deploymentID pgtype.UUID) *http.Request {
	id := ids.MustFromPG(deploymentID)
	req := httptest.NewRequest(http.MethodGet, "/api/deployments/"+id.String()+"?project_id=default&environment_id=default", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("deploymentID", id.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{OrgID: ids.DefaultOrgID, Role: auth.RoleViewer, Kind: auth.ActorKindSession})
	return req.WithContext(ctx)
}

func promoteDeploymentRequest(deploymentRef string, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/deployments/"+deploymentRef+"/promote", strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("deployment", deploymentRef)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{OrgID: ids.DefaultOrgID, UserID: ids.New(), Role: auth.RoleOwner, Kind: auth.ActorKindSession})
	return req.WithContext(ctx)
}

func (f *fakeStore) GetDeployment(_ context.Context, arg db.GetDeploymentParams) (db.Deployment, error) {
	if f.deployment.ID == (pgtype.UUID{}) {
		return db.Deployment{}, pgx.ErrNoRows
	}
	if f.deployment.OrgID != arg.OrgID || f.deployment.ProjectID != arg.ProjectID || f.deployment.EnvironmentID != arg.EnvironmentID || f.deployment.ID != arg.ID {
		return db.Deployment{}, pgx.ErrNoRows
	}
	return f.deployment, nil
}

func deploymentMultipart(t *testing.T, metadata api.CreateDeploymentRequest, source []byte) ([]byte, string) {
	t.Helper()
	if strings.TrimSpace(metadata.ContentHash) == "" {
		metadata.ContentHash = cas.DigestBytes(source)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	metadataBody, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("metadata", string(metadataBody)); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("deployment_source", "deployment-source.tar")
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

func defaultDeploymentMetadata() api.CreateDeploymentRequest {
	return api.CreateDeploymentRequest{ProjectID: auth.DefaultProjectID}
}

func validDeploymentSourceTar(t *testing.T) []byte {
	t.Helper()
	return deploymentSourceTar(t, []tar.Header{
		{Name: "helmr.config.ts", Mode: 0o644, Size: int64(len("export default {}\n"))},
		{Name: "tasks/review-pr.ts", Mode: 0o644, Size: int64(len("export const reviewPr = {}\n"))},
	}, []string{
		"export default {}\n",
		"export const reviewPr = {}\n",
	})
}

func unsafeDeploymentSourceTar(t *testing.T) []byte {
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

func deploymentSourceTar(t *testing.T, headers []tar.Header, contents []string) []byte {
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
	project              db.Project
	projects             []db.Project
	environment          db.Environment
	updatedEnvironment   db.UpdateEnvironmentDetailsParams
	deletedProject       bool
	deletedEnvironment   bool
	createdDeletionJob   db.DeletionJob
	runningDeletionJob   db.DeletionJob
	completedDeletionJob db.DeletionJob
	failedDeletionJob    db.DeletionJob
	sessionHash          []byte
	session              db.GetSessionByTokenHashRow
	refreshedSession     pgtype.UUID
}

func (s *projectManagementStore) GetProject(_ context.Context, arg db.GetProjectParams) (db.Project, error) {
	for _, project := range s.projects {
		if project.OrgID == arg.OrgID && project.ID == arg.ID {
			return project, nil
		}
	}
	if s.project.ID == (pgtype.UUID{}) || s.project.OrgID != arg.OrgID || s.project.ID != arg.ID {
		return db.Project{}, pgx.ErrNoRows
	}
	return s.project, nil
}

func (s *projectManagementStore) ListProjects(_ context.Context, orgID pgtype.UUID) ([]db.Project, error) {
	if len(s.projects) > 0 {
		projects := make([]db.Project, 0, len(s.projects))
		for _, project := range s.projects {
			if project.OrgID == orgID {
				projects = append(projects, project)
			}
		}
		return projects, nil
	}
	if s.project.ID == (pgtype.UUID{}) || s.project.OrgID != orgID {
		return nil, nil
	}
	return []db.Project{s.project}, nil
}

func (s *projectManagementStore) ClearDefaultProject(_ context.Context, orgID pgtype.UUID) (int64, error) {
	var rows int64
	for idx := range s.projects {
		if s.projects[idx].OrgID == orgID && s.projects[idx].IsDefault {
			s.projects[idx].IsDefault = false
			rows++
		}
	}
	if s.project.OrgID == orgID && s.project.IsDefault {
		s.project.IsDefault = false
		rows++
	}
	return rows, nil
}

func (s *projectManagementStore) SetDefaultProject(_ context.Context, arg db.SetDefaultProjectParams) (int64, error) {
	for idx := range s.projects {
		if s.projects[idx].OrgID == arg.OrgID && s.projects[idx].ID == arg.ID {
			s.projects[idx].IsDefault = true
			return 1, nil
		}
	}
	if s.project.OrgID == arg.OrgID && s.project.ID == arg.ID {
		s.project.IsDefault = true
		return 1, nil
	}
	return 0, nil
}

func (s *projectManagementStore) CreateDeletionJob(_ context.Context, arg db.CreateDeletionJobParams) (db.DeletionJob, error) {
	job := db.DeletionJob{
		ID:                   arg.ID,
		OrgID:                arg.OrgID,
		TargetType:           arg.TargetType,
		TargetID:             arg.TargetID,
		TargetProjectID:      arg.TargetProjectID,
		TargetSlug:           arg.TargetSlug,
		TargetName:           arg.TargetName,
		RequestedByPrincipal: arg.RequestedByPrincipal,
		Status:               db.DeletionJobStatusQueued,
		DeletedCounts:        []byte(`{}`),
		RequestedAt:          testTime(),
		UpdatedAt:            testTime(),
	}
	s.createdDeletionJob = job
	return job, nil
}

func (s *projectManagementStore) MarkDeletionJobRunning(_ context.Context, arg db.MarkDeletionJobRunningParams) (db.DeletionJob, error) {
	if s.createdDeletionJob.ID != arg.ID || s.createdDeletionJob.OrgID != arg.OrgID {
		return db.DeletionJob{}, pgx.ErrNoRows
	}
	job := s.createdDeletionJob
	job.Status = db.DeletionJobStatusRunning
	s.runningDeletionJob = job
	return job, nil
}

func (s *projectManagementStore) CompleteDeletionJob(_ context.Context, arg db.CompleteDeletionJobParams) (db.DeletionJob, error) {
	if s.createdDeletionJob.ID != arg.ID || s.createdDeletionJob.OrgID != arg.OrgID {
		return db.DeletionJob{}, pgx.ErrNoRows
	}
	job := s.createdDeletionJob
	job.Status = db.DeletionJobStatusCompleted
	job.DeletedCounts = arg.DeletedCounts
	s.completedDeletionJob = job
	return job, nil
}

func (s *projectManagementStore) FailDeletionJob(_ context.Context, arg db.FailDeletionJobParams) (db.DeletionJob, error) {
	if s.createdDeletionJob.ID != arg.ID || s.createdDeletionJob.OrgID != arg.OrgID {
		return db.DeletionJob{}, pgx.ErrNoRows
	}
	job := s.createdDeletionJob
	job.Status = db.DeletionJobStatusFailed
	job.Failure = arg.Failure
	s.failedDeletionJob = job
	return job, nil
}

func (s *projectManagementStore) DeleteProject(_ context.Context, arg db.DeleteProjectParams) (db.Project, error) {
	for idx, project := range s.projects {
		if project.OrgID == arg.OrgID && project.ID == arg.ID {
			s.deletedProject = true
			return s.projects[idx], nil
		}
	}
	if s.project.ID == (pgtype.UUID{}) || s.project.OrgID != arg.OrgID || s.project.ID != arg.ID {
		return db.Project{}, pgx.ErrNoRows
	}
	s.deletedProject = true
	return s.project, nil
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

func (s *projectManagementStore) DeleteEnvironment(_ context.Context, arg db.DeleteEnvironmentParams) (db.Environment, error) {
	if s.environment.ID == (pgtype.UUID{}) ||
		s.environment.OrgID != arg.OrgID ||
		s.environment.ProjectID != arg.ProjectID ||
		s.environment.ID != arg.ID {
		return db.Environment{}, pgx.ErrNoRows
	}
	if protectedEnvironmentSlug(s.environment.Slug) {
		return db.Environment{}, pgx.ErrNoRows
	}
	s.deletedEnvironment = true
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
	return f.put(mediaType, content), nil
}

func (f *fakeCAS) Stage(_ context.Context, mediaType string) (cas.Stage, error) {
	return &fakeCASStage{store: f, mediaType: mediaType}, nil
}

func (f *fakeCAS) put(mediaType string, content []byte) cas.Object {
	f.body = content
	if f.object.MediaType == "" {
		f.object.MediaType = mediaType
	}
	if f.object.SizeBytes == 0 {
		f.object.SizeBytes = int64(len(content))
	}
	return f.object
}

type fakeCASStage struct {
	store     *fakeCAS
	mediaType string
	content   bytes.Buffer
	closed    bool
}

func (s *fakeCASStage) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("stage is closed")
	}
	return s.content.Write(p)
}

func (s *fakeCASStage) Close() error {
	s.closed = true
	return nil
}

func (s *fakeCASStage) Commit(context.Context) (cas.Object, error) {
	s.closed = true
	return s.store.put(s.mediaType, s.content.Bytes()), nil
}

func (s *fakeCASStage) Abort(context.Context) error {
	s.closed = true
	return nil
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
