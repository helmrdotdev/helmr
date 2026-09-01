package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDeleteProjectPostgresMaintainsDefaultInvariant(t *testing.T) {
	t.Run("non-default", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 3, 0)
		server := projectDeletionServer(fixture.pool)

		response := deleteProjectRecorder(t, server, fixture.orgID, fixture.projectIDs[2])
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		assertProjectDefaults(t, fixture.pool, fixture.orgID, 2, "project-0001")
	})

	t.Run("default promotes deterministic successor", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 3, 0)
		server := projectDeletionServer(fixture.pool)

		response := deleteProjectRecorder(t, server, fixture.orgID, fixture.projectIDs[0])
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		assertProjectDefaults(t, fixture.pool, fixture.orgID, 2, "project-0002")
	})

	t.Run("last project", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 1, 0)
		server := projectDeletionServer(fixture.pool)

		response := deleteProjectRecorder(t, server, fixture.orgID, fixture.projectIDs[0])
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		assertProjectDefaults(t, fixture.pool, fixture.orgID, 0, "")
	})

	t.Run("missing and cross-organization targets are hidden", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 1, 0)
		server := projectDeletionServer(fixture.pool)
		otherOrg := createDeletionTestOrganization(t, fixture.pool, "project-delete-other")

		for _, test := range []struct {
			name      string
			orgID     uuid.UUID
			projectID uuid.UUID
		}{
			{name: "missing", orgID: fixture.orgID, projectID: uuid.NewV7()},
			{name: "cross organization", orgID: otherOrg, projectID: fixture.projectIDs[0]},
		} {
			t.Run(test.name, func(t *testing.T) {
				response := deleteProjectRecorder(t, server, test.orgID, test.projectID)
				if response.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want 404: %s", response.Code, response.Body.String())
				}
			})
		}
		assertProjectDefaults(t, fixture.pool, fixture.orgID, 1, "project-0001")
	})
}

