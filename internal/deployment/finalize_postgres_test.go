package deployment

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type deploymentFinalizePostgresFixture struct {
	pool    *pgxpool.Pool
	request Finalization
}

func newDeploymentFinalizePostgresFixture(t *testing.T) deploymentFinalizePostgresFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	org, project, env := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	if _, err := database.Pool.Exec(t.Context(), `INSERT INTO regions(id,display_name) VALUES('test','Test');
 INSERT INTO organizations(id,name,slug) VALUES($1,'Org','org');
 INSERT INTO projects(id,org_id,default_region_id,slug,name) VALUES($2,$1,'test','project','Project');
 INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) VALUES('until_environment_deletion',$3,$1,$2,'test','Test','#112233');`, pgx.QueryExecModeSimpleProtocol, org, project, env); err != nil {
		t.Fatal(err)
	}
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	return deploymentFinalizePostgresFixture{pool: database.Pool, request: Finalization{orgID: org, projectID: pgvalue.UUID(project), environmentID: pgvalue.UUID(env), retryKey: "finalize", bundle: preparedBundle{root: cas.Descriptor{Digest: artifacttest.Digest("bundle"), SizeBytes: 1024, MediaType: bundle.MediaType}, bundle: bundle.Manifest{Program: output}, objects: []cas.Descriptor{{Digest: output.Artifact.Digest, SizeBytes: output.Artifact.SizeBytes, MediaType: output.Artifact.MediaType}, {Digest: output.Metadata.Definitions[2].Computer.Seed.ArtifactDigest, SizeBytes: 4096, MediaType: definition.ComputerSeedMediaType}}}}}
}

func TestRegisterPostgresConvergesConcurrentRequests(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	records := make([]Record, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i := range records {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := f.request
			if i == 2 {
				req.retryKey = "different-key"
			}
			records[i], errs[i] = register(t.Context(), f.pool, req)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if records[i].ID != records[0].ID {
			t.Fatal("same bundle created distinct deployments")
		}
	}
	for table, want := range map[string]int{"deployments": 1, "telemetry_outbox": 1, "platform_retry_keys": 2, "computer_preparation_specs": 1, "computer_definitions": 1, "agents": 2, "agent_definitions": 2, "cas_objects": 3, "deployment_objects": 3} {
		var count int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s=%d want %d: %v", table, count, want, err)
		}
	}
	req := f.request
	req.bundle.root.Digest = artifacttest.Digest("different bundle")
	if _, err := register(t.Context(), f.pool, req); !errors.Is(err, ErrFinalizationConflict) {
		t.Fatalf("conflicting key: %v", err)
	}
}

func TestRegisterPostgresRollsBackDefinitionsObjectsAndReceipt(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	// A well-formed but foreign Secret fails inside the registration transaction.
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Computer.BuildSecrets = []definition.SecretBinding{{SecretID: uuid.NewV7().String(), Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "raw"}}}
	if _, err := register(t.Context(), f.pool, f.request); err == nil {
		t.Fatal("foreign Secret accepted")
	}
	for _, table := range []string{"deployments", "telemetry_outbox", "platform_retry_keys", "computer_preparation_specs", "computer_definitions", "agents", "agent_definitions", "deployment_objects", "cas_objects", "cas_blobs"} {
		var count int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s=%d: %v", table, count, err)
		}
	}
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Computer.BuildSecrets = []definition.SecretBinding{}
	if _, err := register(t.Context(), f.pool, f.request); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterPostgresRechecksScopeAndRetirement(t *testing.T) {
	for _, kind := range []string{"organization", "project", "retired"} {
		t.Run(kind, func(t *testing.T) {
			f := newDeploymentFinalizePostgresFixture(t)
			switch kind {
			case "organization":
				f.request.orgID = uuid.NewV7()
			case "project":
				f.request.projectID = pgvalue.UUID(uuid.NewV7())
			case "retired":
				if _, err := f.pool.Exec(t.Context(), `UPDATE environments SET retired_at=clock_timestamp() WHERE id=$1`, f.request.environmentID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := register(t.Context(), f.pool, f.request); !errors.Is(err, ErrNotFound) {
				t.Fatalf("scope check: %v", err)
			}
			var count int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM platform_retry_keys`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unexpected receipt %d: %v", count, err)
			}
		})
	}
}

func TestRegisterPostgresOwnedDescriptorConflictRollsBack(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	object := f.request.bundle.objects[0]
	if _, err := db.New(f.pool).UpsertCasObject(t.Context(), db.UpsertCasObjectParams{OrgID: pgvalue.UUID(f.request.orgID), Digest: object.Digest, SizeBytes: object.SizeBytes + 1, MediaType: object.MediaType}); err != nil {
		t.Fatal(err)
	}
	if _, err := register(t.Context(), f.pool, f.request); err == nil {
		t.Fatal("conflicting object descriptor accepted")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployments`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial deployment %d: %v", count, err)
	}
}

// Existing verified content is reused only inside its owning Environment.
func TestFinalizeReusesRegisteredBundleWithoutReadingObjects(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	first, err := register(t.Context(), f.pool, f.request)
	if err != nil {
		t.Fatal(err)
	}
	store := &registeredObjectStore{objects: map[string]cas.Descriptor{}}
	for _, object := range f.request.bundle.objects {
		store.objects[object.Digest] = object
	}
	finalizer := NewFinalizer(store, store, bundle.Admission{}, discardLogger())
	result, err := finalizer.Finalize(t.Context(), f.pool, f.request, func(string) error { t.Fatal("replay should not verify"); return nil })
	if err != nil || result.ID != first.ID {
		t.Fatalf("replay: %+v %v", result, err)
	}

	otherEnv := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) VALUES('until_environment_deletion',$1,$2,$3,'other','Other','#112233')`, otherEnv, f.request.orgID, f.request.projectID); err != nil {
		t.Fatal(err)
	}
	other := f.request
	other.environmentID = pgvalue.UUID(otherEnv)
	if available, err := finalizer.available(t.Context(), f.pool, other); err != nil || available {
		t.Fatalf("cross-Environment verification shortcut: %v %v", available, err)
	}
	if _, err := finalizer.Finalize(t.Context(), f.pool, other, func(string) error { return nil }); err == nil || store.reads != 1 {
		t.Fatalf("cross-Environment did not read/verify bytes: reads=%d err=%v", store.reads, err)
	}
	object := f.request.bundle.objects[0]
	delete(store.objects, object.Digest)
	if _, err := finalizer.Finalize(t.Context(), f.pool, f.request, func(string) error { return nil }); err == nil {
		t.Fatal("missing replay object accepted")
	}
}

type registeredObjectStore struct {
	cas.UploadStore
	objects map[string]cas.Descriptor
	reads   int
}

func (s *registeredObjectStore) Stat(_ context.Context, digest string) (cas.Object, error) {
	d, ok := s.objects[digest]
	if !ok {
		return cas.Object{}, errors.New("unavailable object")
	}
	return cas.Object{Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType}, nil
}
func (s *registeredObjectStore) Get(context.Context, string) (io.ReadCloser, error) {
	s.reads++
	return nil, errors.New("unexpected object read")
}
