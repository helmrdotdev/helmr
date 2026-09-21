package db_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// These tests exercise candidate ownership and atomic root publication. They do
// not prove remote upload verification or live preparation authority.
func initializationParams(t *testing.T, f runtest.Fixture) db.RegisterComputerInitializationParams {
	t.Helper()
	work := f.AddRunLease(t, "assigned", time.Now().Add(-time.Minute))
	p := db.RegisterComputerInitializationParams{
		ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
		Digest: dbtest.Digest(uuid.NewV7().String()), SizeBytes: 1024, LogicalBytes: 4096,
		MediaType: computer.DiskMediaType, InitialConfig: []byte(`{"Env":["A=one"],"User":"root"}`),
	}
	if err := f.Pool.QueryRow(t.Context(), `
SELECT r.id, r.desired_version, w.id, w.head_version_id, w.ownership_generation, w.writer_generation
  FROM runtime_instances r JOIN workspaces w ON w.id = r.workspace_id
  JOIN run_leases l ON l.runtime_instance_id = r.id WHERE l.id = $1`, work.LeaseID).Scan(
		&p.RuntimeInstanceID, &p.RuntimeDesiredVersion, &p.ComputerID, &p.VersionID, &p.OwnershipGeneration, &p.WriterGeneration,
	); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_versions
    SET status='initializing', artifact_id=NULL, content_digest=NULL, size_bytes=0, published_at=NULL
    WHERE id=$1`, p.VersionID)
	return p
}

func initializationArtifact(t *testing.T, f runtest.Fixture, tx db.DBTX, p db.RegisterComputerInitializationParams) pgtype.UUID {
	t.Helper()
	id := pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, t.Context(), tx, `WITH lifetime AS (INSERT INTO cas_object_lifetimes (digest) VALUES ($2) ON CONFLICT DO NOTHING) INSERT INTO cas_objects (org_id, digest, size_bytes, media_type) VALUES ($1,$2,$3,$4)`,
		f.OrgID, p.Digest, p.SizeBytes, p.MediaType)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO artifacts
    (id,org_id,project_id,environment_id,digest,kind,size_bytes,media_type)
    VALUES ($1,$2,$3,$4,$5,'workspace_version',$6,$7)`,
		id, f.OrgID, f.ProjectID, f.EnvironmentID, p.Digest, p.SizeBytes, p.MediaType)
	return id
}

func initializationGet(p db.RegisterComputerInitializationParams) db.GetComputerInitializationParams {
	return db.GetComputerInitializationParams{EnvironmentID: p.EnvironmentID, ComputerID: p.ComputerID, RuntimeInstanceID: p.RuntimeInstanceID}
}
func initializationAbandon(p db.RegisterComputerInitializationParams) db.AbandonComputerInitializationParams {
	return db.AbandonComputerInitializationParams{ID: p.ID, EnvironmentID: p.EnvironmentID, ComputerID: p.ComputerID}
}
func initializationConsume(p db.RegisterComputerInitializationParams, artifact pgtype.UUID) db.PublishComputerInitializationParams {
	return db.PublishComputerInitializationParams{ID: p.ID, EnvironmentID: p.EnvironmentID, ComputerID: p.ComputerID, ArtifactID: artifact}
}

