package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunPinnedComputerCreateUsesSourceDeploymentAndFencesBeforeClaim(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	sourceComputerID := fixture.computerIDs[0]
	source, err := fixture.server.startTask(t.Context(), taskStartRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		TaskDeclaredID: "resize-image",
		PayloadPresent: true,
		Payload:        []byte(`{"source":"computer-create"}`),
		ComputerID:     sourceComputerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = NULL WHERE id = $1
	`, fixture.environmentID); err != nil {
		t.Fatal(err)
	}

	stale := errors.New("stale source run")
	var claimsBefore int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimsBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.server.createComputer(t.Context(), computerCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: computerDeclarationSelector{
			Kind: computerDeclarationRunPinned, RunID: source.RunID,
		},
		DeclaredID: "computer.v1", IdempotencyKey: "blocked",
		Authorize: func(context.Context, pgx.Tx) error {
			return stale
		},
	}); !errors.Is(err, stale) {
		t.Fatalf("stale source error = %v", err)
	}
	var claimsAfter int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT count(*) FROM idempotency_claims
	`).Scan(&claimsAfter); err != nil {
		t.Fatal(err)
	}
	if claimsAfter != claimsBefore {
		t.Fatalf("stale source changed claim count from %d to %d", claimsBefore, claimsAfter)
	}

	key := "run-pinned"
	request := computerCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: computerDeclarationSelector{
			Kind: computerDeclarationRunPinned, RunID: source.RunID,
		},
		DeclaredID: "computer.v1", Key: &key,
		Secrets: []api.ComputerSecret{
			{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "API_TOKEN", Mode: "raw"}},
		},
		IdempotencyKey: "create",
		Authorize: func(context.Context, pgx.Tx) error {
			return nil
		},
	}
	created, err := fixture.server.createComputer(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Snapshot.ID != created.ComputerID.String() ||
		created.Snapshot.Status != api.ComputerStatusAvailable ||
		len(created.Snapshot.Secrets) != 1 ||
		!reflect.DeepEqual(created.Snapshot.Secrets[0], api.ComputerSecret{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "API_TOKEN", Mode: "raw"}}) {
		t.Fatalf("creation snapshot = %+v", created.Snapshot)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE computers
		   SET status = 'deleting', desired_state = 'deleted', updated_at = now() + interval '1 minute'
		 WHERE id = $1
	`, created.ComputerID); err != nil {
		t.Fatal(err)
	}
	replayed, err := fixture.server.createComputer(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	createdSnapshot, err := json.Marshal(created.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	replayedSnapshot, err := json.Marshal(replayed.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ComputerID != created.ComputerID ||
		!bytes.Equal(replayedSnapshot, createdSnapshot) {
		t.Fatalf("replayed = %+v, created = %+v", replayed, created)
	}
	if _, err := fixture.pool.Exec(t.Context(), `
		UPDATE environments SET current_deployment_id = $1 WHERE id = $2
	`, fixture.deploymentID, fixture.environmentID); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.server.createComputer(t.Context(), computerCreateRequest{
		OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID,
		Declaration: computerDeclarationSelector{Kind: computerDeclarationPromoted},
		DeclaredID:  request.DeclaredID, Key: request.Key, Secrets: request.Secrets,
		IdempotencyKey: request.IdempotencyKey,
	})
	var keyConflict ComputerKeyConflictError
	if !errors.As(err, &keyConflict) {
		t.Fatalf("cross-authority create error = %v, want ComputerKeyConflictError", err)
	}

	var deploymentID uuid.UUID
	var versionCount, secretPlacementCount int
	if err := fixture.pool.QueryRow(t.Context(), `
		SELECT computers.creation_deployment_id,
		       (SELECT count(*) FROM computer_disk_versions WHERE computer_id = computers.id),
		       (SELECT count(*) FROM computer_secrets WHERE computer_id = computers.id)
		  FROM computers
		 WHERE computers.id = $1
	`, created.ComputerID).Scan(&deploymentID, &versionCount, &secretPlacementCount); err != nil {
		t.Fatal(err)
	}
	if deploymentID != fixture.deploymentID || versionCount != 1 || secretPlacementCount != 1 {
		t.Fatalf(
			"deployment=%s versions=%d secret placements=%d",
			deploymentID,
			versionCount,
			secretPlacementCount,
		)
	}
}

func TestComputerDeleteWithoutActiveMountSucceeds(t *testing.T) {
	for _, recoveryRequired := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery_required=%t", recoveryRequired), func(t *testing.T) {
			testComputerDeleteWithoutActiveMountSucceeds(t, recoveryRequired)
		})
	}
}

func testComputerDeleteWithoutActiveMountSucceeds(t *testing.T, recoveryRequired bool) {
	product := newActorStartPostgresFixture(t, 1)
	computerID := product.computerIDs[0]
	if recoveryRequired {
		dbtest.MustExec(t, t.Context(), product.pool, `
UPDATE computers
   SET status = 'recovery_required', desired_state = 'stopped', dirty_state = 'dirty_state_lost',
       recovery_id=$2, recovery_disk_version_id=head_disk_version_id, recovery_reason='worker_lost', recovery_started_at=now()
 WHERE id = $1`, computerID, uuid.NewV7())
	}
	var originalKey, originalDeclaredID string
	if err := product.pool.QueryRow(t.Context(), `
SELECT key, sandbox_declared_id FROM computers WHERE id = $1`, computerID).Scan(
		&originalKey, &originalDeclaredID,
	); err != nil {
		t.Fatal(err)
	}

	deleted, err := product.server.deleteComputer(t.Context(), computerDeleteRequest{
		OrgID: product.orgID, ProjectID: product.projectID,
		EnvironmentID: product.environmentID, ComputerID: computerID,
		IdempotencyKey: "computer-delete-without-active-mount",
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Replayed || deleted.ComputerID != computerID {
		t.Fatalf("delete result = %+v", deleted)
	}
	var state db.ComputerStatus
	var desiredState, dirtyState string
	if err := product.pool.QueryRow(t.Context(), `
SELECT status, desired_state, dirty_state FROM computers WHERE id = $1`, computerID).Scan(
		&state, &desiredState, &dirtyState,
	); err != nil {
		t.Fatal(err)
	}
	if state != db.ComputerStatusDeleting || desiredState != "deleted" || dirtyState != "clean" {
		t.Fatalf("computer state = %s/%s/%s, want deleting/deleted/clean", state, desiredState, dirtyState)
	}
	finalized, err := product.server.db.FinalizeDeletingComputers(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(finalized) != 1 || pgvalue.MustUUIDValue(finalized[0]) != computerID {
		t.Fatalf("finalized computers = %+v, want %s", finalized, computerID)
	}
	var tombstone struct {
		state             db.ComputerStatus
		revision          int64
		computerSpecID    uuid.UUID
		key               *string
		sandboxDeclaredID *string
		headVersionID     *uuid.UUID
		deletedAt         time.Time
	}
	if err := product.pool.QueryRow(t.Context(), `
SELECT status, revision, computer_spec_id, key,
       sandbox_declared_id, head_disk_version_id, deleted_at
  FROM computers WHERE id = $1`, computerID).Scan(
		&tombstone.state, &tombstone.revision, &tombstone.computerSpecID,
		&tombstone.key, &tombstone.sandboxDeclaredID, &tombstone.headVersionID,
		&tombstone.deletedAt,
	); err != nil {
		t.Fatal(err)
	}
	if tombstone.state != db.ComputerStatusDeleted || tombstone.revision != 3 ||
		tombstone.computerSpecID == uuid.Nil() || tombstone.key != nil ||
		(tombstone.sandboxDeclaredID == nil || *tombstone.sandboxDeclaredID != originalDeclaredID) || tombstone.headVersionID != nil ||
		tombstone.deletedAt.IsZero() {
		t.Fatalf("computer tombstone = %+v", tombstone)
	}
	if originalKey == "" {
		t.Fatal("computer key was empty before deletion")
	}
	replayed, err := product.server.deleteComputer(t.Context(), computerDeleteRequest{
		OrgID: product.orgID, ProjectID: product.projectID,
		EnvironmentID: product.environmentID, ComputerID: computerID,
		IdempotencyKey: "computer-delete-without-active-mount",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.ComputerID != computerID {
		t.Fatalf("delete replay = %+v", replayed)
	}
	_, err = product.server.deleteComputer(t.Context(), computerDeleteRequest{
		OrgID: product.orgID, ProjectID: product.projectID,
		EnvironmentID: product.environmentID, ComputerID: computerID,
		IdempotencyKey: "computer-delete-after-tombstone",
	})
	if err != nil {
		t.Fatalf("fresh delete after tombstone error = %v", err)
	}
	row, err := db.New(product.pool).GetComputer(t.Context(), db.GetComputerParams{OrgID: pgvalue.UUID(product.orgID), ProjectID: pgvalue.UUID(product.projectID), EnvironmentID: pgvalue.UUID(product.environmentID), ID: pgvalue.UUID(computerID)})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := product.server.computerSnapshot(t.Context(), db.New(product.pool), row)
	if err != nil || snapshot.Status != "deleted" || snapshot.SandboxID != originalDeclaredID {
		t.Fatalf("tombstone snapshot=%+v %v", snapshot, err)
	}
	replacementKey := originalKey
	replacement, err := product.server.createComputer(t.Context(), computerCreateRequest{
		OrgID: product.orgID, ProjectID: product.projectID, EnvironmentID: product.environmentID,
		Declaration: computerDeclarationSelector{Kind: computerDeclarationPromoted},
		DeclaredID:  originalDeclaredID, Key: &replacementKey,
		IdempotencyKey: "computer-recreate-after-delete",
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ComputerID == computerID || replacement.Snapshot.Key == nil ||
		*replacement.Snapshot.Key != originalKey {
		t.Fatalf("replacement computer = %+v", replacement)
	}
}

func TestComputerDeleteFinalizationSkipsBlockedRowsAndIsConcurrent(t *testing.T) {
	product := newActorStartPostgresFixture(t, 4)
	blockedComputerID := product.computerIDs[0]
	if _, err := product.server.startTask(t.Context(), taskStartRequest{
		OrgID: product.orgID, ProjectID: product.projectID, EnvironmentID: product.environmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload: []byte(`{"source":"delete-finalizer-blocker"}`), ComputerID: blockedComputerID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := product.pool.Exec(t.Context(), `
UPDATE computers
   SET status = 'deleting', desired_state = 'deleted', updated_at = now() - interval '1 hour'
 WHERE id = $1`, blockedComputerID); err != nil {
		t.Fatal(err)
	}
	for index, computerID := range product.computerIDs[1:] {
		if _, err := product.server.deleteComputer(t.Context(), computerDeleteRequest{
			OrgID: product.orgID, ProjectID: product.projectID,
			EnvironmentID: product.environmentID, ComputerID: computerID,
			IdempotencyKey: fmt.Sprintf("computer-delete-concurrent-%d", index),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := product.pool.Exec(t.Context(), `
UPDATE computers SET updated_at = now() - ($2::int * interval '10 minutes')
 WHERE id = $1`, computerID, 3-index); err != nil {
			t.Fatal(err)
		}
	}

	type result struct {
		ids []pgtype.UUID
		err error
	}
	first, err := product.server.db.FinalizeDeletingComputers(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || pgvalue.MustUUIDValue(first[0]) != product.computerIDs[1] {
		t.Fatalf("oldest eligible finalization = %+v, want %s", first, product.computerIDs[1])
	}

	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			ids, err := product.server.db.FinalizeDeletingComputers(t.Context(), 1)
			results <- result{ids: ids, err: err}
		}()
	}
	close(start)
	seen := map[uuid.UUID]struct{}{product.computerIDs[1]: {}}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		for _, rawID := range result.ids {
			id := pgvalue.MustUUIDValue(rawID)
			if _, duplicate := seen[id]; duplicate {
				t.Fatalf("computer %s finalized twice", id)
			}
			seen[id] = struct{}{}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("finalized computers = %v, want three eligible rows", seen)
	}
	var blockedStatus db.ComputerStatus
	if err := product.pool.QueryRow(t.Context(), `
SELECT status FROM computers WHERE id = $1`, blockedComputerID).Scan(&blockedStatus); err != nil {
		t.Fatal(err)
	}
	if blockedStatus != db.ComputerStatusDeleting {
		t.Fatalf("blocked computer state = %s, want deleting", blockedStatus)
	}
}

func TestProtectedComputerCreatePersistsFixedMixedBindings(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	fixture.server.secretProxy = testComputerCAStore(t, fixture.pool)
	request := computerCreateRequest{OrgID: fixture.orgID, ProjectID: fixture.projectID, EnvironmentID: fixture.environmentID, Declaration: computerDeclarationSelector{Kind: computerDeclarationPromoted}, DeclaredID: "computer.v1", IdempotencyKey: "mixed-protected", Secrets: []api.ComputerSecret{
		{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "GH_TOKEN", Mode: "protected", AllowedOrigins: []string{"HTTPS://API.GITHUB.COM:443/"}}},
		{Name: "API_TOKEN", Env: &api.SecretEnv{Name: "RAW_TOKEN", Mode: "raw"}},
		{Name: "API_TOKEN", File: &api.SecretFile{Path: "/run/secrets/key"}},
	}}
	result, err := fixture.server.createComputer(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.New(fixture.pool).ListComputerSecrets(t.Context(), pgvalue.UUID(result.ComputerID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || len(result.Snapshot.Secrets) != 3 {
		t.Fatal("mixed bindings not persisted")
	}
	for _, row := range rows {
		if row.PlacementTarget == "GH_TOKEN" {
			if row.Mode != "protected" || len(row.AllowedOrigins) != 1 || row.AllowedOrigins[0] != "https://api.github.com" || len(row.Placeholder) != len("hlmr_protected_")+64 {
				t.Fatal("protected metadata or selector incorrect")
			}
		} else if row.Mode != "raw" || row.Placeholder != "" || len(row.AllowedOrigins) != 0 {
			t.Fatal("raw binding acquired protected state")
		}
		if row.SecretID != rows[0].SecretID {
			t.Fatal("bindings changed stable Secret identity")
		}
	}
	replay, err := fixture.server.createComputer(t.Context(), request)
	if err != nil || !replay.Replayed || replay.ComputerID != result.ComputerID {
		t.Fatal("creation replay changed Computer")
	}
	after, err := db.New(fixture.pool).ListComputerSecrets(t.Context(), pgvalue.UUID(result.ComputerID))
	if err != nil || !reflect.DeepEqual(rows, after) {
		t.Fatal("creation replay changed fixed selectors")
	}
	wire, err := json.Marshal(result.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte("hlmr_protected_")) || bytes.Contains(wire, []byte("ciphertext")) || bytes.Contains(wire, []byte("private_key")) {
		t.Fatal("public readback exposed private transport data")
	}
}

func TestComputerRetainsCreationProvenanceWithoutSandboxDeclaration(t *testing.T) {
	f := newActorStartPostgresFixture(t, 1)
	computerID := pgvalue.UUID(f.computerIDs[0])
	dbtest.MustExec(t, t.Context(), f.pool, `DELETE FROM deployment_definitions WHERE environment_id=$1 AND kind='sandbox'`, f.environmentID)
	q := db.New(f.pool)
	record, err := q.GetComputer(t.Context(), db.GetComputerParams{
		OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID),
		EnvironmentID: pgvalue.UUID(f.environmentID), ID: computerID,
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.server.computerSnapshot(t.Context(), q, record)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DeploymentID != f.deploymentID.String() || snapshot.SandboxID != "computer.v1" {
		t.Fatalf("creation provenance: %+v", snapshot)
	}
	item, err := q.GetComputerListItemByKey(t.Context(), db.GetComputerListItemByKeyParams{
		OrgID: pgvalue.UUID(f.orgID), ProjectID: pgvalue.UUID(f.projectID),
		EnvironmentID: pgvalue.UUID(f.environmentID), Key: pgvalue.Text(f.computerKeys[0]),
	})
	if err != nil || item.DeploymentID != pgvalue.UUID(f.deploymentID) || item.SandboxID != snapshot.SandboxID {
		t.Fatalf("list provenance: %+v, %v", item, err)
	}

}
