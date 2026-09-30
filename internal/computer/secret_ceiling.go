package computer

import (
	"context"
	"slices"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5/pgtype"
)

// secretAuthority describes effective use of one stable scoped Secret ID.
type secretAuthority struct {
	ID      string
	Mode    string
	Origins []string
}

// allowsSecretAuthority reports whether source's bindings cover every target
// binding: a raw source binding covers any use of its Secret, protected
// source bindings cover a protected use restricted to their origins.
func allowsSecretAuthority(source, target []secretAuthority) bool {
	for _, wanted := range target {
		allowed := false
		origins := map[string]bool{}
		for _, grant := range source {
			if grant.ID != wanted.ID {
				continue
			}
			if grant.Mode == "raw" {
				allowed = true
				break
			}
			if grant.Mode == "protected" {
				for _, o := range grant.Origins {
					origins[o] = true
				}
			}
		}
		if allowed {
			continue
		}
		if wanted.Mode != "protected" || len(wanted.Origins) == 0 {
			return false
		}
		for _, o := range wanted.Origins {
			if !origins[o] {
				return false
			}
		}
	}
	return true
}

func computerSecretAuthority(ctx context.Context, q db.Querier, id pgtype.UUID) ([]secretAuthority, error) {
	rows, err := q.ListComputerSecrets(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]secretAuthority, 0, len(rows))
	for _, row := range rows {
		result = append(result, secretAuthority{ID: pgvalue.UUIDString(row.SecretID), Mode: row.Mode, Origins: row.AllowedOrigins})
	}
	return result, nil
}

// CheckSecretTarget returns ErrSecretUnavailable unless the source
// Computer's secret authority covers every binding of the target Computer.
// Bindings are immutable, so this takes no lock; live source authority is
// checked by the caller in the same transaction, including on replay. The
// ceiling does not add a Secret ACL.
func CheckSecretTarget(ctx context.Context, q db.Querier, sourceID, targetID pgtype.UUID) error {
	source, err := computerSecretAuthority(ctx, q, sourceID)
	if err != nil {
		return err
	}
	target, err := computerSecretAuthority(ctx, q, targetID)
	if err != nil {
		return err
	}
	if !allowsSecretAuthority(source, target) {
		return ErrSecretUnavailable
	}
	return nil
}

// LockSecretsWithinCeiling locks the active Secrets that requested bindings
// name, in name order, and returns ErrSecretUnavailable when one is missing
// or the source Computer's secret authority does not cover the bindings. A
// run-sourced creation takes these Secret locks before the live source Run
// authority, including on replay.
func LockSecretsWithinCeiling(ctx context.Context, q db.Querier, sourceID, environmentID pgtype.UUID, requested []secretbinding.Binding) error {
	placements, err := secretbinding.NormalizedPlacements(requested)
	if err != nil {
		return invalidCreate(err)
	}
	source, err := computerSecretAuthority(ctx, q, sourceID)
	if err != nil {
		return err
	}
	names := []string{}
	for _, p := range placements {
		if !slices.Contains(names, p.Name) {
			names = append(names, p.Name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return nil
	}
	rows, err := q.LockActiveSecretsByNameForComputerCreate(ctx, db.LockActiveSecretsByNameForComputerCreateParams{EnvironmentID: environmentID, Names: names})
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for _, row := range rows {
		ids[row.Name] = pgvalue.UUIDString(row.ID)
	}
	target := []secretAuthority{}
	for _, p := range placements {
		if ids[p.Name] == "" {
			return ErrSecretUnavailable
		}
		target = append(target, secretAuthority{ID: ids[p.Name], Mode: p.Mode, Origins: p.AllowedOrigins})
	}
	if !allowsSecretAuthority(source, target) {
		return ErrSecretUnavailable
	}
	return nil
}
