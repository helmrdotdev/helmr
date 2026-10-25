package deployment

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
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
 INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES($3,$1,$2,'test','Test','#112233',1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp());`, pgx.QueryExecModeSimpleProtocol, org, project, env); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), database.Pool, `INSERT INTO api_keys(id,org_id,project_id,environment_id,name,key_prefix,token_hash) VALUES($1,$2,$3,$4,'fixture','fixture',convert_to('fixture','UTF8'))`, key, org, project, env)
	dbtest.SetAPIKeyRole(t, database.Pool, key, "deployments.publish", "deployments.promote", "deployments.read", "templates.refresh", "templates.read", "agents.read")
	principal := auth.Principal{Kind: auth.PrincipalKindAPIKey, APIKeyID: key, CredentialVersion: 1, OrgID: org, ProjectID: project.String(), EnvironmentID: env.String()}
	for _, permission := range []auth.Permission{auth.PermissionDeploymentsPublish, auth.PermissionDeploymentsPromote, auth.PermissionDeploymentsRead, auth.PermissionTemplatesRefresh, auth.PermissionTemplatesRead, auth.PermissionAgentsRead} {
		principal.Grants = append(principal.Grants, auth.PermissionGrant{ProjectID: project.String(), EnvironmentID: env.String(), Permission: permission})
	}
	scope := auth.Scope{OrgID: org, ProjectID: project.String(), EnvironmentID: env.String()}
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	return deploymentFinalizePostgresFixture{pool: database.Pool, request: Finalization{principal: principal, scope: scope, orgID: org, projectID: pgvalue.UUID(project), environmentID: pgvalue.UUID(env), idempotencyKey: "finalize", bundle: preparedBundle{root: cas.Descriptor{Digest: artifacttest.Digest("bundle"), SizeBytes: 1024, MediaType: bundle.MediaType}, bundle: bundle.Manifest{Program: output, Preparations: artifacttest.PreparationOutputs(t, output)}, objects: []cas.Descriptor{{Digest: output.Artifact.Digest, SizeBytes: output.Artifact.SizeBytes, MediaType: output.Artifact.MediaType}, {Digest: output.Metadata.Definitions[2].Template.Seed.ArtifactDigest, SizeBytes: 4096, MediaType: definition.ComputerSeedMediaType}}}}}
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
				req.idempotencyKey = "different-key"
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
	for table, want := range map[string]int{"deployments": 1, "deployment_events": 1, "idempotency_receipts": 2, "image_specs": 1, "template_definitions": 1, "agents": 2, "agent_definitions": 2, "cas_objects": 3, "deployment_objects": 3} {
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
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.BuildSecrets = []definition.SecretBinding{{Secret: "MISSING_TOKEN", Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "raw"}}}
	if _, err := register(t.Context(), f.pool, f.request); err == nil {
		t.Fatal("foreign Secret accepted")
	}
	for _, table := range []string{"deployments", "deployment_events", "idempotency_receipts", "image_specs", "template_definitions", "agents", "agent_definitions", "deployment_objects", "cas_objects", "cas_blobs"} {
		var count int
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s=%d: %v", table, count, err)
		}
	}
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.BuildSecrets = []definition.SecretBinding{}
	if _, err := register(t.Context(), f.pool, f.request); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterPostgresRechecksScope(t *testing.T) {
	for _, kind := range []string{"organization", "project"} {
		t.Run(kind, func(t *testing.T) {
			f := newDeploymentFinalizePostgresFixture(t)
			switch kind {
			case "organization":
				f.request.orgID = uuid.NewV7()
			case "project":
				f.request.projectID = pgvalue.UUID(uuid.NewV7())
			}
			if _, err := register(t.Context(), f.pool, f.request); !errors.Is(err, ErrNotFound) {
				t.Fatalf("scope check: %v", err)
			}
			var count int
			if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_receipts`).Scan(&count); err != nil || count != 0 {
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
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES($1,$2,$3,'other','Other','#112233',1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())`, otherEnv, f.request.orgID, f.request.projectID); err != nil {
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

func TestRegisterScopesLogicalSecretsAndPreservesReplays(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	firstEnv := pgvalue.MustUUIDValue(f.request.environmentID)
	secondEnv := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,max_outstanding_admissions,max_causal_depth,max_resident_computers,max_cpu_millis,max_memory_bytes,max_reserved_storage_bytes,preparation_timeout_ms,admission_rate_per_second,admission_burst,admission_tokens,admission_refilled_at) VALUES($1,$2,$3,'other','Other','#112233',1024,32,1024,1024000,1099511627776,1099511627776,600000,1000000,1000000,1000000,clock_timestamp())`, secondEnv, f.request.orgID, f.request.projectID)
	refs := []definition.SecretBinding{{Secret: "API_TOKEN", Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "raw"}}}
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Secrets = refs
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.BuildSecrets = refs
	records := make([]Record, 0, 2)
	for _, env := range []uuid.UUID{firstEnv, secondEnv} {
		secret := insertRegistrationSecret(t, f.pool, env, "API_TOKEN")
		request := f.request
		request.environmentID = pgvalue.UUID(env)
		record, err := register(t.Context(), f.pool, request)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
		var bindings int
		if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_bindings WHERE environment_id=$1 AND secret_id=$2`, env, secret).Scan(&bindings); err != nil || bindings != 2 {
			t.Fatalf("Environment did not pin its own Secret: %d %v", bindings, err)
		}
		dbtest.MustExec(t, t.Context(), f.pool, `UPDATE secrets SET status='revoked',current_version_id=NULL,revoked_at=clock_timestamp() WHERE id=$1`, secret)
		for _, key := range []string{request.idempotencyKey, "same-bundle-new-key"} {
			replay := request
			replay.idempotencyKey = key
			got, err := register(t.Context(), f.pool, replay)
			if err != nil || got.ID != record.ID {
				t.Fatalf("revocation changed registered bundle replay: %+v %v", got, err)
			}
		}
		request.idempotencyKey = "new-bundle"
		request.bundle.root.Digest = artifacttest.Digest("new bundle")
		if _, err := register(t.Context(), f.pool, request); !errors.Is(err, computer.ErrSecretUnavailable) {
			t.Fatalf("new Deployment accepted revoked Secret: %v", err)
		}
		var deployments, retained int
		if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployments WHERE environment_id=$1`, env).Scan(&deployments); err != nil || deployments != 1 {
			t.Fatalf("failed registration retained state: %d %v", deployments, err)
		}
		if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM secret_bindings WHERE environment_id=$1 AND secret_id=$2`, env, secret).Scan(&retained); err != nil || retained != 2 {
			t.Fatalf("historical bindings changed: %d %v", retained, err)
		}
	}
	if records[0].ID == records[1].ID {
		t.Fatal("cross-Environment Deployment identity reused")
	}
	var digests int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(DISTINCT spec_digest) FROM image_specs`).Scan(&digests); err != nil || digests != 1 {
		t.Fatalf("logical definition changed across Environments: %d %v", digests, err)
	}
}

