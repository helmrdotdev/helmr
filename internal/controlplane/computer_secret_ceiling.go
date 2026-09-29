package controlplane

import (
	"context"
	"slices"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5/pgtype"
)

func computerSecretAuthority(ctx context.Context, q db.Querier, id pgtype.UUID) ([]disk.SecretAuthority, error) {
	rows, err := q.ListComputerSecrets(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]disk.SecretAuthority, 0, len(rows))
	for _, row := range rows {
		result = append(result, disk.SecretAuthority{ID: pgvalue.UUIDString(row.SecretID), Mode: row.Mode, Origins: row.AllowedOrigins})
	}
	return result, nil
}

// Bindings are immutable. Live source authority is checked by the caller in the
// same transaction, including replay; this ceiling does not add a Secret ACL.
func authorizeComputerSecretTarget(ctx context.Context, q db.Querier, sourceID, targetID pgtype.UUID) error {
	source, err := computerSecretAuthority(ctx, q, sourceID)
	if err != nil {
		return err
	}
	target, err := computerSecretAuthority(ctx, q, targetID)
	if err != nil {
		return err
	}
	if !disk.AllowsSecretAuthority(source, target) {
		return errComputerSecretUnavailable
	}
	return nil
}

func authorizeComputerSecretCreate(ctx context.Context, q db.Querier, sourceID, environmentID pgtype.UUID, requested []secretbinding.Binding) error {
	placements, err := normalizeComputerSecretPlacements(requested)
	if err != nil {
		return err
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
	target := []disk.SecretAuthority{}
	for _, p := range placements {
		if ids[p.Name] == "" {
			return errComputerSecretUnavailable
		}
		target = append(target, disk.SecretAuthority{ID: ids[p.Name], Mode: p.Mode, Origins: p.AllowedOrigins})
	}
	if !disk.AllowsSecretAuthority(source, target) {
		return errComputerSecretUnavailable
	}
	return nil
}