func TestDeleteProjectPostgresRollsBackOnRestrictedDescendant(t *testing.T) {
	fixture := newProjectListPostgresFixture(t, 1, 1)
	queries := db.New(fixture.pool)
	environmentID := environmentIDForProject(t, fixture.pool, fixture.projectIDs[0])
	secretID := uuid.NewV7()
	if _, err := queries.CreateSecret(t.Context(), db.CreateSecretParams{
		ID: pgvalue.UUID(secretID), EnvironmentID: pgvalue.UUID(environmentID), Name: "restricted",
		VersionID: pgvalue.UUID(uuid.NewV7()), Nonce: bytes.Repeat([]byte{1}, 12), Ciphertext: bytes.Repeat([]byte{2}, 16),
	}); err != nil {
		t.Fatal(err)
	}

	response := deleteProjectRecorder(t, projectDeletionServer(fixture.pool), fixture.orgID, fixture.projectIDs[0])
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", response.Code, response.Body.String())
	}
	assertProjectDefaults(t, fixture.pool, fixture.orgID, 1, "project-0001")
	var secretCount int
	if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM secrets WHERE id = $1`, secretID).Scan(&secretCount); err != nil {
		t.Fatal(err)
	}
	if secretCount != 1 {
		t.Fatalf("secret count = %d, want 1", secretCount)
	}
}

func TestDeleteProjectPostgresCascadesOnlyTargetScope(t *testing.T) {
	fixture := newProjectListPostgresFixture(t, 2, 1)
	server := projectDeletionServer(fixture.pool)
	targetEnvironmentID := environmentIDForProject(t, fixture.pool, fixture.projectIDs[1])
	siblingEnvironmentID := environmentIDForProject(t, fixture.pool, fixture.projectIDs[0])

	response := deleteProjectRecorder(t, server, fixture.orgID, fixture.projectIDs[1])
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	for _, test := range []struct {
		id   uuid.UUID
		want int
	}{{targetEnvironmentID, 0}, {siblingEnvironmentID, 1}} {
		var count int
		if err := fixture.pool.QueryRow(t.Context(), `SELECT count(*) FROM environments WHERE id = $1`, test.id).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != test.want {
			t.Fatalf("environment %s count = %d, want %d", test.id, count, test.want)
		}
	}
}

func TestDeleteProjectPostgresConcurrentRequestsPreserveDefaultInvariant(t *testing.T) {
	t.Run("same target", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 2, 0)
		server := projectDeletionServer(fixture.pool)
		codes := runConcurrentDeletes(t, server, fixture.orgID, fixture.projectIDs[1], fixture.projectIDs[1])
		if codes[0] != http.StatusNoContent || codes[1] != http.StatusNotFound {
			t.Fatalf("statuses = %v, want [204 404]", codes)
		}
		assertProjectDefaults(t, fixture.pool, fixture.orgID, 1, "project-0001")
	})

	t.Run("default and non-default", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 3, 0)
		server := projectDeletionServer(fixture.pool)
		codes := runConcurrentDeletes(t, server, fixture.orgID, fixture.projectIDs[0], fixture.projectIDs[2])
		if codes[0] != http.StatusNoContent || codes[1] != http.StatusNoContent {
			t.Fatalf("statuses = %v, want [204 204]", codes)
		}
		assertProjectDefaults(t, fixture.pool, fixture.orgID, 1, "project-0002")
	})
}

func TestCreateAndDeleteProjectPostgresSerializeDefaultSelection(t *testing.T) {
	fixture := newProjectListPostgresFixture(t, 1, 0)
	server := projectDeletionServer(fixture.pool)
	start := make(chan struct{})
	responses := make(chan int, 2)
	var ready sync.WaitGroup
	ready.Add(2)

	go func() {
		ready.Done()
		<-start
		responses <- deleteProjectRecorder(t, server, fixture.orgID, fixture.projectIDs[0]).Code
	}()
	go func() {
		ready.Done()
		<-start
		request := httptest.NewRequest(http.MethodPost, "/api/projects", bytes.NewBufferString(`{"slug":"replacement","name":"Replacement"}`))
		request.Header.Set("Content-Type", "application/json")
		request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, ownerActor(fixture.orgID)))
		response := httptest.NewRecorder()
		server.createProject(response, request)
		responses <- response.Code
	}()
	ready.Wait()
	close(start)
	codes := []int{<-responses, <-responses}
	sort.Ints(codes)
	if codes[0] != http.StatusCreated || codes[1] != http.StatusNoContent {
		t.Fatalf("statuses = %v, want [201 204]", codes)
	}
	assertProjectDefaults(t, fixture.pool, fixture.orgID, 1, "replacement")
}

func TestDeleteProjectPostgresPromotionFailureRollsBack(t *testing.T) {
	fixture := newProjectListPostgresFixture(t, 2, 0)
	store := &projectDeletionStore{Querier: db.New(fixture.pool), pool: fixture.pool, failPromotion: true}
	server := &Server{db: store}

	response := deleteProjectRecorder(t, server, fixture.orgID, fixture.projectIDs[0])
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", response.Code, response.Body.String())
	}
	assertProjectDefaults(t, fixture.pool, fixture.orgID, 2, "project-0001")
}

func TestDeleteProjectPostgresDoesNotLockUnrelatedProjectRows(t *testing.T) {
	fixture := newProjectListPostgresFixture(t, 4, 0)
	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	queries := db.New(tx)
	orgID := pgvalue.UUID(fixture.orgID)
	if _, err := queries.LockOrganizationForProjectDefaults(t.Context(), orgID); err != nil {
		t.Fatal(err)
	}
	deleted, err := queries.DeleteProject(t.Context(), db.DeleteProjectParams{OrgID: orgID, ID: pgvalue.UUID(fixture.projectIDs[0])})
	if err != nil || !deleted.IsDefault {
		t.Fatalf("delete default = %+v, %v", deleted, err)
	}
	if rows, err := queries.PromoteFirstProjectDefault(t.Context(), orgID); err != nil || rows != 1 {
		t.Fatalf("promote rows/error = %d/%v", rows, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := fixture.pool.Exec(ctx, `UPDATE projects SET name = 'Unrelated update' WHERE id = $1`, fixture.projectIDs[3]); err != nil {
		t.Fatalf("unrelated project update blocked by delete transaction: %v", err)
	}
	if _, err := db.New(fixture.pool).CreateEnvironment(ctx, db.CreateEnvironmentParams{
		ID: pgvalue.UUID(uuid.NewV7()), OrgID: orgID, ProjectID: pgvalue.UUID(fixture.projectIDs[3]),
		Slug: "unrelated", Name: "Unrelated", ColorHex: "#315FCE", IsDefault: false,
	}); err != nil {
		t.Fatalf("unrelated environment insert blocked by delete transaction: %v", err)
	}
}

func TestDeleteProjectPostgresCandidateScaleUsesConstantBusinessStatements(t *testing.T) {
	if os.Getenv("HELMR_TEST_PROJECT_DELETION_SCALE") != "1" {
		t.Skip("HELMR_TEST_PROJECT_DELETION_SCALE is not set")
	}
	fixture := newProjectListPostgresFixture(t, 1000, 0)
	store := &projectDeletionStore{Querier: db.New(fixture.pool), pool: fixture.pool}
	server := &Server{db: store}

	started := time.Now()
	response := deleteProjectRecorder(t, server, fixture.orgID, fixture.projectIDs[0])
	elapsed := time.Since(started)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	if got := store.statements.Load(); got != 3 {
		t.Fatalf("transaction business statements = %d, want 3", got)
	}
	assertProjectDefaults(t, fixture.pool, fixture.orgID, 999, "project-0002")
	t.Logf("project delete candidate: projects=1000 statements=%d elapsed=%s", store.statements.Load(), elapsed)
}

func TestDeleteEnvironmentPostgresBehavior(t *testing.T) {
	t.Run("ordinary environment", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 1, 0)
		environmentID := createDeletionTestEnvironment(t, fixture.pool, fixture.orgID, fixture.projectIDs[0], "preview")
		response := deleteEnvironmentRecorder(t, projectDeletionServer(fixture.pool), fixture.orgID, fixture.projectIDs[0], environmentID)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
	})

	t.Run("protected environment", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 1, 0)
		environmentID := createDeletionTestEnvironment(t, fixture.pool, fixture.orgID, fixture.projectIDs[0], "production")
		response := deleteEnvironmentRecorder(t, projectDeletionServer(fixture.pool), fixture.orgID, fixture.projectIDs[0], environmentID)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
		}
	})

	t.Run("missing and cross-organization targets are hidden", func(t *testing.T) {
		fixture := newProjectListPostgresFixture(t, 1, 0)
		environmentID := createDeletionTestEnvironment(t, fixture.pool, fixture.orgID, fixture.projectIDs[0], "preview")
		otherOrg := createDeletionTestOrganization(t, fixture.pool, "environment-delete-other")
		server := projectDeletionServer(fixture.pool)
		for _, test := range []struct {
			orgID         uuid.UUID
			environmentID uuid.UUID
		}{{fixture.orgID, uuid.NewV7()}, {otherOrg, environmentID}} {
			response := deleteEnvironmentRecorder(t, server, test.orgID, fixture.projectIDs[0], test.environmentID)
			if response.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", response.Code, response.Body.String())
			}
		}
	})
}

func TestDeleteRoutesRejectViewerSession(t *testing.T) {
	fixture := newProjectListPostgresFixture(t, 1, 0)
	environmentID := createDeletionTestEnvironment(t, fixture.pool, fixture.orgID, fixture.projectIDs[0], "preview")
	keys, err := auth.NewKeys(bytes.Repeat([]byte{3}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	store := &projectDeletionViewerStore{Querier: db.New(fixture.pool), orgID: fixture.orgID}
	publicURL, err := url.Parse("https://helmr.example.test")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{db: store, tx: fixture.pool, authKeys: keys, publicURL: publicURL}
	router := chi.NewRouter()
	server.mountRoutes(router)

	for _, path := range []string{
		"/api/projects/" + fixture.projectIDs[0].String(),
		fmt.Sprintf("/api/projects/%s/environments/%s", fixture.projectIDs[0], environmentID),
	} {
		request := httptest.NewRequest(http.MethodDelete, path, nil)
		request.Header.Set("Authorization", "Bearer viewer-session-token-with-more-than-forty-characters")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403: %s", path, response.Code, response.Body.String())
		}
	}
	assertProjectDefaults(t, fixture.pool, fixture.orgID, 1, "project-0001")
}

func projectDeletionServer(pool *pgxpool.Pool) *Server {
	return &Server{db: db.New(pool), tx: pool}
}

func ownerActor(orgID uuid.UUID) auth.Actor {
	return auth.Actor{OrgID: orgID, Kind: auth.ActorKindSession, Role: auth.RoleOwner}
}

func deleteProjectRecorder(t *testing.T, server *Server, orgID, projectID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Delete("/api/projects/{projectID}", server.deleteProject)
	request := httptest.NewRequest(http.MethodDelete, "/api/projects/"+projectID.String(), nil)
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, ownerActor(orgID)))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func deleteEnvironmentRecorder(t *testing.T, server *Server, orgID, projectID, environmentID uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Delete("/api/projects/{projectID}/environments/{environmentID}", server.deleteEnvironment)
	request := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/projects/%s/environments/%s", projectID, environmentID), nil)
	request = request.WithContext(context.WithValue(request.Context(), actorContextKey{}, ownerActor(orgID)))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func runConcurrentDeletes(t *testing.T, server *Server, orgID uuid.UUID, projectIDs ...uuid.UUID) []int {
	t.Helper()
	start := make(chan struct{})
	responses := make(chan int, len(projectIDs))
	var ready sync.WaitGroup
	ready.Add(len(projectIDs))
	for _, projectID := range projectIDs {
		go func() {
			ready.Done()
			<-start
			responses <- deleteProjectRecorder(t, server, orgID, projectID).Code
		}()
	}
	ready.Wait()
	close(start)
	codes := make([]int, 0, len(projectIDs))
	for range projectIDs {
		codes = append(codes, <-responses)
	}
	sort.Ints(codes)
	return codes
}

func assertProjectDefaults(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, wantCount int, wantDefaultSlug string) {
	t.Helper()
	var projectCount, defaultCount int
	var defaultSlug *string
	if err := pool.QueryRow(t.Context(), `
		SELECT count(*), count(*) FILTER (WHERE is_default), min(slug) FILTER (WHERE is_default)
		  FROM projects
		 WHERE org_id = $1
	`, orgID).Scan(&projectCount, &defaultCount, &defaultSlug); err != nil {
		t.Fatal(err)
	}
	wantDefaultCount := 0
	if wantCount > 0 {
		wantDefaultCount = 1
	}
	if projectCount != wantCount || defaultCount != wantDefaultCount {
		t.Fatalf("projects/defaults = %d/%d, want %d/%d", projectCount, defaultCount, wantCount, wantDefaultCount)
	}
	if wantDefaultSlug == "" {
		if defaultSlug != nil {
			t.Fatalf("default slug = %q, want NULL", *defaultSlug)
		}
	} else if defaultSlug == nil || *defaultSlug != wantDefaultSlug {
		t.Fatalf("default slug = %v, want %q", defaultSlug, wantDefaultSlug)
	}
}

func createDeletionTestOrganization(t *testing.T, pool *pgxpool.Pool, slugPrefix string) uuid.UUID {
	t.Helper()
	orgID := uuid.NewV7()
	if _, err := db.New(pool).CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: slugPrefix, Slug: slugPrefix + "-" + orgID.String(),
	}); err != nil {
		t.Fatal(err)
	}
	return orgID
}

func createDeletionTestEnvironment(t *testing.T, pool *pgxpool.Pool, orgID, projectID uuid.UUID, slug string) uuid.UUID {
	t.Helper()
	environmentID := uuid.NewV7()
	if _, err := db.New(pool).CreateEnvironment(t.Context(), db.CreateEnvironmentParams{
		ID: pgvalue.UUID(environmentID), OrgID: pgvalue.UUID(orgID), ProjectID: pgvalue.UUID(projectID),
		Slug: slug, Name: slug, ColorHex: "#315FCE", IsDefault: false,
	}); err != nil {
		t.Fatal(err)
	}
	return environmentID
}

func environmentIDForProject(t *testing.T, pool *pgxpool.Pool, projectID uuid.UUID) uuid.UUID {
	t.Helper()
	var environmentID uuid.UUID
	if err := pool.QueryRow(t.Context(), `SELECT id FROM environments WHERE project_id = $1 ORDER BY id LIMIT 1`, projectID).Scan(&environmentID); err != nil {
		t.Fatal(err)
	}
	return environmentID
}

type projectDeletionStore struct {
	db.Querier
	pool          *pgxpool.Pool
	failPromotion bool
	statements    atomic.Int64
}

type projectDeletionViewerStore struct {
	db.Querier
	orgID uuid.UUID
}

func (s *projectDeletionViewerStore) GetAuthSessionByTokenHash(context.Context, []byte) (db.GetAuthSessionByTokenHashRow, error) {
	return db.GetAuthSessionByTokenHashRow{
		ID: pgvalue.UUID(uuid.NewV7()), UserID: pgvalue.UUID(uuid.NewV7()),
		OrgID: pgvalue.UUID(s.orgID), Role: string(auth.RoleViewer),
	}, nil
}

func (s *projectDeletionViewerStore) RefreshAuthSession(context.Context, db.RefreshAuthSessionParams) error {
	return nil
}

func (s *projectDeletionStore) BeginQuerier(ctx context.Context) (db.Querier, transaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	return &projectDeletionTxQueries{
		Querier: db.New(tx), failPromotion: s.failPromotion, statements: &s.statements,
	}, tx, nil
}

type projectDeletionTxQueries struct {
	db.Querier
	failPromotion bool
	statements    *atomic.Int64
}

func (q *projectDeletionTxQueries) LockOrganizationForProjectDefaults(ctx context.Context, id pgtype.UUID) (pgtype.UUID, error) {
	q.statements.Add(1)
	return q.Querier.LockOrganizationForProjectDefaults(ctx, id)
}

func (q *projectDeletionTxQueries) DeleteProject(ctx context.Context, arg db.DeleteProjectParams) (db.Project, error) {
	q.statements.Add(1)
	return q.Querier.DeleteProject(ctx, arg)
}

func (q *projectDeletionTxQueries) PromoteFirstProjectDefault(ctx context.Context, orgID pgtype.UUID) (int64, error) {
	q.statements.Add(1)
	if q.failPromotion {
		return 0, errors.New("forced promotion failure")
	}
	return q.Querier.PromoteFirstProjectDefault(ctx, orgID)
}

var _ queryTransactionBeginner = (*projectDeletionStore)(nil)
var _ db.Querier = (*projectDeletionTxQueries)(nil)
