package computer

import (
	"bytes"
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

// declaredID is the Sandbox declaration of the runtest deployment.
const declaredID = "test-computer"

// fixture is a runtest environment whose deployment is current, with an
// active API_TOKEN Secret and a secret store that issues Computer CAs.
type fixture struct {
	runtest.Fixture
	scope    Scope
	secretID uuid.UUID
	store    *secret.Store
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	f := fixture{Fixture: runtest.New(t)}
	f.scope = Scope{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$1 WHERE id=$2`, f.DeploymentID, f.EnvironmentID)
	store, err := secret.New(db.New(f.Pool), f.Pool, bytes.Repeat([]byte{71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	record, err := store.Create(t.Context(), f.EnvironmentID, "API_TOKEN", []byte("token"), "fixture-secret")
	if err != nil {
		t.Fatal(err)
	}
	f.secretID = pgvalue.MustUUIDValue(record.ID)
	return f
}

func (f fixture) creator() Creator {
	return NewCreator(f.store)
}

// insertComputer inserts a Computer of the runtest Sandbox with a raw
// API_TOKEN binding, as an already-created Computer.
func (f fixture) insertComputer(t *testing.T, key string) uuid.UUID {
	t.Helper()
	computerID, versionID := uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `
INSERT INTO computers (
    id, environment_id, region_id, sandbox_declared_id, key, head_disk_version_id,
    computer_spec_id, creation_deployment_id
) VALUES ($1, $2, $3, $4, $5, $6,
    (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$2 AND id=$7),
    (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$2 AND id=$7))`,
		computerID, f.EnvironmentID, runtest.Region, declaredID, key, versionID, f.ComputerDefinitionID)
	dbtest.InsertCommittedComputerRoot(t, t.Context(), tx, versionID, f.EnvironmentID, computerID)
	dbtest.MustExec(t, t.Context(), tx, `
INSERT INTO computer_secrets (mode, computer_id, environment_id, placement_kind, placement_target, secret_id)
VALUES ('raw', $1, $2, 'env', 'API_TOKEN', $3)`, computerID, f.EnvironmentID, f.secretID)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return computerID
}

func (f fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.Pool.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func protectedBinding(env string, origins ...string) secretbinding.Binding {
	return secretbinding.Binding{Name: "API_TOKEN", Env: &secretbinding.Env{Name: env, Mode: "protected", AllowedOrigins: origins}}
}

func rawBinding(env string) secretbinding.Binding {
	return secretbinding.Binding{Name: "API_TOKEN", Env: &secretbinding.Env{Name: env, Mode: "raw"}}
}
