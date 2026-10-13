package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
)

// registerTemplateDefinitions records verified deployment inputs in the caller's
// registration transaction. The caller must admit the exact Program and all seed
// objects before calling; this operation does not turn producer metadata into proof.
// The caller holds the Environment lock before Secret and preparation-spec locks.
func registerTemplateDefinitions(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, output artifact.ProgramOutput) error {
	if env == uuid.Nil() || deployment == uuid.Nil() {
		return ErrInvalidInput
	}
	specs, err := artifact.BuildComputerPreparationSpecs(output)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	manifests := make(map[string]*definition.TemplateManifest, len(specs))
	var refs []secretbinding.Reference
	for _, d := range output.Metadata.Definitions {
		if d.Template == nil {
			continue
		}
		manifests[d.DeclaredID] = d.Template
		refs = append(refs, d.Template.Secrets...)
		refs = append(refs, d.Template.BuildSecrets...)
	}
	resolved, err := resolveSecretReferences(ctx, tx, env, refs)
	if err != nil {
		return err
	}

	type input struct {
		ID           uuid.UUID       `json:"id"`
		TemplateID   uuid.UUID       `json:"template_id"`
		Digest       string          `json:"digest"`
		Definition   string          `json:"definition"`
		Spec         json.RawMessage `json:"spec"`
		Seed         json.RawMessage `json:"seed"`
		BuildRefs    json.RawMessage `json:"build_refs"`
		CPUMillis    int64           `json:"cpu_millis"`
		MemoryBytes  int64           `json:"memory_bytes"`
		DiskBytes    *int64          `json:"disk_bytes"`
		RuntimeRefs  json.RawMessage `json:"runtime_refs"`
		Every        *int64          `json:"every_ms"`
		MaxAge       *int64          `json:"max_age_ms"`
		DeleteUnused *int64          `json:"delete_unused_ms"`
		DeleteNever  bool            `json:"delete_never"`
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
		m := manifests[spec.TemplateDefinitionID]
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
		if definition.ValidateResourcesManifest(m.Resources) != nil || m.Resources.MemoryMiB > math.MaxInt64/(1<<20) || (m.Resources.DiskMiB != nil && *m.Resources.DiskMiB > math.MaxInt64/(1<<20)) {
			return ErrInvalidInput
		}
		var diskBytes *int64
		if m.Resources.DiskMiB != nil {
			value := *m.Resources.DiskMiB * (1 << 20)
			diskBytes = &value
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
		inputs = append(inputs, input{ID: uuid.NewV7(), TemplateID: uuid.NewV7(), Digest: digest, Definition: spec.TemplateDefinitionID, Spec: raw, Seed: seed, BuildRefs: buildRefs, CPUMillis: m.Resources.MilliCPU, MemoryBytes: m.Resources.MemoryMiB * (1 << 20), DiskBytes: diskBytes, RuntimeRefs: runtimeRefs, Every: every, MaxAge: maxAge, DeleteUnused: m.DeleteAfterUnusedMs, DeleteNever: m.DeleteAfterUnusedNever})
		digests = append(digests, digest)
	}
	if len(inputs) == 0 {
		return nil
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO templates(environment_id,id,key)
 SELECT $1,v.template_id,v.definition FROM jsonb_to_recordset($2::jsonb) AS v(template_id uuid,definition text)
 ON CONFLICT(environment_id,key) DO NOTHING`, env, raw); err != nil {
		return err
	}
	templateRows, err := tx.Query(ctx, `SELECT key,id FROM templates WHERE environment_id=$1`, env)
	if err != nil {
		return err
	}
	templates, err := pgx.CollectRows(templateRows, func(row pgx.CollectableRow) (struct {
		key string
		id  uuid.UUID
	}, error) {
		var value struct {
			key string
			id  uuid.UUID
		}
		err := row.Scan(&value.key, &value.id)
		return value, err
	})
	if err != nil {
		return err
	}
	templateIDs := make(map[string]uuid.UUID, len(templates))
	for _, value := range templates {
		templateIDs[value.key] = value.id
	}
	for i := range inputs {
		inputs[i].TemplateID = templateIDs[inputs[i].Definition]
	}
	raw, err = json.Marshal(inputs)
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
		bindings, err := registrationSecretBindings(refs, resolved, &specID, nil, nil)
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
		bindings, err = registrationSecretBindings(refs, resolved, nil, &deployment, &want.TemplateID)
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
	inserted, err := tx.Exec(ctx, `INSERT INTO template_definitions(environment_id,deployment_id,template_id,preparation_spec_id,cpu_millis,memory_bytes,disk_bytes,refresh_every_ms,max_image_age_ms,delete_after_unused_ms,delete_after_unused_never)
 SELECT $1,$2,v.template_id,s.id,v.cpu_millis,v.memory_bytes,v.disk_bytes,v.every_ms,v.max_age_ms,v.delete_unused_ms,v.delete_never
 FROM jsonb_to_recordset($3::jsonb) AS v(template_id uuid,definition text,digest text,cpu_millis bigint,memory_bytes bigint,disk_bytes bigint,runtime_refs jsonb,every_ms bigint,max_age_ms bigint,delete_unused_ms bigint,delete_never boolean)
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
	if err := registerTemplateDefinitions(ctx, tx, env, deployment, output); err != nil {
		return err
	}

	type agentInput struct {
		Name       string    `json:"name"`
		Computer   string    `json:"computer"`
		Setup      bool      `json:"setup"`
		MaxTurn    *int64    `json:"max_turn_ms"`
		CloseIdle  *int64    `json:"close_idle_ms"`
		CloseNever bool      `json:"close_never"`
		ID         uuid.UUID `json:"id"`
	}
	type triggerInput struct {
		Agent       string  `json:"agent"`
		Key         string  `json:"key"`
		Cron        string  `json:"cron"`
		Timezone    string  `json:"timezone"`
		Input       []byte  `json:"input"`
		Destination *string `json:"slack_destination"`
	}
	inputs := make([]agentInput, 0, len(output.Metadata.Definitions))
	triggers := []triggerInput{}
	for _, d := range output.Metadata.Definitions {
		if d.Agent != nil {
			for key, trigger := range d.Agent.Triggers {
				destination, err := definition.SlackChannelReference(trigger.Slack)
				if err != nil {
					return fmt.Errorf("%w: agent %q trigger %q: %v", ErrInvalidInput, d.DeclaredID, key, err)
				}
				triggers = append(triggers, triggerInput{Agent: d.DeclaredID, Key: key, Cron: trigger.Cron, Timezone: trigger.Timezone, Input: trigger.Input, Destination: destination})
			}
			inputs = append(inputs, agentInput{Name: d.DeclaredID, Computer: d.Agent.TemplateDefinitionID, Setup: d.Agent.Setup, MaxTurn: d.Agent.MaxTurnDurationMs, CloseIdle: d.Agent.CloseAfterIdleMs, CloseNever: d.Agent.CloseAfterIdleNever, ID: uuid.NewV7()})
		}
	}
	if len(inputs) == 0 {
		return nil
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agents(environment_id,id,key)
 SELECT $1,v.id,v.name FROM jsonb_to_recordset($2::jsonb) AS v(id uuid,name text)
 ON CONFLICT(environment_id,key) DO NOTHING`, env, raw); err != nil {
		return err
	}
	inserted, err := tx.Exec(ctx, `INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,setup,max_turn_duration_ms,close_after_idle_ms,close_after_idle_never)
 SELECT $1,a.id,$2,t.id,v.setup,v.max_turn_ms,v.close_idle_ms,v.close_never
 FROM jsonb_to_recordset($3::jsonb) AS v(name text,computer text,setup boolean,max_turn_ms bigint,close_idle_ms bigint,close_never boolean)
 JOIN agents a ON a.environment_id=$1 AND a.key=v.name
 JOIN templates t ON t.environment_id=$1 AND t.key=v.computer`, env, deployment, raw)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != int64(len(inputs)) {
		return errors.New("agent registration did not record every definition")
	}
	if len(triggers) == 0 {
		return nil
	}
	rawTriggers, err := json.Marshal(triggers)
	if err != nil {
		return err
	}
	inserted, err = tx.Exec(ctx, `INSERT INTO agent_triggers(environment_id,agent_id,deployment_id,key,cron,timezone,input,slack_destination)
 SELECT $1,a.id,$2,v.key,v.cron,v.timezone,decode(v.input,'base64'),v.slack_destination
 FROM jsonb_to_recordset($3::jsonb) AS v(agent text,key text,cron text,timezone text,input text,slack_destination text)
 JOIN agents a ON a.environment_id=$1 AND a.key=v.agent`, env, deployment, rawTriggers)
	if err != nil {
		return err
	}
	if inserted.RowsAffected() != int64(len(triggers)) {
		return errors.New("agent registration did not record every trigger")
	}
	return nil
}
