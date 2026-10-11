package agent

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
)

type registeredSecretBinding struct {
	Computer    *uuid.UUID `json:"computer_id"`
	Preparation *uuid.UUID `json:"preparation_spec_id"`
	Deployment  *uuid.UUID `json:"deployment_id"`
	Definition  *string    `json:"definition_key"`
	Secret      string     `json:"secret_id"`
	Kind        string     `json:"placement_kind"`
	Target      string     `json:"placement_target"`
	Mode        string     `json:"mode"`
	Origins     []string   `json:"allowed_origins"`
	Placeholder string     `json:"placeholder"`
}

func registrationSecretBindings(refs []secretbinding.Reference, preparation, deployment *uuid.UUID, definition *string) ([]registeredSecretBinding, error) {
	placements, err := secretbinding.NormalizeReferences(refs)
	if err != nil {
		return nil, err
	}
	result := make([]registeredSecretBinding, 0, len(placements))
	for _, p := range placements {
		origins := p.AllowedOrigins
		if origins == nil {
			origins = []string{}
		}
		placeholder, err := secretbinding.Placeholder(p.Mode)
		if err != nil {
			return nil, err
		}
		result = append(result, registeredSecretBinding{Computer: nil, Preparation: preparation, Deployment: deployment, Definition: definition, Secret: p.Name, Kind: p.Kind, Target: p.Target, Mode: p.Mode, Origins: origins, Placeholder: placeholder})
	}
	return result, nil
}

// The Environment lock serializes definition registration. A repeated immutable
// preparation cannot replace its existing placement or origin policy.
func registerSecretBindings(ctx context.Context, tx pgx.Tx, env uuid.UUID, bindings []registeredSecretBinding) error {
	if len(bindings) == 0 {
		return nil
	}
	raw, err := json.Marshal(bindings)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO computer_secret_bindings AS existing(environment_id,computer_id,preparation_spec_id,deployment_id,definition_key,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder)
 SELECT $1,v.computer_id,v.preparation_spec_id,v.deployment_id,v.definition_key,v.secret_id,v.placement_kind,v.placement_target,v.mode,v.allowed_origins,NULLIF(v.placeholder,'')
 FROM jsonb_to_recordset($2::jsonb) AS v(computer_id uuid,preparation_spec_id uuid,deployment_id uuid,definition_key text,secret_id uuid,placement_kind text,placement_target text,mode text,allowed_origins text[],placeholder text)
 ON CONFLICT (environment_id,preparation_spec_id,deployment_id,definition_key,computer_id,placement_kind,placement_target)
 DO UPDATE SET secret_id=EXCLUDED.secret_id WHERE existing.secret_id=EXCLUDED.secret_id AND existing.mode=EXCLUDED.mode AND existing.allowed_origins=EXCLUDED.allowed_origins`, env, raw)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != int64(len(bindings)) {
		return ErrConflict
	}
	return nil
}

type preparationBinding struct {
	Secret             uuid.UUID
	Kind, Target, Mode string
	Origins            []string
}

func readPreparationBindings(ctx context.Context, tx pgx.Tx, env, spec uuid.UUID) ([]preparationBinding, error) {
	rows, err := tx.Query(ctx, `SELECT secret_id,placement_kind,placement_target,mode,allowed_origins FROM computer_secret_bindings WHERE environment_id=$1 AND preparation_spec_id=$2 ORDER BY placement_kind,placement_target`, env, spec)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (preparationBinding, error) {
		var b preparationBinding
		err := row.Scan(&b.Secret, &b.Kind, &b.Target, &b.Mode, &b.Origins)
		return b, err
	})
}

func copyDefinitionSecretBindings(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, definition string, computer uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT secret_id::text,placement_kind,placement_target,mode,allowed_origins FROM computer_secret_bindings WHERE environment_id=$1 AND deployment_id=$2 AND definition_key=$3 ORDER BY placement_kind,placement_target`, env, deployment, definition)
	if err != nil {
		return err
	}
	bindings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (registeredSecretBinding, error) {
		b := registeredSecretBinding{Computer: &computer}
		err := row.Scan(&b.Secret, &b.Kind, &b.Target, &b.Mode, &b.Origins)
		if err == nil {
			b.Placeholder, err = secretbinding.Placeholder(b.Mode)
		}
		return b, err
	})
	if err != nil {
		return err
	}
	return registerSecretBindings(ctx, tx, env, bindings)
}
