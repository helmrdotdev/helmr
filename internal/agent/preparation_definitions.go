package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
)

// registerComputerDefinitions records verified deployment inputs in the caller's
// registration transaction. The caller must admit the exact Program and all seed
// objects before calling; this operation does not turn producer metadata into proof.
// The caller holds the Environment lock before Secret and preparation-spec locks.
func registerComputerDefinitions(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, output artifact.ProgramOutput) error {
	if env == uuid.Nil() || deployment == uuid.Nil() {
		return ErrInvalidInput
	}
	specs, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	var locked uuid.UUID
	manifests := make(map[string]*definition.ComputerManifest, len(specs))
	secretIDs := make(map[uuid.UUID]struct{})
	for _, d := range output.Metadata.Definitions {
		if d.Computer == nil {
			continue
		}
		manifests[d.DeclaredID] = d.Computer
		for _, refs := range [][]definition.SecretBinding{d.Computer.Secrets, d.Computer.BuildSecrets} {
			for _, ref := range refs {
				secretIDs[uuid.MustParse(ref.SecretID)] = struct{}{}
			}
		}
	}
	ids := make([]uuid.UUID, 0, len(secretIDs))
	for id := range secretIDs {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	if len(ids) > 0 {
		// Lock the complete stable-reference set in one ordered query. Revoked
		// references remain registrable; execution delivery owns their activity check.
		rows, err := tx.Query(ctx, `SELECT id FROM secrets WHERE environment_id=$1 AND id=ANY($2::uuid[]) ORDER BY id FOR KEY SHARE`, env, ids)
		if err != nil {
			return err
		}
		found := 0
		for rows.Next() {
			if err := rows.Scan(&locked); err != nil {
				rows.Close()
				return err
			}
			found++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if found != len(ids) {
			return fmt.Errorf("%w: Secret reference is outside the Environment", ErrDenied)
		}
	}
	type input struct {
		ID          uuid.UUID       `json:"id"`
		Digest      string          `json:"digest"`
		Definition  string          `json:"definition"`
		Spec        json.RawMessage `json:"spec"`
		Seed        json.RawMessage `json:"seed"`
		BuildRefs   json.RawMessage `json:"build_refs"`
		Resources   json.RawMessage `json:"resources"`
		RuntimeRefs json.RawMessage `json:"runtime_refs"`
		Every       *int64          `json:"every_ms"`
		MaxAge      *int64          `json:"max_age_ms"`
	}
	inputs := make([]input, 0, len(specs))
	digests := make([]string, 0, len(specs))
	for _, spec := range specs {
		raw, err := artifact.CanonicalComputerPreparationSpec(spec)
		if err != nil {
			return err
		}
		digest, err := artifact.ComputerPreparationSpecDigest(spec)
		if err != nil {
			return err
		}
		m := manifests[spec.ComputerDefinitionID]
		seed, err := json.Marshal(m.Seed)
		if err != nil {
			return err
		}
		buildBindings, err := secretbinding.CanonicalReferences(m.BuildSecrets)
		if err != nil {
			return err
		}
		buildRefs, err := json.Marshal(buildBindings)
		if err != nil {
			return err
		}
		resources, err := json.Marshal(m.Resources)
		if err != nil {
			return err
		}
		runtimeBindings, err := secretbinding.CanonicalReferences(m.Secrets)
		if err != nil {
			return err
		}
		runtimeRefs, err := json.Marshal(runtimeBindings)
		if err != nil {
			return err
		}
		var every, maxAge *int64
		if m.Refresh != nil {
			every = &m.Refresh.EveryMs
			maxAge = m.Refresh.MaxAgeMs
		}
		inputs = append(inputs, input{ID: uuid.NewV7(), Digest: digest, Definition: spec.ComputerDefinitionID, Spec: raw, Seed: seed, BuildRefs: buildRefs, Resources: resources, RuntimeRefs: runtimeRefs, Every: every, MaxAge: maxAge})
		digests = append(digests, digest)
	}
	if len(inputs) == 0 {
		return nil
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed)
 SELECT $1,v.id,v.digest,v.spec,v.seed FROM jsonb_to_recordset($2::jsonb) AS v(id uuid,digest text,spec jsonb,seed jsonb,build_refs jsonb)
 ON CONFLICT(environment_id,spec_digest) DO NOTHING`, env, raw); err != nil {
		return err
	}
	expected := make(map[string]input, len(inputs))
	for _, i := range inputs {
		expected[i.Digest] = i
	}
	rows, err := tx.Query(ctx, `SELECT id,spec_digest,spec,seed FROM computer_preparation_specs WHERE environment_id=$1 AND spec_digest=ANY($2::text[]) ORDER BY spec_digest FOR KEY SHARE`, env, digests)
	if err != nil {
		return err
	}
	found := 0
	specIDs := make([]uuid.UUID, 0, len(inputs))
	buildBindings := make([]registeredSecretBinding, 0)
	runtimeBindings := make([]registeredSecretBinding, 0)
	for rows.Next() {
		var specID uuid.UUID
		var digest string
		var stored, storedSeed []byte
		if err := rows.Scan(&specID, &digest, &stored, &storedSeed); err != nil {
			rows.Close()
			return err
		}
		want, ok := expected[digest]
		if !ok {
			rows.Close()
			return errors.New("unexpected registered preparation spec")
		}
		// A reused identity cannot silently overwrite or accept different projections.
		for _, pair := range [][2][]byte{{want.Spec, stored}, {want.Seed, storedSeed}} {
			expected, err := jsoncanon.Transform(pair[0])
			if err != nil {
				rows.Close()
				return err
			}
			actual, err := jsoncanon.Transform(pair[1])
			if err != nil {
				rows.Close()
				return err
			}
			if !bytes.Equal(expected, actual) {
				rows.Close()
				return errors.New("registered preparation spec has different content")
			}
		}
		specIDs = append(specIDs, specID)
		var refs []secretbinding.Reference
		if err := json.Unmarshal(want.BuildRefs, &refs); err != nil {
			rows.Close()
			return err
		}
		bindings, err := registrationSecretBindings(refs, &specID, nil, nil)
		if err != nil {
			rows.Close()
			return err
		}
		buildBindings = append(buildBindings, bindings...)
		refs = nil
		if err := json.Unmarshal(want.RuntimeRefs, &refs); err != nil {
			rows.Close()
			return err
		}
		bindings, err = registrationSecretBindings(refs, nil, &deployment, &want.Definition)
		if err != nil {
			rows.Close()
			return err
		}
		runtimeBindings = append(runtimeBindings, bindings...)
		found++
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if found != len(inputs) {
		return errors.New("preparation registration did not record every spec")
	}
	if err := registerSecretBindings(ctx, tx, env, buildBindings); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM computer_secret_bindings WHERE environment_id=$1 AND preparation_spec_id=ANY($2::uuid[])`, env, specIDs).Scan(&count); err != nil {
		return err
	}
	if count != len(buildBindings) {
		return ErrConflict
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO computer_definitions(environment_id,deployment_id,definition_key,preparation_spec_id,resources,refresh_every_ms,max_image_age_ms)
 SELECT $1,$2,v.definition,s.id,v.resources,v.every_ms,v.max_age_ms
 FROM jsonb_to_recordset($3::jsonb) AS v(definition text,digest text,resources jsonb,runtime_refs jsonb,every_ms bigint,max_age_ms bigint)
 JOIN computer_preparation_specs s ON s.environment_id=$1 AND s.spec_digest=v.digest`, env, deployment, raw)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != int64(len(inputs)) {
		return errors.New("computer registration did not record every definition")
	}
	return registerSecretBindings(ctx, tx, env, runtimeBindings)
}

// RegisterProgramDefinitions records the Computer and Agent definitions of an
// admitted Program atomically in the caller's deployment transaction. Stable
// Agent identities survive deployments; their definitions remain pinned to one
// deployment. The caller must hold the Environment lock and verify Program and
// seed bytes before registration.
func RegisterProgramDefinitions(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, output artifact.ProgramOutput) error {
	if err := registerComputerDefinitions(ctx, tx, env, deployment, output); err != nil {
		return err
	}

	type agentInput struct {
		Name      string    `json:"name"`
		Computer  string    `json:"computer"`
		Setup     bool      `json:"setup"`
		Triggers  []byte    `json:"triggers"`
		MaxTurn   *int64    `json:"max_turn_ms"`
		CloseIdle *int64    `json:"close_idle_ms"`
		ID        uuid.UUID `json:"id"`
	}
	inputs := make([]agentInput, 0, len(output.Metadata.Definitions))
	for _, d := range output.Metadata.Definitions {
		if d.Agent != nil {
			triggers, err := json.Marshal(d.Agent.Triggers)
			if err != nil {
				return err
			}
			inputs = append(inputs, agentInput{Name: d.DeclaredID, Computer: d.Agent.ComputerDefinitionID, Setup: d.Agent.Setup, Triggers: triggers, MaxTurn: d.Agent.MaxTurnDurationMs, CloseIdle: d.Agent.CloseAfterIdleMs, ID: uuid.NewV7()})
		}
	}
	if len(inputs) == 0 {
		return nil
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agents(environment_id,id,name)
 SELECT $1,v.id,v.name FROM jsonb_to_recordset($2::jsonb) AS v(id uuid,name text)
 ON CONFLICT(environment_id,name) DO NOTHING`, env, raw); err != nil {
		return err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,definition_key,computer_definition_key,setup,triggers,max_turn_duration_ms,close_after_idle_ms)
 SELECT $1,a.id,$2,v.name,v.computer,v.setup,decode(v.triggers,'base64'),v.max_turn_ms,v.close_idle_ms
 FROM jsonb_to_recordset($3::jsonb) AS v(name text,computer text,setup boolean,triggers text,max_turn_ms bigint,close_idle_ms bigint)
 JOIN agents a ON a.environment_id=$1 AND a.name=v.name`, env, deployment, raw)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != int64(len(inputs)) {
		return errors.New("agent registration did not record every definition")
	}
	return nil
}
