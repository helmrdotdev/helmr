package controlplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRegisterFinalizedDeploymentBundlePostgresRollsBackEveryAuthorityRow(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	prepared := fixture.prepared
	prepared.definitions = []finalizedDeploymentDefinition{{
		kind: "invalid", declaredID: "invalid", manifest: []byte(`{}`), manifestDigest: bytes.Repeat([]byte{1}, 32),
	}}
	request := fixture.idempotencyRequest(t, "rollback")
	if _, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, prepared, request,
	); err == nil {
		t.Fatal("registration with an invalid definition succeeded")
	}
	for table, query := range map[string]string{
		"CAS ownership":      `SELECT count(*) FROM cas_objects WHERE org_id = $1`,
		"artifacts":          `SELECT count(*) FROM artifacts WHERE environment_id = $1`,
		"deployments":        `SELECT count(*) FROM deployments WHERE environment_id = $1`,
		"definitions":        `SELECT count(*) FROM deployment_definitions WHERE environment_id = $1`,
		"idempotency claims": `SELECT count(*) FROM idempotency_claims WHERE environment_id = $1`,
	} {
		argument := any(fixture.environmentID)
		if table == "CAS ownership" {
			argument = fixture.orgID
		}
		var count int
		if err := fixture.pool.QueryRow(t.Context(), query, argument).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0 after rollback", table, count)
		}
	}
}

func TestRegisterFinalizedDeploymentBundlePostgresConvergesConcurrentExactRequests(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	responses := make([]string, 2)
	errors := make([]error, 2)
	requests := []idempotency.Request{
		fixture.idempotencyRequest(t, "concurrent"),
		fixture.idempotencyRequest(t, "concurrent"),
	}
	var wait sync.WaitGroup
	for index := range responses {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response, err := fixture.server.registerFinalizedDeploymentBundle(
				t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
				requests[index],
			)
			responses[index], errors[index] = response.ID, err
		}(index)
	}
	wait.Wait()
	for _, err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if responses[0] == "" || responses[0] != responses[1] {
		t.Fatalf("deployment IDs = %v", responses)
	}
	for name, query := range map[string]string{
		"deployment":        `SELECT count(*) FROM deployments WHERE environment_id = $1`,
		"program artifact":  `SELECT count(*) FROM artifacts WHERE environment_id = $1`,
		"idempotency claim": `SELECT count(*) FROM idempotency_claims WHERE environment_id = $1 AND state = 'completed'`,
	} {
		var count int
		if err := fixture.pool.QueryRow(t.Context(), query, fixture.environmentID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s count = %d, want 1", name, count)
		}
	}
	var ownership int
	if err := fixture.pool.QueryRow(
		t.Context(), `SELECT count(*) FROM cas_objects WHERE org_id = $1`, fixture.orgID,
	).Scan(&ownership); err != nil {
		t.Fatal(err)
	}
	if ownership != 2 {
		t.Fatalf("CAS ownership count = %d, want root and Program object", ownership)
	}
}

func TestRegisterFinalizedDeploymentBundlePostgresRejectsIdempotencyKeyForAnotherBundle(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	if _, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "shared"),
	); err != nil {
		t.Fatal(err)
	}
	changed := fixture.prepared
	changed.root.Digest = "sha256:" + string(bytes.Repeat([]byte{'d'}, 64))
	_, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, changed,
		fixture.idempotencyRequestFor(t, fixture.environmentID, "shared", changed.root.Digest),
	)
	var conflict idempotency.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want idempotency conflict", err)
	}
}

func TestFinishFinalizedDeploymentBundlePostgresReplaysAvailableExactBundleWithoutReadingObjects(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	created, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "first"),
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &deploymentFinalizeTrackingStore{
		descriptor: fixture.prepared.objects[0],
		body:       make([]byte, fixture.prepared.objects[0].SizeBytes),
	}
	replayed, err := fixture.server.finishFinalizedDeploymentBundle(
		t.Context(), store, fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "replay"),
		func(deploymentFinalizeProgress) error {
			t.Fatal("replay verified object bytes")
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != created.ID || store.statCount != 1 || store.getCount != 0 {
		t.Fatalf("replayed = %+v stats = %d gets = %d", replayed, store.statCount, store.getCount)
	}
}

func TestFinishFinalizedDeploymentBundlePostgresFailsClosedWhenReplayObjectIsUnavailable(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	if _, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "first"),
	); err != nil {
		t.Fatal(err)
	}
	store := &deploymentFinalizeTrackingStore{
		descriptor: fixture.prepared.objects[0],
		statErr:    errors.New("missing"),
	}
	if _, err := fixture.server.finishFinalizedDeploymentBundle(
		t.Context(), store, fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "replay"), func(deploymentFinalizeProgress) error { return nil },
	); err == nil {
		t.Fatal("replay with an unavailable object succeeded")
	}
	if store.getCount != 0 {
		t.Fatalf("object reads = %d, want 0", store.getCount)
	}
}

