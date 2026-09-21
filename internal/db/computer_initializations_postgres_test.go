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

// These tests exercise candidate ownership only. The fixture's existing version
// is not a disk-publication proof; preparation admission and the atomic root
// publication transaction are separate integration boundaries.
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
	return p
}

func initializationArtifact(t *testing.T, f runtest.Fixture, tx db.DBTX, p db.RegisterComputerInitializationParams) pgtype.UUID {
	t.Helper()
	id := pgvalue.UUID(uuid.NewV7())
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO cas_objects (org_id, digest, size_bytes, media_type) VALUES ($1,$2,$3,$4)`,
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
func initializationConsume(p db.RegisterComputerInitializationParams, artifact pgtype.UUID) db.ConsumeComputerInitializationParams {
	return db.ConsumeComputerInitializationParams{ID: p.ID, EnvironmentID: p.EnvironmentID, ComputerID: p.ComputerID, ArtifactID: artifact}
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
	consumed, err := db.New(tx).ConsumeComputerInitialization(t.Context(), initializationConsume(p, artifact))
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
	artifact = initializationArtifact(t, f, f.Pool, p)
	consumed, err = q.ConsumeComputerInitialization(t.Context(), initializationConsume(p, artifact))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := q.ConsumeComputerInitialization(t.Context(), initializationConsume(p, artifact))
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
			_, err := q.ConsumeComputerInitialization(t.Context(), initializationConsume(p, artifact))
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
	if _, err := q.ConsumeComputerInitialization(t.Context(), initializationConsume(p, wrongArtifact)); !errors.Is(err, pgx.ErrNoRows) {
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
		_, err := q.ConsumeComputerInitialization(t.Context(), initializationConsume(first, firstArtifact))
		results <- err
	}()
	go func() {
		<-start
		_, err := q.ConsumeComputerInitialization(t.Context(), initializationConsume(second, secondArtifact))
		results <- err
	}()
	close(start)
	a, b := <-results, <-results
	if a == nil {
		a, b = b, a
	}
	var uniqueError *pgconn.PgError
	if b != nil || !errors.As(a, &uniqueError) || uniqueError.Code != "23505" || uniqueError.ConstraintName != "computer_initializations_consumed_computer_uidx" {
		t.Fatalf("two initializations consumed: %v / %v", a, b)
	}
}
