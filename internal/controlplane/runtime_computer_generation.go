package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

// loadRuntimeComputerGeneration reads one retained generation and its complete
// certified key closure inside the caller's fenced transaction. Retention is not
// permission: this helper does not authorize Worker delivery or unwrap keys.
// Callers revalidate live authority after any external provider operation.
func loadRuntimeComputerGeneration(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) (db.GetInstanceComputerSourceRootRow, disk.GenerationRoot, []db.ComputerDataKey, error) {
	source, err := q.GetInstanceComputerSourceRoot(ctx, runtimeID)
	if err != nil {
		return source, disk.GenerationRoot{}, nil, err
	}
	root, err := disk.ParseGenerationRoot(source.Locator, source.LogicalBytes)
	if err != nil {
		return source, disk.GenerationRoot{}, nil, err
	}
	keys, err := q.ListInstanceComputerSourceKeys(ctx, runtimeID)
	if err != nil {
		return source, disk.GenerationRoot{}, nil, err
	}
	hasRootKey := false
	for _, key := range keys {
		if !key.Available.Valid || !key.Available.Bool || len(key.WrappedKey) == 0 {
			return source, disk.GenerationRoot{}, nil, errors.New("retained computer source key unavailable")
		}
		hasRootKey = hasRootKey || pgvalue.UUIDString(key.ID) == root.Page.KeyID
	}
	if !hasRootKey {
		return source, disk.GenerationRoot{}, nil, errors.New("retained computer root key missing")
	}
	return source, root, keys, nil
}
