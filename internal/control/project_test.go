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
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestProjectManagementDeletesProject(t *testing.T) {
	projectID := uuid.Must(uuid.NewV7())
	store := &projectManagementStore{
		project: db.Project{
			ID:        pgvalue.UUID(projectID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
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
		OrgID:  dbtest.DefaultOrgID,
		UserID: uuid.Must(uuid.NewV7()),
		Role:   auth.RoleOwner,
		Kind:   auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.deleteProject(rec, req)

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

func TestProjectManagementMarksDeletionJobFailedWhenDeleteFails(t *testing.T) {
	projectID := uuid.Must(uuid.NewV7())
	store := &projectManagementStore{
		project: db.Project{
			ID:        pgvalue.UUID(projectID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			Slug:      "main",
			Name:      "Main",
			IsDefault: true,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
		deleteProjectErr: errors.New("delete failed"),
	}
	server := &Server{db: store}
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID:  dbtest.DefaultOrgID,
		UserID: uuid.Must(uuid.NewV7()),
		Role:   auth.RoleOwner,
		Kind:   auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.deleteProject(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.failedDeletionJob.ID == (pgtype.UUID{}) || store.failedDeletionJob.Status != db.DeletionJobStatusFailed {
		t.Fatalf("deletion job not failed: %+v", store.failedDeletionJob)
	}
	if store.completedDeletionJob.ID != (pgtype.UUID{}) {
		t.Fatalf("deletion job completed unexpectedly: %+v", store.completedDeletionJob)
	}
}

func TestProjectManagementPromotesSiblingWhenDeletingDefaultProject(t *testing.T) {
	defaultProjectID := uuid.Must(uuid.NewV7())
	siblingProjectID := uuid.Must(uuid.NewV7())
	store := &projectManagementStore{
		projects: []db.Project{
			{
				ID:        pgvalue.UUID(defaultProjectID),
				OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
				Slug:      "main",
				Name:      "Main",
				IsDefault: true,
				CreatedAt: testTime(),
				UpdatedAt: testTime(),
			},
			{
				ID:        pgvalue.UUID(siblingProjectID),
				OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
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
		OrgID:  dbtest.DefaultOrgID,
		UserID: uuid.Must(uuid.NewV7()),
		Role:   auth.RoleOwner,
		Kind:   auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", defaultProjectID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.deleteProject(rec, req)

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
	projectID := uuid.Must(uuid.NewV7())
	store := &projectManagementStore{
		sessionHash: sessionHash,
		session: db.GetAuthSessionByTokenHashRow{
			ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			UserID:    pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Role:      string(db.OrgMemberRoleOwner),
			ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)),
		},
		project: db.Project{
			ID:        pgvalue.UUID(projectID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			Slug:      "main",
			Name:      "Main",
			IsDefault: true,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, AuthSecret: []byte(authSecret), PublicURL: mustParseTestURL("https://helmr.example.test"), SessionTTL: time.Hour})
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &projectManagementStore{}})
	req := httptest.NewRequest(http.MethodGet, "/api/projects", nil)
	req.Header.Set("authorization", "Bearer "+auth.APIKeyPrefix+"test-key")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestProjectManagementUpdatesEnvironment(t *testing.T) {
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	store := &projectManagementStore{
		project: db.Project{
			ID:    pgvalue.UUID(projectID),
			OrgID: pgvalue.UUID(dbtest.DefaultOrgID),
			Slug:  "main",
			Name:  "Main",
		},
		environment: db.Environment{
			ID:        pgvalue.UUID(environmentID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID: pgvalue.UUID(projectID),
			Slug:      "dev",
			Name:      "Dev",
			ColorHex:  "#22C55E",
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	server := &Server{db: store}
	req := httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID.String()+"/environments/"+environmentID.String(), strings.NewReader(`{"slug":"qa","name":"QA","color_hex":"#f59e0b"}`))
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID: dbtest.DefaultOrgID,
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
	if store.updatedEnvironment.Slug != "qa" || store.updatedEnvironment.Name != "QA" || store.updatedEnvironment.ColorHex != "#F59E0B" {
		t.Fatalf("update = %+v", store.updatedEnvironment)
	}
	var response api.EnvironmentSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Slug != "qa" || response.Name != "QA" || response.ColorHex != "#F59E0B" {
		t.Fatalf("response = %+v", response)
	}
}

func TestProjectManagementRejectsInvalidEnvironmentColor(t *testing.T) {
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	store := &projectManagementStore{
		environment: db.Environment{
			ID:        pgvalue.UUID(environmentID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID: pgvalue.UUID(projectID),
			Slug:      "dev",
			Name:      "Dev",
			ColorHex:  "#22C55E",
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	server := &Server{db: store}
	req := httptest.NewRequest(http.MethodPatch, "/api/projects/"+projectID.String()+"/environments/"+environmentID.String(), strings.NewReader(`{"slug":"qa","name":"QA","color_hex":"blue"}`))
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID: dbtest.DefaultOrgID,
		Role:  auth.RoleOwner,
		Kind:  auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID.String())
	routeContext.URLParams.Add("environmentID", environmentID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.updateEnvironment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.updatedEnvironment != (db.UpdateEnvironmentDetailsParams{}) {
		t.Fatalf("update should not be called: %+v", store.updatedEnvironment)
	}
}

func TestProjectManagementRejectsDeletingProtectedEnvironment(t *testing.T) {
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	store := &projectManagementStore{
		environment: db.Environment{
			ID:        pgvalue.UUID(environmentID),
			OrgID:     pgvalue.UUID(dbtest.DefaultOrgID),
			ProjectID: pgvalue.UUID(projectID),
			Slug:      "production",
			Name:      "Production",
			ColorHex:  "#315FCE",
			IsDefault: true,
			CreatedAt: testTime(),
			UpdatedAt: testTime(),
		},
	}
	server := &Server{db: store}
	req := httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID.String()+"/environments/"+environmentID.String(), nil)
	req = req.WithContext(context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID: dbtest.DefaultOrgID,
		Role:  auth.RoleOwner,
		Kind:  auth.ActorKindSession,
	}))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", projectID.String())
	routeContext.URLParams.Add("environmentID", environmentID.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	rec := httptest.NewRecorder()

	server.deleteEnvironment(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.deletedEnvironment {
		t.Fatal("protected environment was deleted")
	}
}

func deploymentRequest(body []byte, contentType string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/deployments", bytes.NewReader(body))
	req.Header.Set("content-type", contentType)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", pgvalue.MustUUIDValue(testProjectID()).String())
	routeContext.URLParams.Add("environmentID", pgvalue.MustUUIDValue(testEnvironmentID()).String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{OrgID: dbtest.DefaultOrgID, Role: auth.RoleOwner, Kind: auth.ActorKindSession})
	return req.WithContext(ctx)
}

func currentDeploymentRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/deployments/current", nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", testProjectIDString())
	routeContext.URLParams.Add("environmentID", testEnvironmentIDString())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{OrgID: dbtest.DefaultOrgID, Role: auth.RoleViewer, Kind: auth.ActorKindSession})
	return req.WithContext(ctx)
}

func testScopedArtifact(id pgtype.UUID, kind db.ArtifactKind, digest string, mediaType string) db.Artifact {
	return db.Artifact{
		ID:            id,
		OrgID:         pgvalue.UUID(dbtest.DefaultOrgID),
		ProjectID:     testProjectID(),
		EnvironmentID: testEnvironmentID(),
		Digest:        digest,
		Kind:          kind,
		SizeBytes:     1,
		MediaType:     mediaType,
		CreatedAt:     testTime(),
	}
}

func deploymentStatusRequest(deploymentID pgtype.UUID) *http.Request {
	id := pgvalue.MustUUIDValue(deploymentID)
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/deployments/"+id.String(), nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", testProjectIDString())
	routeContext.URLParams.Add("environmentID", testEnvironmentIDString())
	routeContext.URLParams.Add("deploymentID", id.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{OrgID: dbtest.DefaultOrgID, Role: auth.RoleViewer, Kind: auth.ActorKindSession})
	return req.WithContext(ctx)
}

func promoteDeploymentRequest(deploymentRef string, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/projects/"+testProjectIDString()+"/environments/"+testEnvironmentIDString()+"/deployments/"+deploymentRef+"/promote", strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("projectID", testProjectIDString())
	routeContext.URLParams.Add("environmentID", testEnvironmentIDString())
	routeContext.URLParams.Add("deployment", deploymentRef)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeContext)
	ctx = context.WithValue(ctx, actorContextKey{}, auth.Actor{OrgID: dbtest.DefaultOrgID, UserID: uuid.Must(uuid.NewV7()), Role: auth.RoleOwner, Kind: auth.ActorKindSession})
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
		metadata.ContentHash = sha256sum.DigestBytes(source)
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
	return api.CreateDeploymentRequest{}
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
	objects       map[string]cas.Object
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
	deleteProjectErr     error
	sessionHash          []byte
	session              db.GetAuthSessionByTokenHashRow
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
	if s.deleteProjectErr != nil {
		return db.Project{}, s.deleteProjectErr
	}
	for idx, project := range s.projects {
		if project.OrgID == arg.OrgID && project.ID == arg.ID {
			s.deletedProject = true
			s.projects[idx].IsDefault = false
			return s.projects[idx], nil
		}
	}
	if s.project.ID == (pgtype.UUID{}) || s.project.OrgID != arg.OrgID || s.project.ID != arg.ID {
		return db.Project{}, pgx.ErrNoRows
	}
	s.deletedProject = true
	s.project.IsDefault = false
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
	updated.ColorHex = arg.ColorHex
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

func (s *projectManagementStore) GetAuthSessionByTokenHash(_ context.Context, hash []byte) (db.GetAuthSessionByTokenHashRow, error) {
	if !bytes.Equal(hash, s.sessionHash) {
		return db.GetAuthSessionByTokenHashRow{}, pgx.ErrNoRows
	}
	return s.session, nil
}

func (s *projectManagementStore) RefreshAuthSession(_ context.Context, arg db.RefreshAuthSessionParams) error {
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
	if f.object.Digest == "" {
		f.object.Digest = sha256sum.DigestBytes(content)
	}
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

func (f *fakeCAS) Stat(_ context.Context, digest string) (cas.Object, error) {
	if f.objects != nil {
		object, ok := f.objects[digest]
		if !ok {
			return cas.Object{}, errors.New("object not found")
		}
		return object, nil
	}
	return f.object, nil
}

func (f *fakeCAS) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.body)), nil
}

func (f *fakeCAS) Delete(_ context.Context, digest string) error {
	f.deletedDigest = digest
	return nil
}
