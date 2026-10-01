package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computerdbtest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const sourceSandbox = "test-computer"

// liveSourceFixture is a running, entered source Run whose Computer holds a
// raw API_TOKEN binding.
type liveSourceFixture struct {
	runtest.Fixture
	work     runtest.RunLease
	fence    ExecutionFence
	source   uuid.UUID
	secretID uuid.UUID
	store    *secret.Store
	creator  computer.Creator
}

func newLiveSourceFixture(t *testing.T) liveSourceFixture {
	t.Helper()
	f := liveSourceFixture{Fixture: runtest.New(t)}
	f.work = f.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
	expiresAt := time.Now().Add(10 * time.Minute).UTC()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='running', started_at=now(), expires_at=$2 WHERE id=$1`, f.work.LeaseID, expiresAt)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=$2 WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, f.work.LeaseID, expiresAt)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running', started_at=now(), active_started_at=now() WHERE id=$1`, f.work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1 AND number=1`, f.work.RunID)
	var hostClaim, groupClaim int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version, g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&hostClaim, &groupClaim); err != nil {
		t.Fatal(err)
	}
	f.fence = ExecutionFence{
		LeaseID: pgvalue.UUID(f.work.LeaseID), LeaseSequence: 1,
		WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID),
		WorkerEpoch: 1, GroupClaimVersion: groupClaim, HostClaimVersion: hostClaim,
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, f.work.RunID).Scan(&f.source); err != nil {
		t.Fatal(err)
	}
	store, err := secret.New(db.New(f.Pool), f.Pool, bytes.Repeat([]byte{71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.store, f.creator = store, computer.NewCreator(store)
	record, err := store.Create(t.Context(), f.EnvironmentID, "API_TOKEN", []byte("token"), "fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	f.secretID = pgvalue.MustUUIDValue(record.ID)
	f.bind(t, f.source, "raw", "API_TOKEN")
	return f
}

func (f liveSourceFixture) bind(t *testing.T, computerID uuid.UUID, mode, target string) {
	t.Helper()
	placeholder, origins := "", []string{}
	if mode == "protected" {
		placeholder, origins = "hlmr_protected_"+string(bytes.Repeat([]byte{'a'}, 64)), []string{"https://example.com"}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_secrets (mode, computer_id, environment_id, placement_kind, placement_target, secret_id, allowed_origins, placeholder)
 VALUES ($1, $2, $3, 'env', $4, $5, $6, $7)`, mode, computerID, f.EnvironmentID, target, f.secretID, origins, placeholder)
}

func (f liveSourceFixture) insertComputer(t *testing.T, key string) uuid.UUID {
	t.Helper()
	computerID, versionID := uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `
INSERT INTO computers (id, environment_id, region_id, sandbox_declared_id, key, head_disk_version_id, computer_spec_id, creation_deployment_id)
VALUES ($1, $2, $3, $4, $5, $6,
    (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$2 AND id=$7),
    (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$2 AND id=$7))`,
		computerID, f.EnvironmentID, runtest.Region, sourceSandbox, key, versionID, f.ComputerDefinitionID)
	computerdbtest.InsertCommittedComputerRoot(t, t.Context(), tx, versionID, f.EnvironmentID, computerID)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return computerID
}

func (f liveSourceFixture) staleFence() ExecutionFence {
	stale := f.fence
	stale.LeaseSequence++
	return stale
}