func TestComputerInitializationRegistrationRetainsExactIdentity(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	p := initializationParams(t, f)
	first, err := q.RegisterComputerInitialization(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	// Ownership is registered before remote upload and artifact publication.
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM cas_objects WHERE digest = $1`, p.Digest).Scan(&count); err != nil || count != 0 {
		t.Fatalf("registration published a CAS object: count=%d, %v", count, err)
	}
	retry := p
	retry.ID = pgvalue.UUID(uuid.NewV7())
	retry.InitialConfig = []byte(`{"User":"root","Env":["A=one"]}`)
	replayed, err := q.RegisterComputerInitialization(t.Context(), retry)
	if err != nil || replayed.ID != first.ID || replayed.CreatedAt != first.CreatedAt {
		t.Fatalf("registration replay changed receipt: %+v, %v", replayed, err)
	}
	for _, test := range []struct {
		name   string
		change func(*db.RegisterComputerInitializationParams)
	}{
		{"ciphertext", func(p *db.RegisterComputerInitializationParams) { p.Digest = dbtest.Digest("different ciphertext") }},
		{"size", func(p *db.RegisterComputerInitializationParams) { p.SizeBytes++ }},
		{"capacity", func(p *db.RegisterComputerInitializationParams) { p.LogicalBytes *= 2 }},
		{"configuration", func(p *db.RegisterComputerInitializationParams) { p.InitialConfig = []byte(`{"User":"1000"}`) }},
		{"desired-version", func(p *db.RegisterComputerInitializationParams) { p.RuntimeDesiredVersion++ }},
		{"owner", func(p *db.RegisterComputerInitializationParams) { p.OwnershipGeneration++ }},
		{"writer", func(p *db.RegisterComputerInitializationParams) { p.WriterGeneration++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := p
			test.change(&changed)
			if _, err := q.RegisterComputerInitialization(t.Context(), changed); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("changed candidate: %v", err)
			}
		})
	}
	wrongScope := initializationGet(p)
	wrongScope.EnvironmentID = pgvalue.UUID(uuid.NewV7())
	if _, err := q.GetComputerInitialization(t.Context(), wrongScope); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("cross-environment read: %v", err)
	}
	abandoned, err := q.AbandonComputerInitialization(t.Context(), initializationAbandon(p))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := q.AbandonComputerInitialization(t.Context(), initializationAbandon(p))
	if err != nil || repeated.AbandonedAt != abandoned.AbandonedAt {
		t.Fatalf("abandon replay: %+v, %v", repeated, err)
	}
	if _, err := q.RegisterComputerInitialization(t.Context(), p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("abandoned candidate reopened: %v", err)
	}
	// Runtime termination does not erase the upload's retained owner.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET observed_state='failed', terminal_at=clock_timestamp(), terminal_reason_code='test' WHERE id=$1`, p.RuntimeInstanceID)
	retained, err := q.GetComputerInitialization(t.Context(), initializationGet(p))
	if err != nil || retained.Status != "abandoned" || retained.Digest != p.Digest {
		t.Fatalf("lost cleanup owner: %+v, %v", retained, err)
	}
}

