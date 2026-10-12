package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
)

// ErrSecretUnavailable means a declared Secret is missing or revoked in the Environment.
var ErrSecretUnavailable = errors.New("secret is unavailable in the environment")

// Resolve only at new admission, under the Environment lock. SHARE conflicts
// with revocation's status update, so admission cannot race past a revoke.
func resolveSecretReferences(ctx context.Context, tx pgx.Tx, env uuid.UUID, refs []secretbinding.Reference) (map[string]uuid.UUID, error) {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Secret)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	resolved := make(map[string]uuid.UUID, len(names))
	if len(names) == 0 {
		return resolved, nil
	}
	rows, err := tx.Query(ctx, `SELECT name,id FROM secrets WHERE environment_id=$1 AND name=ANY($2::text[]) AND status='active' ORDER BY id FOR SHARE`, env, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var id uuid.UUID
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		resolved[name] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(resolved) != len(names) {
		return nil, ErrSecretUnavailable
	}
	return resolved, nil
}

type registeredSecretBinding struct {
	Computer    *uuid.UUID `json:"computer_id"`
	Preparation *uuid.UUID `json:"preparation_spec_id"`
	Deployment  *uuid.UUID `json:"deployment_id"`
	Template    *uuid.UUID `json:"template_id"`
	Secret      string     `json:"secret_id"`
	Kind        string     `json:"placement_kind"`
	Target      string     `json:"placement_target"`
	Mode        string     `json:"mode"`
	Origins     []string   `json:"allowed_origins"`
	Placeholder string     `json:"placeholder"`
}

func registrationSecretBindings(refs []secretbinding.Reference, resolved map[string]uuid.UUID, preparation, deployment *uuid.UUID, template *uuid.UUID) ([]registeredSecretBinding, error) {
	placements, err := secretbinding.NormalizeReferences(refs)
	if err != nil {
		return nil, err
	}
	result := make([]registeredSecretBinding, 0, len(placements))
	for _, p := range placements {
		if resolved[p.Name] == uuid.Nil() {
			return nil, ErrSecretUnavailable
		}
		origins := p.AllowedOrigins
		if origins == nil {
			origins = []string{}
		}
		placeholder, err := secretbinding.Placeholder(p.Mode)
		if err != nil {
			return nil, err
		}
		result = append(result, registeredSecretBinding{Computer: nil, Preparation: preparation, Deployment: deployment, Template: template, Secret: resolved[p.Name].String(), Kind: p.Kind, Target: p.Target, Mode: p.Mode, Origins: origins, Placeholder: placeholder})
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
	tag, err := tx.Exec(ctx, `INSERT INTO computer_secret_bindings AS existing(environment_id,computer_id,preparation_spec_id,deployment_id,template_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder)
 SELECT $1,v.computer_id,v.preparation_spec_id,v.deployment_id,v.template_id,v.secret_id,v.placement_kind,v.placement_target,v.mode,v.allowed_origins,NULLIF(v.placeholder,'')
 FROM jsonb_to_recordset($2::jsonb) AS v(computer_id uuid,preparation_spec_id uuid,deployment_id uuid,template_id uuid,secret_id uuid,placement_kind text,placement_target text,mode text,allowed_origins text[],placeholder text)
 ON CONFLICT (environment_id,preparation_spec_id,deployment_id,template_id,computer_id,placement_kind,placement_target)
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

func copyDefinitionSecretBindings(ctx context.Context, tx pgx.Tx, env, deployment uuid.UUID, template uuid.UUID, computer uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT secret_id::text,placement_kind,placement_target,mode,allowed_origins FROM computer_secret_bindings WHERE environment_id=$1 AND deployment_id=$2 AND template_id=$3 ORDER BY placement_kind,placement_target`, env, deployment, template)
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
