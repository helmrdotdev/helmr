package controlplane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

// The caller holds the live Runtime and Computer authority through commit.
// Certification proves the immutable DAG; the pin proves this Runtime retained it.
func requireCertifiedComputerRoot(ctx context.Context, q db.Querier, authority runLeaseClaimAuthority, publicationKey []byte, root computer.GenerationRoot) error {
	return requireRuntimeComputerRoot(ctx, q, authority.runtime, authority.run.EnvironmentID, authority.workspace.ID, publicationKey, root)
}

func requireRuntimeComputerRoot(ctx context.Context, q db.Querier, runtime db.RuntimeInstance, environmentID, computerID pgtype.UUID, publicationKey []byte, root computer.GenerationRoot) error {
	locator, err := root.Locator(runtime.ReservedGuestEphemeralDiskBytes)
	if err != nil {
		return err
	}
	object, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: environmentID, ComputerID: computerID, Digest: root.Pack.Digest})
	if err != nil {
		return err
	}
	if !object.Certified.Bool {
		return errors.New("Computer root is not certified")
	}
	var evidence blockformat.ObjectInspection
	if err := json.Unmarshal(object.Inspection, &evidence); err != nil {
		return err
	}
	if evidence.Pack == nil {
		return errors.New("Computer root is not an inspected pack")
	}
	if err := evidence.Pack.CheckRoot(locator, root.LogicalBytes); err != nil {
		return err
	}
	_, err = q.RequireRuntimeComputerObjectPin(ctx, db.RequireRuntimeComputerObjectPinParams{RuntimeInstanceID: runtime.ID, PublicationKey: publicationKey, RuntimeDesiredVersion: runtime.DesiredVersion, Digest: root.Pack.Digest})
	return err
}

type computerVersionRootStore interface {
	CreateComputerVersionRoot(context.Context, db.CreateComputerVersionRootParams) error
}

func recordComputerVersionRoot(ctx context.Context, q computerVersionRootStore, authority runLeaseClaimAuthority, versionID pgtype.UUID, root computer.GenerationRoot) error {
	raw, err := json.Marshal(root)
	if err != nil {
		return err
	}
	return q.CreateComputerVersionRoot(ctx, db.CreateComputerVersionRootParams{EnvironmentID: authority.run.EnvironmentID, ComputerID: authority.workspace.ID, VersionID: versionID, Locator: raw})
}