func TestComputerInitializationConsumptionIsTransactional(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	p := initializationParams(t, f)
	// A transaction can begin before another transaction creates the candidate.
	// Its old transaction timestamp must not backdate a consumption receipt.
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := q.RegisterComputerInitialization(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	artifact := initializationArtifact(t, f, tx, p)
	consumed, err := db.New(tx).PublishComputerInitialization(t.Context(), initializationConsume(p, artifact))
	if err != nil || consumed.Status != "consumed" {
		t.Fatalf("consume: %+v, %v", consumed, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	retained, err := q.GetComputerInitialization(t.Context(), initializationGet(p))
	if err != nil || retained.Status != "registered" || retained.ArtifactID.Valid {
		t.Fatalf("rollback lost candidate: %+v, %v", retained, err)
	}
	assertInitializationRoot(t, f, p, "initializing", pgtype.UUID{})
	artifact = initializationArtifact(t, f, f.Pool, p)
	consumed, err = q.PublishComputerInitialization(t.Context(), initializationConsume(p, artifact))
	if err != nil {
		t.Fatal(err)
	}
	assertInitializationRoot(t, f, p, "committed", artifact)
	if _, err := q.PublishComputerInitialization(t.Context(), initializationConsume(p, artifact)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("consumed candidate published again: %v", err)
	}
	replayed, err := q.GetComputerInitialization(t.Context(), initializationGet(p))
	if err != nil || consumed.ConsumedAt != replayed.ConsumedAt {
		t.Fatalf("consume replay changed receipt: %+v, %v", replayed, err)
	}
	if _, err := q.RegisterComputerInitialization(t.Context(), p); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("consumed candidate registered again: %v", err)
	}
	resolved, err := q.GetComputerInitialization(t.Context(), initializationGet(p))
	if err != nil || resolved.Status != "consumed" || resolved.ArtifactID != artifact {
		t.Fatalf("terminal registration readback: %+v, %v", resolved, err)
	}
	if _, err := q.AbandonComputerInitialization(t.Context(), initializationAbandon(p)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("consumed candidate abandoned: %v", err)
	}
}

func TestComputerInitializationConsumeAndAbandonSerialize(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	for range 6 {
		p := initializationParams(t, f)
		if _, err := q.RegisterComputerInitialization(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		artifact := initializationArtifact(t, f, f.Pool, p)
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() {
			<-start
			_, err := q.PublishComputerInitialization(t.Context(), initializationConsume(p, artifact))
			results <- err
		}()
		go func() {
			<-start
			_, err := q.AbandonComputerInitialization(t.Context(), initializationAbandon(p))
			results <- err
		}()
		close(start)
		a, b := <-results, <-results
		if !((a == nil && errors.Is(b, pgx.ErrNoRows)) || (b == nil && errors.Is(a, pgx.ErrNoRows))) {
			t.Fatalf("expected one winner: %v / %v", a, b)
		}
	}
}

func TestComputerInitializationRejectsInvalidOwnershipAndLifecycle(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	p := initializationParams(t, f)
	other := initializationParams(t, f)
	for _, test := range []struct {
		name, code string
		change     func(*db.RegisterComputerInitializationParams)
	}{
		{"wrong-computer", "23503", func(p *db.RegisterComputerInitializationParams) { p.ComputerID = other.ComputerID }},
		{"wrong-version", "23503", func(p *db.RegisterComputerInitializationParams) { p.VersionID = other.VersionID }},
		{"wrong-environment", "23503", func(p *db.RegisterComputerInitializationParams) { p.EnvironmentID = pgvalue.UUID(uuid.NewV7()) }},
		{"wrong-format", "23514", func(p *db.RegisterComputerInitializationParams) { p.MediaType = "application/octet-stream" }},
		{"unaligned-capacity", "23514", func(p *db.RegisterComputerInitializationParams) { p.LogicalBytes++ }},
		{"oversized-object", "23514", func(p *db.RegisterComputerInitializationParams) { p.SizeBytes = 3 << 20 }},
		{"invalid-config", "23514", func(p *db.RegisterComputerInitializationParams) { p.InitialConfig = []byte(`null`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := p
			test.change(&bad)
			_, err := q.RegisterComputerInitialization(t.Context(), bad)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != test.code {
				t.Fatalf("invalid candidate accepted/wrong error: %v", err)
			}
		})
	}
	if _, err := q.RegisterComputerInitialization(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	other.Digest = p.Digest
	_, duplicateErr := q.RegisterComputerInitialization(t.Context(), other)
	var duplicate *pgconn.PgError
	if !errors.As(duplicateErr, &duplicate) || duplicate.Code != "23505" || duplicate.ConstraintName != "computer_initializations_digest_key" {
		t.Fatalf("second upload owner reused candidate digest: %v", duplicateErr)
	}
	for _, status := range []string{"consumed", "abandoned"} {
		_, err := f.Pool.Exec(t.Context(), `UPDATE computer_initializations SET status=$2 WHERE id=$1`, p.ID, status)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23514" {
			t.Fatalf("terminal status without receipt %s: %v", status, err)
		}
	}
	exactArtifact := initializationArtifact(t, f, f.Pool, p)
	_, err := f.Pool.Exec(t.Context(), `UPDATE computer_initializations SET status='consumed', artifact_id=$2 WHERE id=$1`, p.ID, exactArtifact)
	var lifecycleError *pgconn.PgError
	if !errors.As(err, &lifecycleError) || lifecycleError.Code != "23514" {
		t.Fatalf("consumed candidate without timestamp: %v", err)
	}
	// Matching environment alone is insufficient: a different artifact cannot
	// consume this candidate even when it belongs to the same Computer owner.
	other.Digest = dbtest.Digest("other-object")
	wrongArtifact := initializationArtifact(t, f, f.Pool, other)
	if _, err := q.PublishComputerInitialization(t.Context(), initializationConsume(p, wrongArtifact)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("different artifact consumed: %v", err)
	}
}

func TestComputerInitializationOnlyOneConsumptionPerComputer(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	first := initializationParams(t, f)
	second := first
	second.ID = pgvalue.UUID(uuid.NewV7())
	second.RuntimeInstanceID = pgvalue.UUID(uuid.NewV7())
	second.RuntimeDesiredVersion = 2
	second.Digest = dbtest.Digest("replacement-initial-ciphertext")
	// A retained, already reclaimed runtime can own a prior candidate for the
	// same Computer. This fixture is not a physical cleanup proof.
	dbtest.MustExec(t, t.Context(), f.Pool, `
INSERT INTO runtime_instances (
    id, org_id, worker_group_id, project_id, environment_id, region_id,
    worker_instance_id, runtime_identity_id, deployment_definition_id, worker_epoch,
    vm_vcpu_count, cpu_config_digest, reserved_cpu_millis, reserved_memory_bytes,
    reserved_guest_ephemeral_disk_bytes, reserved_execution_slots, workspace_id,
    preparation_expires_at, desired_state, desired_version, desired_reason,
    observed_state, terminal_at, reclaimed_at, reclaim_evidence, terminal_reason_code
)
SELECT $2, org_id, worker_group_id, project_id, environment_id, region_id,
       worker_instance_id, runtime_identity_id, deployment_definition_id, worker_epoch,
       vm_vcpu_count, cpu_config_digest, reserved_cpu_millis, reserved_memory_bytes,
       reserved_guest_ephemeral_disk_bytes, reserved_execution_slots, workspace_id,
       preparation_expires_at, 'closed', 2, 'test', 'closed', now(), now(),
       '{"method":"session_closed"}'::jsonb, 'test'
  FROM runtime_instances WHERE id=$1`, first.RuntimeInstanceID, second.RuntimeInstanceID)
	for _, p := range []db.RegisterComputerInitializationParams{first, second} {
		if _, err := q.RegisterComputerInitialization(t.Context(), p); err != nil {
			t.Fatal(err)
		}
	}
	firstArtifact := initializationArtifact(t, f, f.Pool, first)
	secondArtifact := initializationArtifact(t, f, f.Pool, second)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := q.PublishComputerInitialization(t.Context(), initializationConsume(first, firstArtifact))
		results <- err
	}()
	go func() {
		<-start
		_, err := q.PublishComputerInitialization(t.Context(), initializationConsume(second, secondArtifact))
		results <- err
	}()
	close(start)
	a, b := <-results, <-results
	if a == nil {
		a, b = b, a
	}
	if b != nil || !errors.Is(a, pgx.ErrNoRows) {
		t.Fatalf("two initializations consumed: %v / %v", a, b)
	}
}

func TestComputerInitializationRevocationBatchSkipsLockedRuntime(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	var candidates []db.RegisterComputerInitializationParams
	for range 3 {
		// The fixture has already cleared preparation reservations, so each
		// candidate has permanently lost its recorded preparation authority.
		p := initializationParams(t, f)
		if _, err := q.RegisterComputerInitialization(t.Context(), p); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, p)
	}
	locked, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback(context.Background())
	var id pgtype.UUID
	if err := locked.QueryRow(t.Context(), `SELECT id FROM runtime_instances WHERE id=$1 FOR UPDATE`, candidates[0].RuntimeInstanceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if n, err := db.New(tx).AbandonRevokedComputerInitializations(ctx, 1); err != nil || n != 1 {
		t.Fatalf("bounded sweep: %d, %v", n, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range candidates {
		row, err := q.GetComputerInitialization(ctx, initializationGet(p))
		if err != nil || row.Status != "registered" {
			t.Fatalf("rollback changed candidate: %+v, %v", row, err)
		}
	}
	if n, err := q.AbandonRevokedComputerInitializations(ctx, 1); err != nil || n != 1 {
		t.Fatalf("bounded retry: %d, %v", n, err)
	}
	for i, p := range candidates {
		row, err := q.GetComputerInitialization(ctx, initializationGet(p))
		want := "registered"
		if i == 1 {
			want = "abandoned"
		}
		if err != nil || row.Status != want {
			t.Fatalf("candidate %d: %+v, %v", i, row, err)
		}
	}
	if err := locked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := q.AbandonRevokedComputerInitializations(ctx, 10); err != nil || n != 2 {
		t.Fatalf("remaining sweep: %d, %v", n, err)
	}
	if n, err := q.AbandonRevokedComputerInitializations(ctx, 10); err != nil || n != 0 {
		t.Fatalf("replay: %d, %v", n, err)
	}
}

func assertInitializationRoot(t *testing.T, f runtest.Fixture, p db.RegisterComputerInitializationParams, status string, artifact pgtype.UUID) {
	t.Helper()
	var gotStatus string
	var gotArtifact pgtype.UUID
	var digest pgtype.Text
	var size int64
	var published pgtype.Timestamptz
	var head, base pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `
SELECT v.status,v.artifact_id,v.content_digest,v.size_bytes,v.published_at,w.head_version_id,a.base_workspace_version_id
FROM workspace_versions v JOIN workspaces w ON w.id=v.workspace_id
JOIN run_attempts a ON a.workspace_id=w.id
WHERE v.id=$1`, p.VersionID).Scan(&gotStatus, &gotArtifact, &digest, &size, &published, &head, &base); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || gotArtifact != artifact || head != p.VersionID || base != p.VersionID {
		t.Fatalf("root identity/state changed: %s %v %v %v", gotStatus, gotArtifact, head, base)
	}
	if status == "initializing" {
		if digest.Valid || size != 0 || published.Valid {
			t.Fatal("unpublished root has fabricated persistent state")
		}
	} else if !digest.Valid || digest.String != p.Digest || size != p.LogicalBytes || !published.Valid {
		t.Fatal("published root does not match exact candidate")
	}
}

func TestComputerCreationHasUnpublishedStableRoot(t *testing.T) {
	f := runtest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.EnvironmentID, f.DeploymentID)
	work := f.AddRunLease(t, "assigned", time.Now())
	q := db.New(f.Pool)
	scheduleID := pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO schedules (id,environment_id,task_declared_id,deployment_definition_id,deployment_id,cron_pattern,timezone,status,effective_from,next_fire_at)
VALUES ($1,$2,'test-task',$3,$4,'* * * * *','UTC','active',now(),now()+interval '1 minute')`, scheduleID, f.EnvironmentID, f.TaskDefinitionID, f.DeploymentID)
	for _, kind := range []string{"current deployment", "Run deployment", "schedule"} {
		t.Run(kind, func(t *testing.T) {
			computerID, rootID := pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7())
			var err error
			if kind == "current deployment" {
				_, err = q.CreateWorkspaceFromCurrentDeployment(t.Context(), db.CreateWorkspaceFromCurrentDeploymentParams{
					OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID),
					DeploymentDefinitionID: pgvalue.UUID(f.WorkspaceDefinitionID), SandboxDeclaredID: "test-workspace", ID: computerID, InitialVersionID: rootID,
				})
			} else if kind == "Run deployment" {
				_, err = q.CreateWorkspaceFromRunDeployment(t.Context(), db.CreateWorkspaceFromRunDeploymentParams{
					EnvironmentID: pgvalue.UUID(f.EnvironmentID), RunID: pgvalue.UUID(work.RunID), SandboxDeclaredID: "test-workspace", ID: computerID, InitialVersionID: rootID,
				})
			} else {
				_, err = q.CreateWorkspaceForScheduleFire(t.Context(), db.CreateWorkspaceForScheduleFireParams{
					EnvironmentID: pgvalue.UUID(f.EnvironmentID), ScheduleID: scheduleID, ExpectedGeneration: 1,
					SandboxDeclaredID: "test-workspace", ID: computerID, InitialVersionID: rootID,
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			var valid bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT v.id=$2 AND v.status='initializing' AND v.parent_version_id IS NULL
AND v.artifact_id IS NULL AND v.content_digest IS NULL AND v.size_bytes=0 AND v.published_at IS NULL
FROM workspaces w JOIN workspace_versions v ON v.id=w.head_version_id WHERE w.id=$1`, computerID, rootID).Scan(&valid); err != nil || !valid {
				t.Fatalf("new root fabricated persistence: %v %v", valid, err)
			}
			_, err = f.Pool.Exec(t.Context(), `UPDATE workspace_versions SET status='committed',published_at=now() WHERE id=$1`, rootID)
			var check *pgconn.PgError
			if !errors.As(err, &check) || check.Code != "23514" {
				t.Fatalf("root without disk committed: %v", err)
			}
		})
	}
}