func insertRegistrationSecret(t *testing.T, pool *pgxpool.Pool, env uuid.UUID, name string) uuid.UUID {
	t.Helper()
	id, version := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), pool, `INSERT INTO secrets(id,environment_id,name,current_version_id) VALUES($1,$2,$3,$4); INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($4,$1,1,decode(repeat('00',12),'hex'),decode(repeat('00',16),'hex'))`, pgx.QueryExecModeSimpleProtocol, id, env, name, version)
	return id
}

func TestRegisterRechecksSecretAfterConcurrentRevocation(t *testing.T) {
	f := newDeploymentFinalizePostgresFixture(t)
	secret := insertRegistrationSecret(t, f.pool, pgvalue.MustUUIDValue(f.request.environmentID), "API_TOKEN")
	f.request.bundle.bundle.Program.Metadata.Definitions[2].Template.Secrets = []definition.SecretBinding{{Secret: "API_TOKEN", Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "raw"}}}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var locked uuid.UUID
	if err = tx.QueryRow(t.Context(), `SELECT id FROM secrets WHERE id=$1 FOR UPDATE`, secret).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := register(ctx, f.pool, f.request); done <- err }()
	for {
		var waiting bool
		if err = f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'SELECT name,id FROM secrets%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("registration did not wait for revocation: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err = tx.Exec(t.Context(), `UPDATE secrets SET status='revoked',current_version_id=NULL,revoked_at=clock_timestamp() WHERE id=$1`, secret); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, computer.ErrSecretUnavailable) {
		t.Fatalf("concurrent revocation bypassed admission: %v", err)
	}
	var count int
	if err = f.pool.QueryRow(t.Context(), `SELECT count(*) FROM deployments`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("racing registration left state: %d %v", count, err)
	}
}