func (f liveSourceFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateComputerUsesSourceDeploymentAndFencesBeforeClaim(t *testing.T) {
	f := newLiveSourceFixture(t)
	key := "run-pinned"
	creation := ComputerCreation{
		DeclaredID: sourceSandbox, Key: &key,
		Secrets:        []secretbinding.Binding{{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "API_TOKEN", Mode: "raw"}}},
		IdempotencyKey: "create",
	}
	claimsBefore := f.count(t, `SELECT count(*) FROM idempotency_claims`)
	if _, err := CreateComputer(t.Context(), f.Pool, f.creator, f.staleFence(), creation); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("stale source error = %v", err)
	}
	if claimsAfter := f.count(t, `SELECT count(*) FROM idempotency_claims`); claimsAfter != claimsBefore {
		t.Fatalf("stale source changed claim count from %d to %d", claimsBefore, claimsAfter)
	}

	// The environment has no current deployment; creation resolves the
	// Sandbox from the deployment the source Run is pinned to.
	created, err := CreateComputer(t.Context(), f.Pool, f.creator, f.fence, creation)
	if err != nil {
		t.Fatal(err)
	}
	if created.Snapshot.ID != created.ComputerID.String() || created.Snapshot.Status != computer.StatusAvailable ||
		len(created.Snapshot.Secrets) != 1 ||
		!reflect.DeepEqual(created.Snapshot.Secrets[0], secretbinding.Binding{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "API_TOKEN", Mode: "raw"}}) {
		t.Fatalf("creation snapshot = %+v", created.Snapshot)
	}
	var creationDeploymentID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT creation_deployment_id FROM computers WHERE id=$1`, created.ComputerID).Scan(&creationDeploymentID); err != nil || creationDeploymentID != f.DeploymentID {
		t.Fatalf("creation deployment = %s, %v; want source deployment %s", creationDeploymentID, err, f.DeploymentID)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET status='deleting', desired_state='deleted', updated_at=now()+interval '1 minute' WHERE id=$1`, created.ComputerID)
	replayed, err := CreateComputer(t.Context(), f.Pool, f.creator, f.fence, creation)
	if err != nil {
		t.Fatal(err)
	}
	createdJSON, err := json.Marshal(created.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	replayedJSON, err := json.Marshal(replayed.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ComputerID != created.ComputerID || !bytes.Equal(replayedJSON, createdJSON) {
		t.Fatalf("replayed = %+v, created = %+v", replayed, created)
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$1 WHERE id=$2`, f.DeploymentID, f.EnvironmentID)
	_, err = f.creator.Create(t.Context(), f.Pool, computer.Request{
		Scope:      computer.Scope{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID},
		DeclaredID: sourceSandbox, Key: &key, Secrets: creation.Secrets, IdempotencyKey: creation.IdempotencyKey,
	})
	var keyConflict computer.KeyConflictError
	if !errors.As(err, &keyConflict) {
		t.Fatalf("cross-authority create error = %v, want KeyConflictError", err)
	}
	if n := f.count(t, `SELECT count(*) FROM computer_disk_versions WHERE computer_id=$1`, created.ComputerID); n != 1 {
		t.Fatalf("versions = %d", n)
	}
	if n := f.count(t, `SELECT count(*) FROM computer_secrets WHERE computer_id=$1`, created.ComputerID); n != 1 {
		t.Fatalf("secret placements = %d", n)
	}
}

func TestCreateComputerHoldsBindingsWithinSourceCeiling(t *testing.T) {
	f := newLiveSourceFixture(t)
	protected := []secretbinding.Binding{{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}}
	created, err := CreateComputer(t.Context(), f.Pool, f.creator, f.fence, ComputerCreation{DeclaredID: sourceSandbox, Secrets: protected, IdempotencyKey: "protected"})
	if err != nil {
		t.Fatal(err)
	}
	ca, err := db.New(f.Pool).GetComputerSecretCAPublic(t.Context(), db.GetComputerSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(created.ComputerID)})
	if err != nil || len(ca.Certificate) == 0 || !ca.NotAfter.Time.Equal(created.Snapshot.CreatedAt.AddDate(10, 0, 0).Truncate(time.Second)) {
		t.Fatalf("run-sourced CA = %+v, %v", ca, err)
	}

	// A source narrowed to protected use cannot create raw use.
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_secrets WHERE computer_id=$1`, f.source)
	f.bind(t, f.source, "protected", "TOKEN")
	raw := []secretbinding.Binding{{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "RAW", Mode: "raw"}}}
	if _, err := CreateComputer(t.Context(), f.Pool, f.creator, f.fence, ComputerCreation{DeclaredID: sourceSandbox, Secrets: raw, IdempotencyKey: "raw"}); !errors.Is(err, computer.ErrSecretUnavailable) {
		t.Fatalf("raw escalation = %v", err)
	}

	// A replay whose receipt names a Computer with wider bindings than the
	// source is rejected even though the request itself is within bounds.
	wider := f.insertComputer(t, "wider")
	f.bind(t, wider, "raw", "TOKEN")
	placements, err := secretbinding.NormalizedPlacements(protected)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(placements)
	if err != nil {
		t.Fatal(err)
	}
	request, err := idempotency.NewRuntimeComputerCreateRequest(f.EnvironmentID, f.work.RunID, sourceSandbox, "seeded", idempotency.ComputerCreateFingerprint{Secrets: encoded})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	receipt, err := json.Marshal(map[string]computer.Snapshot{"computer": {
		ID: wider.String(), SandboxID: sourceSandbox, DeploymentID: f.DeploymentID.String(), Status: computer.StatusAvailable,
		Secrets: []secretbinding.Binding{}, CreatedAt: now, UpdatedAt: now, LastActivityAt: now,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(t.Context(), request)
		if err != nil {
			return err
		}
		_, err = claims.Complete(t.Context(), acquired.Claim, receipt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateComputer(t.Context(), f.Pool, f.creator, f.fence, ComputerCreation{DeclaredID: sourceSandbox, Secrets: protected, IdempotencyKey: "seeded"}); !errors.Is(err, computer.ErrSecretUnavailable) {
		t.Fatalf("replay of wider Computer = %v", err)
	}
}

func TestDeleteComputerReplaysAfterTombstoneUnderLiveSource(t *testing.T) {
	f := newLiveSourceFixture(t)
	target := f.insertComputer(t, "delete-replay")
	if _, err := DeleteComputer(t.Context(), f.Pool, f.staleFence(), target, "delete"); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("stale source delete = %v", err)
	}
	deleted, err := DeleteComputer(t.Context(), f.Pool, f.fence, target, "delete")
	if err != nil || deleted.Replayed || deleted.ComputerID != target {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	finalized, err := db.New(f.Pool).FinalizeDeletingComputers(t.Context(), 1)
	if err != nil || len(finalized) != 1 || pgvalue.MustUUIDValue(finalized[0]) != target {
		t.Fatalf("finalized = %+v, %v", finalized, err)
	}
	replayed, err := DeleteComputer(t.Context(), f.Pool, f.fence, target, "delete")
	if err != nil || !replayed.Replayed || replayed.ComputerID != target {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
	if _, err := DeleteComputer(t.Context(), f.Pool, f.fence, uuid.NewV7(), "absent"); !errors.Is(err, computer.ErrNotFound) {
		t.Fatalf("absent target = %v", err)
	}
	if _, err := DeleteComputer(t.Context(), f.Pool, f.fence, f.source, "own"); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("source Computer with its live member = %v", err)
	}
}

func TestReadAndListComputerMembersRequireLiveSource(t *testing.T) {
	f := newLiveSourceFixture(t)
	snapshot, err := ReadComputer(t.Context(), f.Pool, f.fence, f.source)
	if err != nil || snapshot.ID != f.source.String() || len(snapshot.Secrets) != 1 {
		t.Fatalf("read = %+v, %v", snapshot, err)
	}
	if _, err := ReadComputer(t.Context(), f.Pool, f.fence, uuid.NewV7()); !errors.Is(err, computer.ErrNotFound) {
		t.Fatalf("absent read = %v", err)
	}
	if _, err := ReadComputer(t.Context(), f.Pool, f.staleFence(), f.source); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("stale read = %v", err)
	}
	page, err := ListComputerMembers(t.Context(), f.Pool, f.fence, f.source, computer.MembersQuery{})
	if err != nil || len(page.Members) != 1 || page.Members[0].RunID != f.work.RunID.String() {
		t.Fatalf("members = %+v, %v", page, err)
	}
	// A rejected query is the outcome of a committed transaction.
	var input computer.InputError
	for _, query := range []computer.MembersQuery{{Cursor: "broken"}, {Limit: computer.MaxListLimit + 1}} {
		txb := &commitRecorder{pool: f.Pool}
		if _, err := ListComputerMembers(t.Context(), txb, f.fence, f.source, query); !errors.As(err, &input) {
			t.Fatalf("rejected query %+v = %v", query, err)
		}
		if txb.commits != 1 {
			t.Fatalf("rejected query %+v committed %d transactions", query, txb.commits)
		}
	}
	// Transaction failures take precedence over the query outcome.
	if _, err := ListComputerMembers(t.Context(), f.Pool, f.fence, uuid.NewV7(), computer.MembersQuery{Cursor: "broken"}); !errors.Is(err, computer.ErrNotFound) {
		t.Fatalf("absent Computer with a broken cursor = %v", err)
	}
	if _, err := ListComputerMembers(t.Context(), f.Pool, f.staleFence(), f.source, computer.MembersQuery{Cursor: "broken"}); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("stale source with a broken cursor = %v", err)
	}
	if _, err := ListComputerMembers(t.Context(), f.Pool, f.staleFence(), f.source, computer.MembersQuery{}); !errors.Is(err, ErrStaleSource) {
		t.Fatalf("stale members = %v", err)
	}
}

// commitRecorder counts the transactions that commit.
type commitRecorder struct {
	pool    *pgxpool.Pool
	commits int
}

func (r *commitRecorder) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return recordedTx{Tx: tx, recorder: r}, nil
}

type recordedTx struct {
	pgx.Tx
	recorder *commitRecorder
}

func (tx recordedTx) Commit(ctx context.Context) error {
	if err := tx.Tx.Commit(ctx); err != nil {
		return err
	}
	tx.recorder.commits++
	return nil
}

func TestCreateComputerPersistsPinnedCAWithCreationAndRollsBackFailures(t *testing.T) {
	for _, mode := range []string{"protected", "raw", "none", "after-generation-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newLiveSourceFixture(t)
			creation := ComputerCreation{DeclaredID: sourceSandbox, IdempotencyKey: "pinned-ca"}
			switch mode {
			case "raw":
				creation.Secrets = []secretbinding.Binding{{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "RAW", Mode: "raw"}}}
			case "none":
			default:
				creation.Secrets = []secretbinding.Binding{{Name: "API_TOKEN", Env: &secretbinding.Env{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}}}}
			}
			before := f.count(t, `SELECT count(*) FROM computers`)
			if mode == "after-generation-failure" {
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_ca_binding() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
        IF NOT EXISTS (SELECT 1 FROM computers WHERE id=NEW.computer_id AND secret_ca_certificate IS NOT NULL) THEN RAISE EXCEPTION 'CA not generated'; END IF;
        RAISE EXCEPTION 'synthetic post-generation failure'; END $$;
        CREATE TRIGGER reject_ca_binding BEFORE INSERT ON computer_secrets FOR EACH ROW EXECUTE FUNCTION reject_ca_binding();`)
				if _, err := CreateComputer(t.Context(), f.Pool, f.creator, f.fence, creation); err == nil || !strings.Contains(err.Error(), "synthetic post-generation failure") {
					t.Fatalf("creation after CA generation = %v", err)
				}
				if after := f.count(t, `SELECT count(*) FROM computers`); after != before {
					t.Fatal("failed transaction retained Computer")
				}
				if n := f.count(t, `SELECT count(*) FROM idempotency_claims WHERE operation='computer.create'`); n != 0 {
					t.Fatalf("failed transaction retained %d claims", n)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_ca_binding ON computer_secrets`)
			}
			created, err := CreateComputer(t.Context(), f.Pool, f.creator, f.fence, creation)
			if err != nil {
				t.Fatalf("creation: %v", err)
			}
			if after := f.count(t, `SELECT count(*) FROM computers`); after != before+1 {
				t.Fatalf("computers = %d, want %d", after, before+1)
			}
			ca, err := db.New(f.Pool).GetComputerSecretCAPublic(t.Context(), db.GetComputerSecretCAPublicParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(created.ComputerID)})
			if err != nil {
				t.Fatal(err)
			}
			hasCA := mode == "protected" || mode == "after-generation-failure"
			if (len(ca.Certificate) > 0) != hasCA || ca.NotAfter.Valid != hasCA {
				t.Fatal("CA presence differs from bindings")
			}
			if hasCA {
				if !ca.NotAfter.Time.Equal(created.Snapshot.CreatedAt.AddDate(10, 0, 0).Truncate(time.Second)) {
					t.Fatal("expiry not anchored to inserted timestamp")
				}
				if err := secret.ValidateProxyTrust(ca.Certificate, ca.NotAfter.Time, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			// A replay issues no CA, so it succeeds without an issuer.
			replay, err := CreateComputer(t.Context(), f.Pool, computer.NewCreator((*secret.Store)(nil)), f.fence, creation)
			if err != nil || !replay.Replayed || replay.ComputerID != created.ComputerID {
				t.Fatalf("replay = %+v, %v", replay, err)
			}
		})
	}
}