func TestComputerInitializationWorkerReceiptSurvivesHeadAdvance(t *testing.T) {
	f := runtest.New(t)
	q := db.New(f.Pool)
	p := initializationParams(t, f)
	if _, err := q.RegisterComputerInitialization(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	artifact := initializationArtifact(t, f, f.Pool, p)
	published, err := q.PublishComputerInitialization(t.Context(), initializationConsume(p, artifact))
	if err != nil {
		t.Fatal(err)
	}
	// This fixture owns an existing lease. Advance a child version under that
	// recorded provenance; do not rewrite or remove the original root receipt.
	next := pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO workspace_versions
(id,environment_id,workspace_id,parent_version_id,artifact_id,content_digest,size_bytes,status,source_workspace_lease_id,ownership_generation,writer_generation,published_at)
SELECT $1,v.environment_id,v.workspace_id,v.id,v.artifact_id,v.content_digest,v.size_bytes,'committed',l.id,l.ownership_generation,l.writer_generation,now()
FROM workspace_versions v JOIN workspace_leases l ON l.workspace_id=v.workspace_id WHERE v.id=$2`, next, p.VersionID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspaces SET head_version_id=$2 WHERE id=$1`, p.ComputerID, next)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, p.RuntimeInstanceID)
	args := db.GetWorkerComputerInitializationParams{RuntimeInstanceID: p.RuntimeInstanceID, RuntimeDesiredVersion: p.RuntimeDesiredVersion, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1}
	receipt, err := q.GetWorkerComputerInitialization(t.Context(), args)
	if err != nil || receipt.ID != published.ID || receipt.VersionID != p.VersionID || receipt.ConsumedAt != published.ConsumedAt {
		t.Fatalf("historical worker receipt lost: %+v %v", receipt, err)
	}
	args.WorkerInstanceID = pgvalue.UUID(uuid.NewV7())
	if _, err = q.GetWorkerComputerInitialization(t.Context(), args); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("another Worker read receipt: %v", err)
	}
}
