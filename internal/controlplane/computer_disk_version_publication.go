package controlplane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/jackc/pgx/v5/pgtype"
)

func requireRuntimeComputerRoot(ctx context.Context, q db.Querier, runtime db.ComputerInstance, environmentID, computerID pgtype.UUID, publicationKey []byte, root disk.GenerationRoot) error {
	locator, err := root.Locator(runtime.ReservedGuestEphemeralDiskBytes)
	if err != nil {
		return err
	}
	object, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: environmentID, ComputerID: computerID, Digest: root.Pack.Digest})
	if err != nil {
		return err
	}
	if !object.Certified.Bool {
		return errors.New("computer root is not certified")
	}
	var evidence blockformat.ObjectInspection
	if err := json.Unmarshal(object.Inspection, &evidence); err != nil {
		return err
	}
	if evidence.Pack == nil {
		return errors.New("computer root is not an inspected pack")
	}
	if err := evidence.Pack.CheckRoot(locator, root.LogicalBytes); err != nil {
		return err
	}
	_, err = q.RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: runtime.ID, PublicationKey: publicationKey, InstanceDesiredVersion: runtime.DesiredVersion, Digest: root.Pack.Digest})
	return err
}
