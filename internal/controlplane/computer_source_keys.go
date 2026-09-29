package controlplane

import (
	"bytes"
	"context"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type computerSourceKeys struct {
	VersionID, Scope, WriteKeyID string
	Root                         disk.GenerationRoot
	Keys                         []computerKeyMaterial
}

func (s *computerSourceKeys) clear() {
	for _, k := range s.Keys {
		clear(k.Key)
	}
}

// source pins the Runtime write key and returns the retained generation's read keys.
// Provider calls run outside SQL locks. This does not grant runtime
// execution or publication. The caller owns clearing every returned plaintext.
func (b *computerKeyBroker) source(ctx context.Context, f computerKeyFence) (_ computerSourceKeys, retErr error) {
	source, rows, err := b.sourceEnvelopes(ctx, f)
	if err != nil {
		return computerSourceKeys{}, err
	}
	defer func() {
		if retErr != nil {
			source.clear()
		}
	}()
	for _, row := range rows {
		id := pgvalue.UUIDString(row.ID)
		key, err := b.wrapper.Unwrap(ctx, source.Scope, id, computerkey.Envelope{WrappingKeyID: row.WrappingKeyID, Ciphertext: row.WrappedKey})
		if err != nil || len(key) != computerkey.Size {
			clear(key)
			return computerSourceKeys{}, errComputerKeyUnavailable
		}
		source.Keys = append(source.Keys, computerKeyMaterial{Scope: source.Scope, ID: id, Key: key})
	}
	current, currentRows, err := b.sourceEnvelopes(ctx, f)
	if err != nil || current.VersionID != source.VersionID || current.Scope != source.Scope || current.WriteKeyID != source.WriteKeyID || current.Root != source.Root || len(currentRows) != len(rows) {
		return computerSourceKeys{}, errComputerKeyUnavailable
	}
	for i, row := range rows {
		now := currentRows[i]
		if now.ID != row.ID || now.WrappingKeyID != row.WrappingKeyID || !bytes.Equal(now.WrappedKey, row.WrappedKey) {
			return computerSourceKeys{}, errComputerKeyUnavailable
		}
	}
	return source, nil
}

func (b *computerKeyBroker) sourceEnvelopes(ctx context.Context, f computerKeyFence) (computerSourceKeys, []db.ComputerDataKey, error) {
	var source computerSourceKeys
	var envelopes []db.ComputerDataKey
	err := inTxWith(ctx, b.tx, func(work *txWork) error {
		tx := work.tx
		authority, err := dispatch.LockComputerSourcePreparation(ctx, tx, f.ComputerPreparationFence)
		if err != nil {
			return errComputerKeyUnavailable
		}
		var claims bool
		err = tx.QueryRow(ctx, `SELECT w.claim_version=$3 AND g.claim_version=$4 FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1 AND g.id=$2`, f.WorkerID, f.WorkerGroupID, f.ClaimVersion, f.GroupClaimVersion).Scan(&claims)
		if err != nil || !claims {
			return errComputerKeyUnavailable
		}
		q := db.New(tx)
		retained, root, keys, err := loadRuntimeComputerGeneration(ctx, q, f.RuntimeID)
		if err != nil || retained.VersionID != authority.VersionID || root.LogicalBytes != authority.LogicalBytes {
			return errComputerKeyUnavailable
		}

		writeKey, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: f.RuntimeID, EnvironmentID: authority.EnvironmentID, ComputerID: authority.ComputerID})
		if err != nil {
			return errComputerKeyUnavailable
		}
		n, err := q.PinRuntimeComputerKey(ctx, db.PinRuntimeComputerKeyParams{KeyID: writeKey.ID, ComputerInstanceID: f.RuntimeID, EnvironmentID: authority.EnvironmentID, ComputerID: authority.ComputerID})
		if err != nil || n != 1 {
			return errComputerKeyUnavailable
		}
		found := false
		for _, k := range keys {
			found = found || k.ID == writeKey.ID
		}
		if !found {
			keys = append(keys, writeKey)
		}
		scope, err := disk.EncryptionScope(pgvalue.UUIDString(authority.OrgID), pgvalue.UUIDString(authority.EnvironmentID), pgvalue.UUIDString(authority.ComputerID))
		if err != nil {
			return errComputerKeyUnavailable
		}
		if err = authority.CheckDeadlines(ctx, tx); err != nil {
			return errComputerKeyUnavailable
		}
		source = computerSourceKeys{VersionID: pgvalue.UUIDString(retained.VersionID), Scope: scope, Root: root, WriteKeyID: pgvalue.UUIDString(writeKey.ID)}
		envelopes = keys
		return nil
	})
	if err != nil {
		return computerSourceKeys{}, nil, err
	}
	return source, envelopes, nil
}
