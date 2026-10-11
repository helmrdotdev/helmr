package agent

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
)

func TestComputerDefinitionRegistrationReusesExactInputsAndSeparatesPolicy(t *testing.T) {
	f := newFixture(t)
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	every, age := int64(1001), int64(3507)
	output.Metadata.Definitions[2].Computer.Refresh = &definition.ComputerRefresh{EveryMs: every, MaxAgeMs: &age}
	register := func(deployment uuid.UUID, output artifact.ProgramOutput) error {
		return db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			if err := lockDefinitionTestEnvironment(t, tx, f.env); err != nil {
				return err
			}
			return registerComputerDefinitions(t.Context(), tx, f.env, deployment, output)
		})
	}
	if err := register(f.deployment, output); err != nil {
		t.Fatal(err)
	}
	second := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$2,$3)`, f.env, second, artifacttest.Digest("second")); err != nil {
		t.Fatal(err)
	}
	if err := register(second, output); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_preparation_specs WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 2 {
		t.Fatalf("specs %d: %v", count, err)
	}
	var gotEvery, gotAge int64
	var scheduled bool
	if err := f.pool.QueryRow(t.Context(), `SELECT d.refresh_every_ms,d.max_image_age_ms,s.next_refresh_at IS NOT NULL FROM computer_definitions d JOIN computer_preparation_specs s ON s.environment_id=d.environment_id AND s.id=d.preparation_spec_id WHERE d.environment_id=$1 AND d.deployment_id=$2`, f.env, second).Scan(&gotEvery, &gotAge, &scheduled); err != nil || gotEvery != every || gotAge != age || scheduled {
		t.Fatalf("policy %d %d scheduled=%v: %v", gotEvery, gotAge, scheduled, err)
	}
	// A failed registration never leaves a new input row behind.
	third := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$2,$3)`, f.env, third, artifacttest.Digest("third")); err != nil {
		t.Fatal(err)
	}
	output.Artifact.Digest = artifacttest.Digest("new program")
	sentinel := errors.New("rollback caller's larger transaction")
	err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		if err := lockDefinitionTestEnvironment(t, tx, f.env); err != nil {
			return err
		}
		if err := registerComputerDefinitions(t.Context(), tx, f.env, third, output); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_preparation_specs WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 2 {
		t.Fatalf("rollback specs %d: %v", count, err)
	}
}

func TestComputerDefinitionRegistrationRejectsForeignSecretAndConflictingStoredInput(t *testing.T) {
	f := newFixture(t)
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	output.Metadata.Definitions[2].Computer.BuildSecrets = []definition.SecretBinding{{SecretID: uuid.NewV7().String(), Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "raw"}}}
	err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		if err := lockDefinitionTestEnvironment(t, tx, f.env); err != nil {
			return err
		}
		return registerComputerDefinitions(t.Context(), tx, f.env, f.deployment, output)
	})
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign Secret %v", err)
	}
	output.Metadata.Definitions[2].Computer.BuildSecrets = []definition.SecretBinding{}
	specs, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := artifact.CanonicalComputerPreparationSpec(specs[0])
	if err != nil {
		t.Fatal(err)
	}
	digest, err := artifact.ComputerPreparationSpecDigest(specs[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed) VALUES($1,$2,$3,$4,'{}')`, f.env, uuid.NewV7(), digest, raw); err != nil {
		t.Fatal(err)
	}
	err = db.RunTx(context.Background(), f.pool, func(tx pgx.Tx) error {
		if err := lockDefinitionTestEnvironment(t, tx, f.env); err != nil {
			return err
		}
		return registerComputerDefinitions(t.Context(), tx, f.env, f.deployment, output)
	})
	if err == nil {
		t.Fatal("registered conflicting seed projection")
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_definitions WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 1 {
		t.Fatalf("partial registration %d: %v", count, err)
	}
}

func TestProgramDefinitionRegistrationPinsComputerAndPreservesAgentIdentity(t *testing.T) {
	f := newFixture(t)
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	duration, idle := int64(1101), int64(2203)
	output.Metadata.Definitions[0].Agent.MaxTurnDurationMs = &duration
	output.Metadata.Definitions[0].Agent.CloseAfterIdleMs = &idle
	register := func(deployment uuid.UUID) error {
		return db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			if err := lockDefinitionTestEnvironment(t, tx, f.env); err != nil {
				return err
			}
			return RegisterProgramDefinitions(t.Context(), tx, f.env, deployment, output)
		})
	}
	if err := register(f.deployment); err != nil {
		t.Fatal(err)
	}
	second := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO deployments(environment_id,id,bundle_digest) VALUES($1,$2,$3)`, f.env, second, artifacttest.Digest("second")); err != nil {
		t.Fatal(err)
	}
	if err := register(second); err != nil {
		t.Fatal(err)
	}
	var identities, defs int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(DISTINCT agent_id),count(*) FROM agent_definitions WHERE environment_id=$1 AND definition_key='build'`, f.env).Scan(&identities, &defs); err != nil || identities != 1 || defs != 2 {
		t.Fatalf("stable identity %d/%d: %v", identities, defs, err)
	}
	var gotDuration, gotIdle int64
	var computer string
	if err := f.pool.QueryRow(t.Context(), `SELECT max_turn_duration_ms,close_after_idle_ms,computer_definition_key FROM agent_definitions WHERE environment_id=$1 AND deployment_id=$2 AND definition_key='build'`, f.env, second).Scan(&gotDuration, &gotIdle, &computer); err != nil || gotDuration != duration || gotIdle != idle || computer != "repo" {
		t.Fatalf("pinned definition %d/%d/%s: %v", gotDuration, gotIdle, computer, err)
	}
	// Even a direct write cannot bind an Agent to another deployment's Computer.
	if _, err := f.pool.Exec(t.Context(), `UPDATE agent_definitions SET computer_definition_key='missing' WHERE environment_id=$1 AND deployment_id=$2 AND definition_key='build'`, f.env, second); err == nil {
		t.Fatal("missing Computer reference accepted")
	}
}

func TestDispatchUsesMillisecondTurnDuration(t *testing.T) {
	f := newFixture(t)
	if _, err := f.pool.Exec(t.Context(), `UPDATE agent_definitions SET max_turn_duration_ms=1101 WHERE environment_id=$1`, f.env); err != nil {
		t.Fatal(err)
	}
	a := f.enqueue(t, "milliseconds")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	var duration float64
	if err := f.pool.QueryRow(t.Context(), `SELECT extract(epoch FROM deadline_at-started_at)*1000 FROM turns WHERE environment_id=$1 AND id=$2`, f.env, a.TurnID).Scan(&duration); err != nil || duration < 1101 || duration > 1102 {
		t.Fatalf("duration=%f ms: %v", duration, err)
	}
}

func lockDefinitionTestEnvironment(t *testing.T, tx pgx.Tx, env uuid.UUID) error {
	t.Helper()
	var id uuid.UUID
	return tx.QueryRow(t.Context(), `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, env).Scan(&id)
}