func TestFinishFinalizedDeploymentBundlePostgresVerifiesSameDigestInAnotherEnvironment(t *testing.T) {
	fixture := newDeploymentFinalizePostgresFixture(t)
	if _, err := fixture.server.registerFinalizedDeploymentBundle(
		t.Context(), fixture.orgID, fixture.projectID, fixture.environmentID, fixture.prepared,
		fixture.idempotencyRequest(t, "first"),
	); err != nil {
		t.Fatal(err)
	}
	otherEnvironment := pgvalue.UUID(uuid.NewV7())
	if _, err := fixture.server.db.CreateEnvironment(t.Context(), db.CreateEnvironmentParams{
		ID: otherEnvironment, OrgID: pgvalue.UUID(fixture.orgID), ProjectID: fixture.projectID,
		Slug: "preview", Name: "Preview", ColorHex: "#315FCE",
	}); err != nil {
		t.Fatal(err)
	}
	store := &deploymentFinalizeTrackingStore{
		descriptor: fixture.prepared.objects[0],
		body:       make([]byte, fixture.prepared.objects[0].SizeBytes),
	}
	if _, err := fixture.server.finishFinalizedDeploymentBundle(
		t.Context(), store, fixture.orgID, fixture.projectID, otherEnvironment, fixture.prepared,
		fixture.idempotencyRequestFor(t, otherEnvironment, "other", fixture.prepared.root.Digest),
		func(deploymentFinalizeProgress) error { return nil },
	); err == nil {
		t.Fatal("unverified object created a deployment in another environment")
	}
	if store.getCount != 1 {
		t.Fatalf("object reads = %d, want full verification", store.getCount)
	}
}

type deploymentFinalizeTrackingStore struct {
	cas.UploadStore
	descriptor cas.Descriptor
	body       []byte
	statErr    error
	statCount  int
	getCount   int
}

func (s *deploymentFinalizeTrackingStore) Stat(context.Context, string) (cas.Object, error) {
	s.statCount++
	if s.statErr != nil {
		return cas.Object{}, s.statErr
	}
	return cas.Object{
		Digest: s.descriptor.Digest, SizeBytes: s.descriptor.SizeBytes, MediaType: s.descriptor.MediaType,
	}, nil
}

func (s *deploymentFinalizeTrackingStore) Get(context.Context, string) (io.ReadCloser, error) {
	s.getCount++
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

type deploymentFinalizePostgresFixture struct {
	pool          *pgxpool.Pool
	server        *Server
	orgID         uuid.UUID
	projectID     pgtype.UUID
	environmentID pgtype.UUID
	prepared      finalizedDeploymentBundle
}

func newDeploymentFinalizePostgresFixture(t *testing.T) deploymentFinalizePostgresFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	queries := db.New(database.Pool)
	regionID := "test-region"
	if _, err := queries.CreateRegion(t.Context(), db.CreateRegionParams{ID: regionID, DisplayName: "Test"}); err != nil {
		t.Fatal(err)
	}
	orgID := uuid.NewV7()
	projectUUID := uuid.NewV7()
	environmentUUID := uuid.NewV7()
	projectID := pgvalue.UUID(projectUUID)
	environmentID := pgvalue.UUID(environmentUUID)
	if _, err := queries.CreateOrganization(t.Context(), db.CreateOrganizationParams{
		ID: pgvalue.UUID(orgID), Name: "Finalize", Slug: "finalize",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateProject(t.Context(), db.CreateProjectParams{
		ID: projectID, OrgID: pgvalue.UUID(orgID), DefaultRegionID: regionID,
		Slug: "finalize", Name: "Finalize", IsDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.CreateEnvironment(t.Context(), db.CreateEnvironmentParams{
		ID: environmentID, OrgID: pgvalue.UUID(orgID), ProjectID: projectID,
		Slug: "staging", Name: "Staging", ColorHex: "#315FCE", IsDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	program := cas.Descriptor{
		Digest:    "sha256:" + string(bytes.Repeat([]byte{'b'}, 64)),
		SizeBytes: 4096, MediaType: deployment.ProgramArtifactMediaType,
	}
	prepared := finalizedDeploymentBundle{
		root: cas.Descriptor{
			Digest:    "sha256:" + string(bytes.Repeat([]byte{'a'}, 64)),
			SizeBytes: 512, MediaType: deployment.DeploymentBundleMediaType,
		},
		bundle: deployment.DeploymentBundle{
			Runtime: deployment.DeploymentBundleRuntime{Artifact: deployment.BundleObject{
				Digest: "sha256:" + string(bytes.Repeat([]byte{'c'}, 64)),
			}},
			Program: deployment.ProgramOutput{Artifact: deployment.ProgramDescriptor{
				Digest: program.Digest, SizeBytes: program.SizeBytes, MediaType: program.MediaType,
			}},
		},
		objects:     []cas.Descriptor{program},
		indexDigest: bytes.Repeat([]byte{2}, 32),
		queueConfig: []byte(`{"formatVersion":0,"queues":[]}`),
	}
	return deploymentFinalizePostgresFixture{
		pool: database.Pool, server: &Server{db: queries, tx: database.Pool},
		orgID: orgID, projectID: projectID, environmentID: environmentID, prepared: prepared,
	}
}

func (f deploymentFinalizePostgresFixture) idempotencyRequest(t *testing.T, key string) idempotency.Request {
	t.Helper()
	return f.idempotencyRequestFor(t, f.environmentID, key, f.prepared.root.Digest)
}

func (f deploymentFinalizePostgresFixture) idempotencyRequestFor(
	t *testing.T,
	environmentID pgtype.UUID,
	key string,
	bundleDigest string,
) idempotency.Request {
	t.Helper()
	environmentUUID, err := pgvalue.UUIDValue(environmentID)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := pgvalue.UUIDValue(f.projectID)
	if err != nil {
		t.Fatal(err)
	}
	request, err := idempotency.NewDeploymentFinalizeRequest(
		environmentUUID, projectID, key,
		idempotency.DeploymentFinalizeFingerprint{BundleDigest: bundleDigest},
	)
	if err != nil {
		t.Fatal(err)
	}
	return request
}
