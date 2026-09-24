package controlplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// This provider boundary is consumed only by the CP broker. The transport and
// Worker never choose an envelope, wrapping key, scope or data-key identifier.
type ComputerKeyWrapper interface {
	Wrap(context.Context, string, string, []byte) (computerkey.Envelope, error)
	Unwrap(context.Context, string, string, computerkey.Envelope) ([]byte, error)
}
type computerKeyBroker struct {
	tx      TxBeginner
	wrapper ComputerKeyWrapper
}
type computerKeyFence struct {
	dispatch.ComputerPreparationFence
	ClaimVersion, GroupClaimVersion int64
}
type computerKeyMaterial struct {
	Scope, ID string
	Key       []byte
}

var errComputerKeyUnavailable = errors.New("computer key authority is unavailable")

func newComputerKeyBroker(tx TxBeginner, wrapper ComputerKeyWrapper) (*computerKeyBroker, error) {
	if tx == nil || wrapper == nil {
		return nil, errors.New("computer key transactions and wrapping provider are required")
	}
	return &computerKeyBroker{tx: tx, wrapper: wrapper}, nil
}

// initial prepares only an initializing root, through the existing preparation
// authority. Continuation/restore key selection is a separate operation; an absent
// continuation key must never trigger fresh initialization. The returned key is
// host-only material, and its caller owns clearing it after use.
func (b *computerKeyBroker) initial(ctx context.Context, f computerKeyFence) (computerKeyMaterial, error) {
	row, scope, err := b.pinInitial(ctx, f, nil, "")
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return computerKeyMaterial{}, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Scope discovery was authorized, but no secret leaves the CP here. Provider
		// work must not hold SQL locks; a second transaction revalidates before insert.
		if scope == "" {
			return computerKeyMaterial{}, errComputerKeyUnavailable
		}
		key := make([]byte, computerkey.Size)
		if _, err = rand.Read(key); err != nil {
			return computerKeyMaterial{}, err
		}
		keyID := pgvalue.UUID(uuid.NewV7())
		envelope, wrapErr := b.wrapper.Wrap(ctx, scope, pgvalue.UUIDString(keyID), key)
		clear(key)
		if wrapErr != nil {
			return computerKeyMaterial{}, errors.New("wrap computer key")
		}
		candidate := db.ComputerDataKey{ID: keyID, WrappingKeyID: envelope.WrappingKeyID, WrappedKey: envelope.Ciphertext}
		row, scope, err = b.pinInitial(ctx, f, &candidate, scope)
		if err != nil {
			return computerKeyMaterial{}, err
		}
	}
	keyID := pgvalue.UUIDString(row.ID)
	plain, err := b.wrapper.Unwrap(ctx, scope, keyID, computerkey.Envelope{WrappingKeyID: row.WrappingKeyID, Ciphertext: row.WrappedKey})
	if err != nil {
		clear(plain)
		return computerKeyMaterial{}, errors.New("unwrap computer key")
	}
	// A successful unwrap is not a delivery grant. Revocation, expiry, cancellation
	// or a changed reservation during provider I/O must suppress the response.
	current, currentScope, err := b.pinInitial(ctx, f, nil, "")
	if err != nil || currentScope != scope || current.ID != row.ID || current.WrappingKeyID != row.WrappingKeyID || !bytes.Equal(current.WrappedKey, row.WrappedKey) || len(plain) != computerkey.Size {
		clear(plain)
		return computerKeyMaterial{}, errComputerKeyUnavailable
	}
	return computerKeyMaterial{Scope: scope, ID: keyID, Key: plain}, nil
}