func TestComputerDefinitionRegistrationUsesStableScopedSecretReferences(t *testing.T) {
	f := newFixture(t)
	otherEnv, foreign, revoked := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO environments(history_retention_mode,id,org_id,project_id,slug,name,color_hex) SELECT 'until_environment_deletion',$2,org_id,project_id,'other','Other','#112233' FROM environments WHERE id=$1;
 INSERT INTO secrets(id,environment_id,name,status,revoked_at) VALUES($3,$2,'TOKEN','revoked',clock_timestamp()),($4,$1,'OLD_TOKEN','revoked',clock_timestamp());`, pgx.QueryExecModeSimpleProtocol, f.env, otherEnv, foreign, revoked); err != nil {
		t.Fatal(err)
	}
	output := artifact.ProgramOutput{Artifact: artifact.ProgramDescriptor{Digest: artifacttest.Digest("program"), SizeBytes: 4096, MediaType: artifact.ProgramArtifactMediaType}, Metadata: artifacttest.ProgramMetadata(t)}
	register := func() error {
		return db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
			if err := lockDefinitionTestEnvironment(t, tx, f.env); err != nil {
				return err
			}
			return RegisterProgramDefinitions(t.Context(), tx, f.env, f.deployment, output)
		})
	}
	output.Metadata.Definitions[2].Computer.BuildSecrets = []definition.SecretBinding{{SecretID: foreign.String(), Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "raw"}}}
	if err := register(); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign stable reference: %v", err)
	}
	output.Metadata.Definitions[2].Computer.BuildSecrets = []definition.SecretBinding{
		{SecretID: revoked.String(), Env: &definition.SecretBindingEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://API.EXAMPLE.COM:443", "https://api.example.com"}}},
		{SecretID: revoked.String(), File: &secretbinding.File{Path: "/etc/build/token"}},
	}
	output.Metadata.Definitions[2].Computer.Secrets = []definition.SecretBinding{{SecretID: revoked.String(), Env: &definition.SecretBindingEnv{Name: "RUNTIME_TOKEN", Mode: "raw"}}}
	if err := register(); err != nil {
		t.Fatalf("registration must not become a whole-deployment Secret readiness gate: %v", err)
	}
	rows, err := f.pool.Query(t.Context(), `SELECT preparation_spec_id IS NOT NULL,placement_kind,placement_target,mode,allowed_origins
        FROM computer_secret_bindings WHERE environment_id=$1 ORDER BY preparation_spec_id IS NOT NULL,placement_kind,placement_target`, f.env)
	if err != nil {
		t.Fatal(err)
	}
	type binding struct {
		build              bool
		kind, target, mode string
		origins            []string
	}
	actual, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (binding, error) {
		var b binding
		err := row.Scan(&b.build, &b.kind, &b.target, &b.mode, &b.origins)
		return b, err
	})
	if err != nil || len(actual) != 3 {
		t.Fatalf("registered bindings: %+v %v", actual, err)
	}
	if actual[0].build || actual[0].target != "RUNTIME_TOKEN" || actual[0].mode != "raw" || len(actual[0].origins) != 0 {
		t.Fatalf("runtime binding: %+v", actual[0])
	}
	if !actual[1].build || actual[1].target != "TOKEN" || actual[1].mode != "protected" || len(actual[1].origins) != 1 || actual[1].origins[0] != "https://api.example.com" {
		t.Fatalf("protected build binding: %+v", actual[1])
	}
	if !actual[2].build || actual[2].kind != "file" || actual[2].target != "/etc/build/token" || actual[2].mode != "raw" {
		t.Fatalf("file build binding: %+v", actual[2])
	}

}
