package control

import (
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

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
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
	if store.deployment.DeploymentSourceArtifactID == (pgtype.UUID{}) {
		t.Fatalf("deployment = %+v", store.deployment)
	}
	if len(store.artifacts) != 1 || store.artifacts[0].Digest != artifactStore.object.Digest {
		t.Fatalf("artifacts = %+v", store.artifacts)
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

func TestCreateDeploymentUsesConfiguredScopeWhenScopeIsOmitted(t *testing.T) {
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

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.deployment.ProjectID != testProjectID() || store.deployment.EnvironmentID != testEnvironmentID() {
		t.Fatalf("deployment scope = project %v environment %v", store.deployment.ProjectID, store.deployment.EnvironmentID)
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
	if err := writer.WriteField("project_id", testProjectIDString()); err != nil {
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

func TestCreateDeploymentRejectsUnsupportedVersionMetadata(t *testing.T) {
	for _, tt := range []struct {
		name     string
		metadata api.CreateDeploymentRequest
		want     string
	}{
		{
			name: "bundle format",
			metadata: api.CreateDeploymentRequest{
				ProjectID:           testProjectIDString(),
				BundleFormatVersion: 99,
			},
			want: "unsupported bundle_format_version 99",
		},
		{
			name: "worker protocol",
			metadata: api.CreateDeploymentRequest{
				ProjectID:             testProjectIDString(),
				WorkerProtocolVersion: "helmr.worker.future",
			},
			want: "unsupported worker_protocol_version",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{
				db:  &fakeStore{},
				cas: &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 12, MediaType: api.DeploymentSourceArtifactMediaType}},
				log: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			body, contentType := deploymentMultipart(t, tt.metadata, validDeploymentSourceTar(t))
			req := deploymentRequest(body, contentType)
			rec := httptest.NewRecorder()

			server.createDeployment(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
}

func TestCreateDeploymentReusesDeployedContentHashWithoutPromotion(t *testing.T) {
	digest := "sha256:" + strings.Repeat("9", 64)
	store := &fakeStore{
		createDeploymentResult: &db.Deployment{
			ID:                         testDeploymentID(),
			OrgID:                      ids.ToPG(dbtest.DefaultOrgID),
			ProjectID:                  testProjectID(),
			EnvironmentID:              testEnvironmentID(),
			ContentHash:                digest,
			DeploymentSourceArtifactID: testArtifactID(),
			Status:                     db.DeploymentStatusDeployed,
			CreatedAt:                  testTime(),
			BuildingAt:                 testTime(),
			BuiltAt:                    testTime(),
			DeployedAt:                 testTime(),
		},
		artifacts: []db.Artifact{{
			ID:            testArtifactID(),
			OrgID:         ids.ToPG(dbtest.DefaultOrgID),
			ProjectID:     testProjectID(),
			EnvironmentID: testEnvironmentID(),
			Digest:        digest,
			Kind:          db.ArtifactKindDeploymentSource,
			SizeBytes:     12,
			MediaType:     api.DeploymentSourceArtifactMediaType,
		}},
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
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{
		kind:          auth.ActorKindAPIKey,
		projectID:     testProjectIDString(),
		environmentID: testEnvironmentIDString(),
		permissions:   []auth.Permission{auth.PermissionTasksDeploy},
	}, CAS: &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("c", 64), MediaType: api.DeploymentSourceArtifactMediaType}}},
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
	if store.deployment.DeploymentSourceArtifactID == (pgtype.UUID{}) {
		t.Fatalf("deployment = %+v", store.deployment)
	}
}

func TestDeploymentRouteAuthorizesBeforeReadingDeploymentSource(t *testing.T) {
	store := &fakeStore{}
	artifactStore := &fakeCAS{object: cas.Object{Digest: "sha256:" + strings.Repeat("f", 64), MediaType: api.DeploymentSourceArtifactMediaType}}
	server := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: store, Auth: fakeAuth{
		kind:          auth.ActorKindAPIKey,
		projectID:     testProjectIDString(),
		environmentID: testEnvironmentIDString(),
	}, CAS: artifactStore},
	)
	boundary := "helmr-test-boundary"
	body := strings.Join([]string{
		"--" + boundary,
		`Content-Disposition: form-data; name="metadata"`,
		"",
		`{}`,
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

func TestGetCurrentDeploymentReturnsCatalog(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                         testDeploymentID(),
			OrgID:                      ids.ToPG(dbtest.DefaultOrgID),
			ProjectID:                  testProjectID(),
			EnvironmentID:              testEnvironmentID(),
			DeploymentSourceArtifactID: testArtifactID(),
			Status:                     db.DeploymentStatusDeployed,
			CreatedAt:                  testTime(),
			DeployedAt:                 testTime(),
		},
		deploymentTasks: []db.DeploymentTask{
			{
				ID:               testDeploymentTaskID(),
				OrgID:            ids.ToPG(dbtest.DefaultOrgID),
				ProjectID:        testProjectID(),
				EnvironmentID:    testEnvironmentID(),
				DeploymentID:     testDeploymentID(),
				BundleArtifactID: testArtifactID(),
				TaskID:           "review-pr",
				FilePath:         "tasks/review-pr.ts",
				ExportName:       "reviewPr",
				CreatedAt:        testTime(),
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
			ID:                         testDeploymentID(),
			OrgID:                      ids.ToPG(dbtest.DefaultOrgID),
			ProjectID:                  testProjectID(),
			EnvironmentID:              testEnvironmentID(),
			DeploymentSourceArtifactID: testArtifactID(),
			Status:                     db.DeploymentStatusFailed,
			Failure:                    []byte(`{"message":"build failed"}`),
			CreatedAt:                  testTime(),
			FailedAt:                   testTime(),
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
	if response.DeploymentSource.Digest == "" {
		t.Fatalf("deployment source = %+v", response.DeploymentSource)
	}
	if len(response.Tasks) != 0 {
		t.Fatalf("tasks = %+v", response.Tasks)
	}
}

func TestGetDeploymentAllowsDeployPermission(t *testing.T) {
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                         testDeploymentID(),
			OrgID:                      ids.ToPG(dbtest.DefaultOrgID),
			ProjectID:                  testProjectID(),
			EnvironmentID:              testEnvironmentID(),
			DeploymentSourceArtifactID: testArtifactID(),
			Status:                     db.DeploymentStatusQueued,
			Failure:                    []byte(`{}`),
			CreatedAt:                  testTime(),
		},
	}
	server := &Server{db: store, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := ids.MustFromPG(testDeploymentID())
	req := httptest.NewRequest(http.MethodGet, "/api/deployments/"+id.String(), nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("deploymentID", id.String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeContext))
	ctx := context.WithValue(req.Context(), actorContextKey{}, auth.Actor{
		OrgID:         dbtest.DefaultOrgID,
		Role:          auth.RoleDeveloper,
		Kind:          auth.ActorKindAPIKey,
		ProjectID:     testProjectIDString(),
		EnvironmentID: testEnvironmentIDString(),
		Permissions:   []auth.Permission{auth.PermissionTasksDeploy},
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
	sourceArtifactID := ids.ToPG(ids.New())
	bundleArtifactID := ids.ToPG(ids.New())
	buildManifestArtifactID := ids.ToPG(ids.New())
	deploymentManifestArtifactID := ids.ToPG(ids.New())
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                           testDeploymentID(),
			OrgID:                        ids.ToPG(dbtest.DefaultOrgID),
			ProjectID:                    testProjectID(),
			EnvironmentID:                testEnvironmentID(),
			DeploymentSourceArtifactID:   sourceArtifactID,
			BuildManifestArtifactID:      buildManifestArtifactID,
			DeploymentManifestArtifactID: deploymentManifestArtifactID,
			Status:                       db.DeploymentStatusDeployed,
			CreatedAt:                    testTime(),
			DeployedAt:                   testTime(),
		},
		deploymentTasks: []db.DeploymentTask{
			{
				ID:               testDeploymentTaskID(),
				OrgID:            ids.ToPG(dbtest.DefaultOrgID),
				ProjectID:        testProjectID(),
				EnvironmentID:    testEnvironmentID(),
				DeploymentID:     testDeploymentID(),
				BundleArtifactID: bundleArtifactID,
				TaskID:           "deploy",
				FilePath:         "tasks/deploy.ts",
				ExportName:       "deploy",
				CreatedAt:        testTime(),
			},
		},
		artifacts: []db.Artifact{
			testScopedArtifact(sourceArtifactID, db.ArtifactKindDeploymentSource, "sha256:"+strings.Repeat("b", 64), api.DeploymentSourceArtifactMediaType),
			testScopedArtifact(bundleArtifactID, db.ArtifactKindTaskBundle, "sha256:"+strings.Repeat("c", 64), api.TaskBundleArtifactMediaType),
			testScopedArtifact(buildManifestArtifactID, db.ArtifactKindBuildManifest, "sha256:"+strings.Repeat("d", 64), api.BuildManifestArtifactMediaType),
			testScopedArtifact(deploymentManifestArtifactID, db.ArtifactKindDeploymentManifest, "sha256:"+strings.Repeat("e", 64), api.DeploymentManifestArtifactMediaType),
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
	if response.BuildManifestDigest != "sha256:"+strings.Repeat("d", 64) || response.DeploymentManifestDigest != "sha256:"+strings.Repeat("e", 64) {
		t.Fatalf("manifest digests = %q %q", response.BuildManifestDigest, response.DeploymentManifestDigest)
	}
	if response.Tasks[0].BundleDigest != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("task bundle digest = %q", response.Tasks[0].BundleDigest)
	}
}

func TestPromoteDeploymentResolvesVersionInPathScope(t *testing.T) {
	environmentID := testEnvironmentID()
	store := &fakeStore{
		deployment: db.Deployment{
			ID:                         testDeploymentID(),
			OrgID:                      ids.ToPG(dbtest.DefaultOrgID),
			ProjectID:                  testProjectID(),
			EnvironmentID:              environmentID,
			Version:                    "20260101.2",
			DeploymentSourceArtifactID: testArtifactID(),
			Status:                     db.DeploymentStatusDeployed,
			CreatedAt:                  testTime(),
			DeployedAt:                 testTime(),
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
	body, contentType := deploymentMultipart(t, api.CreateDeploymentRequest{ContentHash: "sha256:" + strings.Repeat("0", 64)}, validDeploymentSourceTar(t))
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

func (f *fakeStore) GetCurrentDeploymentTask(_ context.Context, arg db.GetCurrentDeploymentTaskParams) (db.GetCurrentDeploymentTaskRow, error) {
	f.currentDeploymentTaskCalls++
	if arg.TaskID != "deploy" {
		return db.GetCurrentDeploymentTaskRow{}, pgx.ErrNoRows
	}
	return db.GetCurrentDeploymentTaskRow{
		ID:                     testDeploymentTaskID(),
		OrgID:                  arg.OrgID,
		ProjectID:              arg.ProjectID,
		EnvironmentID:          arg.EnvironmentID,
		DeploymentID:           testDeploymentID(),
		TaskID:                 arg.TaskID,
		FilePath:               "tasks/deploy.ts",
		ExportName:             "deploy",
		SecretDeclarations:     f.currentDeploymentTaskSecretDeclarations,
		MaxDurationSeconds:     300,
		CreatedAt:              testTime(),
		BundleDigest:           "sha256:" + strings.Repeat("b", 64),
		DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
	}, nil
}

func (f *fakeStore) GetCurrentDeployment(_ context.Context, arg db.GetCurrentDeploymentParams) (db.Deployment, error) {
	if f.currentDeploymentMissing {
		return db.Deployment{}, pgx.ErrNoRows
	}
	if f.deployment.ID == (pgtype.UUID{}) {
		if arg.ProjectID != testProjectID() || arg.EnvironmentID != testEnvironmentID() {
			return db.Deployment{}, pgx.ErrNoRows
		}
		return db.Deployment{
			ID:                         testDeploymentID(),
			OrgID:                      arg.OrgID,
			ProjectID:                  arg.ProjectID,
			EnvironmentID:              arg.EnvironmentID,
			Version:                    "20260101.1",
			ContentHash:                "sha256:" + strings.Repeat("a", 64),
			DeploymentSourceArtifactID: testArtifactID(),
			Status:                     db.DeploymentStatusDeployed,
			CreatedAt:                  testTime(),
			DeployedAt:                 testTime(),
		}, nil
	}
	if f.deployment.Status != db.DeploymentStatusDeployed {
		return db.Deployment{}, pgx.ErrNoRows
	}
	if f.deployment.OrgID != arg.OrgID || f.deployment.ProjectID != arg.ProjectID || f.deployment.EnvironmentID != arg.EnvironmentID {
		return db.Deployment{}, pgx.ErrNoRows
	}
	return db.Deployment{
		ID:                           f.deployment.ID,
		OrgID:                        f.deployment.OrgID,
		ProjectID:                    f.deployment.ProjectID,
		EnvironmentID:                f.deployment.EnvironmentID,
		DeploymentSourceArtifactID:   f.deployment.DeploymentSourceArtifactID,
		BuildManifestArtifactID:      f.deployment.BuildManifestArtifactID,
		DeploymentManifestArtifactID: f.deployment.DeploymentManifestArtifactID,
		Status:                       f.deployment.Status,
		Failure:                      f.deployment.Failure,
		CreatedAt:                    f.deployment.CreatedAt,
		BuildingAt:                   f.deployment.BuildingAt,
		BuiltAt:                      f.deployment.BuiltAt,
		DeployedAt:                   f.deployment.DeployedAt,
		FailedAt:                     f.deployment.FailedAt,
	}, nil
}

func (f *fakeStore) ListDeploymentTasks(_ context.Context, arg db.ListDeploymentTasksParams) ([]db.DeploymentTask, error) {
	tasks := make([]db.DeploymentTask, 0, len(f.deploymentTasks))
	for _, task := range f.deploymentTasks {
		if task.OrgID == arg.OrgID && task.ProjectID == arg.ProjectID && task.EnvironmentID == arg.EnvironmentID && task.DeploymentID == arg.DeploymentID {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

func (f *fakeStore) ListDeclarativeScheduleSummariesForEnvironment(context.Context, db.ListDeclarativeScheduleSummariesForEnvironmentParams) ([]db.ListDeclarativeScheduleSummariesForEnvironmentRow, error) {
	return nil, nil
}

func (f *fakeStore) ScheduleInstanceTriggerIsCurrent(context.Context, db.ScheduleInstanceTriggerIsCurrentParams) (bool, error) {
	return true, nil
}

func (f *fakeStore) GetEnvironment(_ context.Context, arg db.GetEnvironmentParams) (db.Environment, error) {
	if arg.ProjectID != testProjectID() || arg.ID != testEnvironmentID() {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		ProjectID: arg.ProjectID,
		Slug:      "prod",
		Name:      "Production",
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetProject(_ context.Context, arg db.GetProjectParams) (db.Project, error) {
	if arg.ID != testProjectID() {
		return db.Project{}, pgx.ErrNoRows
	}
	return db.Project{
		ID:        arg.ID,
		OrgID:     arg.OrgID,
		Slug:      "main",
		Name:      "Main",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetProjectBySlug(_ context.Context, arg db.GetProjectBySlugParams) (db.Project, error) {
	if arg.Slug != "main" {
		return db.Project{}, pgx.ErrNoRows
	}
	return db.Project{
		ID:        testProjectID(),
		OrgID:     arg.OrgID,
		Slug:      arg.Slug,
		Name:      "Main",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetDefaultEnvironment(_ context.Context, arg db.GetDefaultEnvironmentParams) (db.Environment, error) {
	if arg.ProjectID != testProjectID() {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        testEnvironmentID(),
		OrgID:     arg.OrgID,
		ProjectID: arg.ProjectID,
		Slug:      "production",
		Name:      "Production",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) GetEnvironmentBySlug(_ context.Context, arg db.GetEnvironmentBySlugParams) (db.Environment, error) {
	if arg.ProjectID != testProjectID() || arg.Slug != "production" {
		return db.Environment{}, pgx.ErrNoRows
	}
	return db.Environment{
		ID:        testEnvironmentID(),
		OrgID:     arg.OrgID,
		ProjectID: arg.ProjectID,
		Slug:      arg.Slug,
		Name:      "Production",
		IsDefault: true,
		CreatedAt: testTime(),
		UpdatedAt: testTime(),
	}, nil
}

func (f *fakeStore) CreateDeployment(_ context.Context, arg db.CreateDeploymentParams) (db.Deployment, error) {
	if f.createDeploymentErr != nil {
		return db.Deployment{}, f.createDeploymentErr
	}
	if f.createDeploymentResult != nil {
		f.deployment = *f.createDeploymentResult
		if f.deployment.Version == "" {
			f.deployment.Version = arg.Version
		}
		if f.deployment.ApiVersion == "" {
			f.deployment.ApiVersion = arg.ApiVersion
		}
		if f.deployment.BundleFormatVersion == 0 {
			f.deployment.BundleFormatVersion = arg.BundleFormatVersion
		}
		if f.deployment.WorkerProtocolVersion == "" {
			f.deployment.WorkerProtocolVersion = arg.WorkerProtocolVersion
		}
		if !f.deployment.WorkerGroupID.Valid {
			f.deployment.WorkerGroupID = arg.WorkerGroupID
		}
		return f.deployment, nil
	}
	f.deployment = db.Deployment{
		ID:                         arg.ID,
		OrgID:                      arg.OrgID,
		ProjectID:                  arg.ProjectID,
		EnvironmentID:              arg.EnvironmentID,
		Version:                    arg.Version,
		ApiVersion:                 arg.ApiVersion,
		SdkVersion:                 arg.SdkVersion,
		CliVersion:                 arg.CliVersion,
		BundleFormatVersion:        arg.BundleFormatVersion,
		WorkerProtocolVersion:      arg.WorkerProtocolVersion,
		WorkerGroupID:              arg.WorkerGroupID,
		ContentHash:                arg.ContentHash,
		DeploymentSourceArtifactID: arg.DeploymentSourceArtifactID,
		Status:                     arg.Status,
		CreatedAt:                  testTime(),
		DeployedAt:                 testTime(),
	}
	return f.deployment, nil
}

func (f *fakeStore) CreateArtifact(_ context.Context, arg db.CreateArtifactParams) (db.Artifact, error) {
	artifact := db.Artifact{
		ID:                        arg.ID,
		OrgID:                     arg.OrgID,
		ProjectID:                 arg.ProjectID,
		EnvironmentID:             arg.EnvironmentID,
		Digest:                    arg.Digest,
		Kind:                      arg.Kind,
		SizeBytes:                 arg.SizeBytes,
		MediaType:                 arg.MediaType,
		CreatedByWorkerInstanceID: arg.CreatedByWorkerInstanceID,
		CreatedAt:                 testTime(),
	}
	f.artifacts = append(f.artifacts, artifact)
	return artifact, nil
}

func (f *fakeStore) GetArtifact(_ context.Context, arg db.GetArtifactParams) (db.Artifact, error) {
	for _, artifact := range f.artifacts {
		if artifact.OrgID == arg.OrgID && artifact.ProjectID == arg.ProjectID && artifact.EnvironmentID == arg.EnvironmentID && artifact.ID == arg.ID {
			return artifact, nil
		}
	}
	if arg.ID == testArtifactID() {
		return db.Artifact{
			ID:            arg.ID,
			OrgID:         arg.OrgID,
			ProjectID:     arg.ProjectID,
			EnvironmentID: arg.EnvironmentID,
			Digest:        "sha256:" + strings.Repeat("a", 64),
			Kind:          db.ArtifactKindDeploymentSource,
			SizeBytes:     12,
			MediaType:     api.DeploymentSourceArtifactMediaType,
			CreatedAt:     testTime(),
		}, nil
	}
	return db.Artifact{}, pgx.ErrNoRows
}

func (f *fakeStore) ListArtifactsByIDs(ctx context.Context, arg db.ListArtifactsByIDsParams) ([]db.Artifact, error) {
	artifacts := make([]db.Artifact, 0, len(arg.Ids))
	for _, artifactID := range arg.Ids {
		artifact, err := f.GetArtifact(ctx, db.GetArtifactParams{
			OrgID:         arg.OrgID,
			ProjectID:     arg.ProjectID,
			EnvironmentID: arg.EnvironmentID,
			ID:            artifactID,
		})
		if isNoRows(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}

func (f *fakeStore) LockDeploymentReusableBuildKey(_ context.Context, _ db.LockDeploymentReusableBuildKeyParams) error {
	return nil
}

func (f *fakeStore) AllocateDeploymentVersion(_ context.Context, _ db.AllocateDeploymentVersionParams) (string, error) {
	return "20260101.1", nil
}

func (f *fakeStore) GetDefaultWorkerGroup(context.Context) (db.WorkerGroup, error) {
	return db.WorkerGroup{
		ID:          testWorkerGroupID(),
		Name:        "default",
		Description: "Default worker group",
		CreatedAt:   testTime(),
		UpdatedAt:   testTime(),
	}, nil
}

func (f *fakeStore) GetReusableDeploymentByContentHash(_ context.Context, arg db.GetReusableDeploymentByContentHashParams) (db.Deployment, error) {
	if f.deployment.OrgID == arg.OrgID && f.deployment.ProjectID == arg.ProjectID && f.deployment.EnvironmentID == arg.EnvironmentID && f.deployment.ContentHash == arg.ContentHash && f.deployment.WorkerGroupID == arg.WorkerGroupID {
		return f.deployment, nil
	}
	return db.Deployment{}, pgx.ErrNoRows
}

func (f *fakeStore) PromoteDeployment(_ context.Context, arg db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error) {
	f.deploymentPromotions = append(f.deploymentPromotions, arg)
	return db.PromoteDeploymentRow{
		ID:                  arg.ID,
		OrgID:               arg.OrgID,
		ProjectID:           arg.ProjectID,
		EnvironmentID:       arg.EnvironmentID,
		DeploymentID:        arg.DeploymentID,
		PromotedByPrincipal: arg.PromotedByPrincipal,
		Reason:              arg.Reason,
		CreatedAt:           testTime(),
	}, nil
}

func (f *fakeStore) GetDeploymentByVersion(_ context.Context, arg db.GetDeploymentByVersionParams) (db.Deployment, error) {
	if f.deployment.Version == arg.Version && f.deployment.OrgID == arg.OrgID && f.deployment.ProjectID == arg.ProjectID && f.deployment.EnvironmentID == arg.EnvironmentID {
		return f.deployment, nil
	}
	return db.Deployment{}, pgx.ErrNoRows
}

func (f *fakeStore) GetDeploymentForOrg(_ context.Context, arg db.GetDeploymentForOrgParams) (db.Deployment, error) {
	if f.deployment.ID == arg.ID && f.deployment.OrgID == arg.OrgID {
		return f.deployment, nil
	}
	return db.Deployment{}, pgx.ErrNoRows
}

func (f *fakeStore) ListDeploymentsByVersionForOrg(_ context.Context, arg db.ListDeploymentsByVersionForOrgParams) ([]db.Deployment, error) {
	if f.deployment.Version == arg.Version && f.deployment.OrgID == arg.OrgID {
		return []db.Deployment{f.deployment}, nil
	}
	return nil, nil
}

func (f *fakeStore) GetDeploymentTask(_ context.Context, arg db.GetDeploymentTaskParams) (db.GetDeploymentTaskRow, error) {
	f.getDeploymentTaskCalls++
	if len(f.deploymentTasks) == 0 && arg.TaskID == "deploy" && arg.DeploymentID == testDeploymentID() {
		return db.GetDeploymentTaskRow{
			ID:                     testDeploymentTaskID(),
			OrgID:                  arg.OrgID,
			ProjectID:              arg.ProjectID,
			EnvironmentID:          arg.EnvironmentID,
			DeploymentID:           arg.DeploymentID,
			TaskID:                 arg.TaskID,
			FilePath:               "tasks/deploy.ts",
			ExportName:             "deploy",
			MaxDurationSeconds:     300,
			CreatedAt:              testTime(),
			DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
		}, nil
	}
	for _, task := range f.deploymentTasks {
		if task.OrgID == arg.OrgID && task.ProjectID == arg.ProjectID && task.EnvironmentID == arg.EnvironmentID && task.DeploymentID == arg.DeploymentID && task.TaskID == arg.TaskID {
			return db.GetDeploymentTaskRow{
				ID:                     task.ID,
				OrgID:                  task.OrgID,
				ProjectID:              task.ProjectID,
				EnvironmentID:          task.EnvironmentID,
				DeploymentID:           task.DeploymentID,
				TaskID:                 task.TaskID,
				FilePath:               task.FilePath,
				ExportName:             task.ExportName,
				HandlerEntrypoint:      task.HandlerEntrypoint,
				BundleArtifactID:       task.BundleArtifactID,
				BundleDigest:           "sha256:" + strings.Repeat("b", 64),
				RequestedMilliCpu:      task.RequestedMilliCpu,
				RequestedMemoryMib:     task.RequestedMemoryMib,
				SecretDeclarations:     task.SecretDeclarations,
				ResourceRequirements:   task.ResourceRequirements,
				QueueName:              task.QueueName,
				QueueConcurrencyLimit:  task.QueueConcurrencyLimit,
				Ttl:                    task.Ttl,
				MaxDurationSeconds:     task.MaxDurationSeconds,
				CreatedAt:              task.CreatedAt,
				DeploymentSourceDigest: "sha256:" + strings.Repeat("a", 64),
			}, nil
		}
	}
	return db.GetDeploymentTaskRow{}, pgx.ErrNoRows
}

func (f *fakeStore) GetDeploymentQueueConfig(_ context.Context, arg db.GetDeploymentQueueConfigParams) (db.GetDeploymentQueueConfigRow, error) {
	for _, task := range f.deploymentTasks {
		if task.OrgID == arg.OrgID && task.ProjectID == arg.ProjectID && task.EnvironmentID == arg.EnvironmentID && task.DeploymentID == arg.DeploymentID && task.QueueName == arg.QueueName {
			return db.GetDeploymentQueueConfigRow{
				QueueName:             task.QueueName,
				QueueConcurrencyLimit: task.QueueConcurrencyLimit,
			}, nil
		}
	}
	return db.GetDeploymentQueueConfigRow{}, pgx.ErrNoRows
}

func (f *fakeStore) CreateDeploymentTask(_ context.Context, arg db.CreateDeploymentTaskParams) (db.DeploymentTask, error) {
	task := db.DeploymentTask{
		ID:                    arg.ID,
		OrgID:                 arg.OrgID,
		ProjectID:             arg.ProjectID,
		EnvironmentID:         arg.EnvironmentID,
		DeploymentID:          arg.DeploymentID,
		TaskID:                arg.TaskID,
		FilePath:              arg.FilePath,
		ExportName:            arg.ExportName,
		HandlerEntrypoint:     arg.HandlerEntrypoint,
		BundleArtifactID:      arg.BundleArtifactID,
		BundleFormatVersion:   arg.BundleFormatVersion,
		RequestedMilliCpu:     arg.RequestedMilliCpu,
		RequestedMemoryMib:    arg.RequestedMemoryMib,
		SecretDeclarations:    arg.SecretDeclarations,
		ResourceRequirements:  arg.ResourceRequirements,
		QueueName:             arg.QueueName,
		QueueConcurrencyLimit: arg.QueueConcurrencyLimit,
		Ttl:                   arg.Ttl,
		MaxDurationSeconds:    arg.MaxDurationSeconds,
		CreatedAt:             testTime(),
	}
	f.deploymentTasks = append(f.deploymentTasks, task)
	return task, nil
}

func (f *fakeStore) AppendDeploymentEvent(_ context.Context, arg db.AppendDeploymentEventParams) (db.AppendDeploymentEventRow, error) {
	event := db.Event{
		Seq:             int64(len(f.deploymentEvents) + 1),
		OrgID:           arg.OrgID,
		ProjectID:       arg.ProjectID,
		EnvironmentID:   arg.EnvironmentID,
		DeploymentID:    arg.DeploymentID,
		Category:        arg.Category,
		Severity:        arg.Severity,
		Source:          arg.Source,
		Kind:            arg.Kind,
		Message:         arg.Message,
		Payload:         arg.Payload,
		RedactionClass:  arg.RedactionClass,
		CreatedAt:       testTime(),
		OccurredAt:      testTime(),
		SnapshotVersion: pgtype.Int8{},
	}
	f.deploymentEvents = append(f.deploymentEvents, event)
	return db.AppendDeploymentEventRow{
		Seq:             event.Seq,
		OrgID:           event.OrgID,
		ProjectID:       event.ProjectID,
		EnvironmentID:   event.EnvironmentID,
		DeploymentID:    event.DeploymentID,
		Category:        event.Category,
		Severity:        event.Severity,
		Source:          event.Source,
		Kind:            event.Kind,
		Message:         event.Message,
		Payload:         event.Payload,
		RedactionClass:  event.RedactionClass,
		CreatedAt:       event.CreatedAt,
		OccurredAt:      event.OccurredAt,
		SnapshotVersion: event.SnapshotVersion,
	}, nil
}

func fakeRunDeploymentID(run db.Run) pgtype.UUID {
	if run.DeploymentID.Valid {
		return run.DeploymentID
	}
	return testDeploymentID()
}

func fakeRunDeploymentTaskID(run db.Run) pgtype.UUID {
	if run.DeploymentTaskID.Valid {
		return run.DeploymentTaskID
	}
	return testDeploymentTaskID()
}