// pinInitial serializes on the existing Computer/Runtime authority locks. A racing
// first fetch uses the winner's persisted key. No plaintext or provider I/O occurs
// inside the transaction, and lost replies retain the same runtime pin for retry.
func (b *computerKeyBroker) pinInitial(ctx context.Context, f computerKeyFence, candidate *db.ComputerDataKey, expectedScope string) (db.ComputerDataKey, string, error) {
	tx, err := b.tx.Begin(ctx)
	if err != nil {
		return db.ComputerDataKey{}, "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	authority, err := dispatch.LockComputerPreparation(ctx, tx, f.ComputerPreparationFence)
	if err != nil {
		return db.ComputerDataKey{}, "", errComputerKeyUnavailable
	}
	var claims bool
	err = tx.QueryRow(ctx, `SELECT w.claim_version=$3 AND g.claim_version=$4
 FROM worker_instances w JOIN worker_groups g ON g.id=w.worker_group_id
 WHERE w.id=$1 AND g.id=$2`, f.WorkerID, f.WorkerGroupID, f.ClaimVersion, f.GroupClaimVersion).Scan(&claims)
	if err != nil || !claims {
		return db.ComputerDataKey{}, "", errComputerKeyUnavailable
	}
	scope, err := computer.EncryptionScope(pgvalue.UUIDString(authority.OrgID), pgvalue.UUIDString(authority.EnvironmentID), pgvalue.UUIDString(authority.ComputerID))
	if err != nil || (expectedScope != "" && expectedScope != scope) {
		return db.ComputerDataKey{}, "", errComputerKeyUnavailable
	}
	q := db.New(tx)
	row, err := q.GetInitialComputerWriteKey(ctx, db.GetInitialComputerWriteKeyParams{RuntimeInstanceID: f.RuntimeID, EnvironmentID: authority.EnvironmentID, ComputerID: authority.ComputerID})
	if errors.Is(err, pgx.ErrNoRows) {
		// A missing row is initialization only when both authoritative pointers are
		// empty. Never replace an unavailable/corrupt persisted key with a fresh one.
		var current, runtime pgtype.UUID
		if err = tx.QueryRow(ctx, `SELECT c.write_key_id,r.computer_write_key_id FROM workspaces c JOIN runtime_instances r ON r.environment_id=c.environment_id AND r.workspace_id=c.id WHERE r.id=$1`, f.RuntimeID).Scan(&current, &runtime); err != nil {
			return db.ComputerDataKey{}, "", err
		}
		if current.Valid || runtime.Valid {
			return db.ComputerDataKey{}, "", errComputerKeyUnavailable
		}
		if candidate == nil {
			if err = authority.CheckDeadlines(ctx, tx); err != nil {
				return db.ComputerDataKey{}, "", errComputerKeyUnavailable
			}
			return db.ComputerDataKey{}, scope, pgx.ErrNoRows
		}
		row, err = q.CreateComputerKey(ctx, db.CreateComputerKeyParams{ID: candidate.ID, EnvironmentID: authority.EnvironmentID, ComputerID: authority.ComputerID, WrappingKeyID: candidate.WrappingKeyID, WrappedKey: candidate.WrappedKey})
		if err != nil {
			return db.ComputerDataKey{}, "", err
		}
		n, err := q.InitializeComputerWriteKey(ctx, db.InitializeComputerWriteKeyParams{KeyID: row.ID, EnvironmentID: row.EnvironmentID, ComputerID: row.ComputerID})
		if err != nil {
			return db.ComputerDataKey{}, "", err
		}
		if n != 1 {
			return db.ComputerDataKey{}, "", errComputerKeyUnavailable
		}
	} else if err != nil {
		return db.ComputerDataKey{}, "", err
	}
	n, err := q.PinRuntimeComputerKey(ctx, db.PinRuntimeComputerKeyParams{KeyID: row.ID, RuntimeInstanceID: f.RuntimeID, EnvironmentID: row.EnvironmentID, ComputerID: row.ComputerID})
	if err != nil {
		return db.ComputerDataKey{}, "", err
	}
	if n != 1 {
		return db.ComputerDataKey{}, "", errComputerKeyUnavailable
	}
	if err = authority.CheckDeadlines(ctx, tx); err != nil {
		return db.ComputerDataKey{}, "", errComputerKeyUnavailable
	}
	if err = tx.Commit(ctx); err != nil {
		return db.ComputerDataKey{}, "", err
	}
	return row, scope, nil
}
