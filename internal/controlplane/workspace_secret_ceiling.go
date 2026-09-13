package controlplane

import (
	"context"
	"slices"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func workspaceSecretAuthority(ctx context.Context, q db.Querier, id pgtype.UUID) ([]workspace.SecretAuthority, error) {
	rows, err := q.ListWorkspaceSecrets(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]workspace.SecretAuthority, 0, len(rows))
	for _, row := range rows {
		result = append(result, workspace.SecretAuthority{ID: pgvalue.UUIDString(row.SecretID), Mode: row.Mode, Origins: row.AllowedOrigins})
	}
	return result, nil
}

// Bindings are immutable. Live source authority is checked by the caller in the
// same transaction, including replay; this ceiling does not add a Secret ACL.
func authorizeWorkspaceSecretTarget(ctx context.Context, q db.Querier, sourceID, targetID pgtype.UUID) error {
	source, err := workspaceSecretAuthority(ctx, q, sourceID)
	if err != nil {
		return err
	}
	target, err := workspaceSecretAuthority(ctx, q, targetID)
	if err != nil {
		return err
	}
	if !workspace.AllowsSecretAuthority(source, target) {
		return errWorkspaceSecretUnavailable
	}
	return nil
}

func authorizeWorkspaceSecretCreate(ctx context.Context, q db.Querier, sourceID, environmentID pgtype.UUID, requested []api.WorkspaceSecret) error {
	placements, err := normalizeWorkspaceSecretPlacements(requested)
	if err != nil {
		return err
	}
	source, err := workspaceSecretAuthority(ctx, q, sourceID)
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
	rows, err := q.LockActiveSecretsByNameForWorkspaceCreate(ctx, db.LockActiveSecretsByNameForWorkspaceCreateParams{EnvironmentID: environmentID, Names: names})
	if err != nil {
		return err
	}
	ids := map[string]string{}
	for _, row := range rows {
		ids[row.Name] = pgvalue.UUIDString(row.ID)
	}
	target := []workspace.SecretAuthority{}
	for _, p := range placements {
		if ids[p.Name] == "" {
			return errWorkspaceSecretUnavailable
		}
		target = append(target, workspace.SecretAuthority{ID: ids[p.Name], Mode: p.Mode, Origins: p.AllowedOrigins})
	}
	if !workspace.AllowsSecretAuthority(source, target) {
		return errWorkspaceSecretUnavailable
	}
	return nil
}
