package computer

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// checkpointPublication is the authority that records a capture's disk
// objects: its checkpoint source is locked and fresh, its manifest is
// registered and its members are still checkpointing. It compares no claim
// versions.
type checkpointPublication struct {
	tx     pgx.Tx
	ref    CheckpointRef
	source checkpointSource
}

// lockCheckpointPublication locks the checkpoint source and checks capture
// freshness, the registered manifest and the member set.
func lockCheckpointPublication(ctx context.Context, tx pgx.Tx, ref CheckpointRef) (checkpointPublication, error) {
	source, err := lockCheckpointSource(ctx, tx, ref)
	if err != nil {
		return checkpointPublication{}, err
	}
	instance, cp := source.instance, source.checkpoint
	q := db.New(tx)
	if err = source.checkLive(ctx, ref); err != nil {
		return checkpointPublication{}, err
	}
	if _, err = q.RequireRegisteredCheckpointManifest(ctx, db.RequireRegisteredCheckpointManifestParams{ID: cp.ID, Manifest: cp.Manifest}); err != nil {
		return checkpointPublication{}, err
	}
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: instance.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		return checkpointPublication{}, err
	}
	if err = source.checkMembers(ctx, len(members)); err != nil {
		return checkpointPublication{}, err
	}
	return checkpointPublication{tx: tx, ref: ref, source: source}, nil
}

// recheck re-evaluates the whole authority after blocking object writes,
// before commit.
func (p checkpointPublication) recheck(ctx context.Context) error {
	_, err := lockCheckpointPublication(ctx, p.tx, p.ref)
	return err
}

// objects is the checkpoint's object scope: the Instance's retained source
// keys and its pinned write key.
func (p checkpointPublication) objects(ctx context.Context) (objectScope, error) {
	instance := p.source.instance
	q := db.New(p.tx)
	keys, err := q.ListInstanceComputerSourceKeys(ctx, instance.ID)
	if err != nil {
		return objectScope{}, err
	}
	allowed := make(map[string]bool, len(keys)+1)
	for _, key := range keys {
		allowed[pgvalue.UUIDString(key.ID)] = true
	}
	write, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: instance.ID, EnvironmentID: instance.EnvironmentID, ComputerID: instance.ComputerID})
	if err != nil {
		return objectScope{}, err
	}
	if !instance.WriteKeyID.Valid || instance.WriteKeyID != write.ID {
		return objectScope{}, objectConflict("instance write key is not pinned")
	}
	allowed[pgvalue.UUIDString(write.ID)] = true
	return objectScope{
		objectRetention: objectRetention{environmentID: instance.EnvironmentID, computerID: instance.ComputerID, instanceID: instance.ID, desiredVersion: instance.DesiredVersion, key: checkpointPublicationKey(pgvalue.MustUUIDValue(p.source.checkpoint.ID))},
		orgID:           instance.OrgID, projectID: instance.ProjectID, logicalBytes: instance.ReservedGuestEphemeralDiskBytes, allowedKeys: allowed,
	}, nil
}

// RegisterCheckpointObject registers a capture's disk object before its
// upload, in one transaction that rechecks the checkpoint authority after
// recording.
func (p Publisher) RegisterCheckpointObject(ctx context.Context, ref CheckpointRef, inspection blockformat.ObjectInspection) error {
	return p.inCheckpointPublication(ctx, ref, func(c checkpointPublication, scope objectScope) error {
		return scope.registerObject(ctx, c.tx, inspection)
	})
}

// ReuseCheckpointObject pins an already certified object of the Computer for
// the capture.
func (p Publisher) ReuseCheckpointObject(ctx context.Context, ref CheckpointRef, inspection blockformat.ObjectInspection) error {
	return p.inCheckpointPublication(ctx, ref, func(c checkpointPublication, scope objectScope) error {
		return scope.reuseObject(ctx, c.tx, inspection)
	})
}

// CertifyCheckpointObject certifies a registered capture object after its
// upload: a first transaction verifies the exact registration and pin, object
// storage confirms the bytes outside any transaction, and a second
// transaction certifies them and rechecks the checkpoint authority. A storage
// failure reports ErrStorageUnavailable.
func (p Publisher) CertifyCheckpointObject(ctx context.Context, ref CheckpointRef, inspection blockformat.ObjectInspection) error {
	object, err := describeObject(inspection)
	if err != nil {
		return err
	}
	if err = p.inCheckpointPublication(ctx, ref, func(c checkpointPublication, scope objectScope) error {
		return scope.verifyRegistered(ctx, c.tx, inspection)
	}); err != nil {
		return err
	}
	stored, err := p.objects.Stat(ctx, object.digest)
	if err != nil {
		return storageUnavailable(err)
	}
	return p.inCheckpointPublication(ctx, ref, func(c checkpointPublication, scope objectScope) error {
		return scope.certifyObject(ctx, c.tx, inspection, stored)
	})
}

// inCheckpointPublication runs fn in one transaction under the checkpoint
// publication authority and its object scope, then rechecks the authority.
func (p Publisher) inCheckpointPublication(ctx context.Context, ref CheckpointRef, fn func(checkpointPublication, objectScope) error) error {
	err := db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		c, err := lockCheckpointPublication(ctx, tx, ref)
		if err != nil {
			return err
		}
		scope, err := c.objects(ctx)
		if err != nil {
			return err
		}
		if err = fn(c, scope); err != nil {
			return err
		}
		return c.recheck(ctx)
	})
	return authorityChanged(err)
}
