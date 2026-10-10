package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ComputerKeyBroker delivers only the retained root and certified key closure
// of an authenticated Host's exact physical allocation. Wrapping credentials
// remain in the control plane; callers clear returned plaintext after use.
type ComputerKeyBroker struct {
	database db.TxBeginner
	wrapper  DataKeyWrapper
}

func NewComputerKeyBroker(database db.TxBeginner, wrapper DataKeyWrapper) (*ComputerKeyBroker, error) {
	if database == nil || wrapper == nil {
		return nil, errors.New("computer keys require a database and wrapping provider")
	}
	return &ComputerKeyBroker{database: database, wrapper: wrapper}, nil
}

type ComputerDiskKey struct {
	ID  uuid.UUID
	Key []byte
}
type ComputerDiskSource struct {
	Scope       string
	BaseVersion string
	Root        disk.VersionRoot
	WriteKeyID  uuid.UUID
	Keys        []ComputerDiskKey
}

func (s *ComputerDiskSource) Clear() {
	for _, key := range s.Keys {
		clear(key.Key)
	}
}

type computerKeyEnvelope struct {
	id       uuid.UUID
	envelope computerkey.Envelope
}
type computerDiskPin struct {
	source    ComputerDiskSource
	rootID    uuid.UUID
	envelopes []computerKeyEnvelope
}

// Source pins the write key once per physical allocation. Retry never selects
// a newer source, replaces an existing key, or renews execution authority.
// Every provider operation occurs without SQL locks and is reauthorized before
// any plaintext leaves this operation.
func (b *ComputerKeyBroker) Source(ctx context.Context, host workergroup.HostPrincipal, identity ComputerLeaseIdentity) (_ ComputerDiskSource, resultErr error) {
	pin, err := b.pin(ctx, host, identity, nil, "")
	if err != nil {
		return ComputerDiskSource{}, err
	}
	if pin.source.WriteKeyID == uuid.Nil() {
		key := make([]byte, computerkey.Size)
		if _, err := rand.Read(key); err != nil {
			return ComputerDiskSource{}, err
		}
		candidate := computerKeyEnvelope{id: uuid.NewV7()}
		candidate.envelope, err = b.wrapper.Wrap(ctx, pin.source.Scope, candidate.id.String(), key)
		clear(key)
		if err != nil {
			return ComputerDiskSource{}, err
		}
		pin, err = b.pin(ctx, host, identity, &candidate, pin.source.Scope)
		if err != nil {
			return ComputerDiskSource{}, err
		}
	}
	source := pin.source
	defer func() {
		if resultErr != nil {
			source.Clear()
		}
	}()
	for _, wrapped := range pin.envelopes {
		key, err := b.wrapper.Unwrap(ctx, source.Scope, wrapped.id.String(), wrapped.envelope)
		if err != nil {
			clear(key)
			return ComputerDiskSource{}, err
		}
		if len(key) != computerkey.Size {
			clear(key)
			return ComputerDiskSource{}, errors.New("computer key provider returned invalid key length")
		}
		source.Keys = append(source.Keys, ComputerDiskKey{ID: wrapped.id, Key: key})
	}
	current, err := b.pin(ctx, host, identity, nil, source.Scope)
	if err != nil {
		return ComputerDiskSource{}, err
	}
	if current.rootID != pin.rootID || current.source.Scope != source.Scope || current.source.BaseVersion != source.BaseVersion || current.source.Root != source.Root || current.source.WriteKeyID != source.WriteKeyID || len(current.envelopes) != len(pin.envelopes) {
		return ComputerDiskSource{}, ErrConflict
	}
	for i, k := range pin.envelopes {
		now := current.envelopes[i]
		if now.id != k.id || now.envelope.WrappingKeyID != k.envelope.WrappingKeyID || !bytes.Equal(now.envelope.Ciphertext, k.envelope.Ciphertext) {
			return ComputerDiskSource{}, ErrConflict
		}
	}
	return source, nil
}

func (b *ComputerKeyBroker) pin(ctx context.Context, host workergroup.HostPrincipal, identity ComputerLeaseIdentity, candidate *computerKeyEnvelope, expectedScope string) (computerDiskPin, error) {
	var result computerDiskPin
	if !identity.valid() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, b.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var org uuid.UUID
		var initial string
		if err := tx.QueryRow(ctx, `SELECT e.org_id,'sha256:'||encode(c.initial_root_digest,'hex') FROM computers c JOIN environments e ON e.id=c.environment_id
 WHERE c.environment_id=$1 AND c.id=$2 AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL FOR NO KEY UPDATE OF c`, identity.EnvironmentID, identity.ComputerID).Scan(&org, &initial); err != nil {
			return err
		}
		if err := checkComputerDiskAuthority(ctx, tx, host, identity); err != nil {
			return err
		}
		var err error
		result.source.Scope, err = computerkey.EncryptionScope(org.String(), identity.EnvironmentID.String())
		if err != nil {
			return err
		}
		if expectedScope != "" && expectedScope != result.source.Scope {
			return ErrConflict
		}
		var restored *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT restored_from_save_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&restored); err != nil {
			return err
		}
		var raw []byte
		var logicalBytes int64
		if restored == nil {
			result.source.BaseVersion = initial
			err = tx.QueryRow(ctx, `SELECT r.id,r.locator,r.logical_bytes FROM computers c JOIN computer_disk_roots r ON (r.environment_id,r.id)=(c.environment_id,c.initial_root_id) WHERE c.environment_id=$1 AND c.id=$2`, identity.EnvironmentID, identity.ComputerID).Scan(&result.rootID, &raw, &logicalBytes)
		} else {
			result.source.BaseVersion = restored.String()
			err = tx.QueryRow(ctx, `SELECT r.id,r.locator,r.logical_bytes FROM computer_saves s JOIN computer_disk_roots r ON (r.environment_id,r.id)=(s.environment_id,s.root_id) WHERE s.environment_id=$1 AND s.computer_id=$2 AND s.id=$3`, identity.EnvironmentID, identity.ComputerID, *restored).Scan(&result.rootID, &raw, &logicalBytes)
		}
		if err != nil {
			return err
		}
		result.source.Root, err = disk.ParseVersionRoot(raw, logicalBytes)
		if err != nil {
			return err
		}
		if restored == nil {
			digest, err := result.source.Root.Digest()
			if err != nil {
				return err
			}
			if digest != initial {
				return ErrConflict
			}
		}
		var boundRoot uuid.UUID
		err = tx.QueryRow(ctx, `SELECT base_root_id,write_key_id FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND base_root_id IS NOT NULL AND disk_released_at IS NULL`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&boundRoot, &result.source.WriteKeyID)
		if errors.Is(err, pgx.ErrNoRows) {
			if candidate == nil {
				return checkComputerDiskAuthority(ctx, tx, host, identity)
			}
			if candidate.id == uuid.Nil() || len(candidate.envelope.WrappingKeyID) == 0 || len(candidate.envelope.WrappingKeyID) > 2048 || len(candidate.envelope.Ciphertext) == 0 || len(candidate.envelope.Ciphertext) > computerkey.MaxWrappedSize {
				return ErrInvalidInput
			}
			if _, err := tx.Exec(ctx, `INSERT INTO computer_data_keys(environment_id,id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,$4,$5)`, identity.EnvironmentID, candidate.id, identity.ComputerID, candidate.envelope.WrappingKeyID, candidate.envelope.Ciphertext); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE computer_leases SET base_root_id=$4,write_key_id=$5 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND base_root_id IS NULL AND disk_released_at IS NULL`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, result.rootID, candidate.id); err != nil {
				return err
			}
			result.source.WriteKeyID = candidate.id
		} else if err != nil {
			return err
		} else if boundRoot != result.rootID {
			return ErrConflict
		}
		rows, err := tx.Query(ctx, `SELECT k.id,k.wrapping_key_id,k.wrapped_key,k.available FROM computer_data_keys k
 WHERE k.environment_id=$1 AND (k.id=$3 OR k.id IN (SELECT key_id FROM computer_object_keys WHERE environment_id=$1 AND digest=$2)) ORDER BY k.id FOR KEY SHARE`, identity.EnvironmentID, result.source.Root.Pack.Digest, result.source.WriteKeyID)
		if err != nil {
			return err
		}
		result.envelopes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (computerKeyEnvelope, error) {
			var k computerKeyEnvelope
			var available bool
			if err := row.Scan(&k.id, &k.envelope.WrappingKeyID, &k.envelope.Ciphertext, &available); err != nil {
				return k, err
			}
			if !available || len(k.envelope.Ciphertext) == 0 {
				return k, ErrDenied
			}
			return k, nil
		})
		if err != nil {
			return err
		}
		rootKey, writeKey := false, false
		for _, k := range result.envelopes {
			rootKey = rootKey || k.id.String() == result.source.Root.Page.KeyID
			writeKey = writeKey || k.id == result.source.WriteKeyID
		}
		if !rootKey || !writeKey {
			return ErrDenied
		}
		return checkComputerDiskAuthority(ctx, tx, host, identity)
	})
	if err != nil {
		return computerDiskPin{}, allocationError(err)
	}
	return result, nil
}

func checkComputerDiskAuthority(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, identity ComputerLeaseIdentity) error {
	var live bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3
 AND computer_instance_id=$4 AND worker_host_id=$5 AND worker_epoch=$6 AND status IN ('acquiring','active','releasing')
 AND delivered_at IS NOT NULL AND expires_at>clock_timestamp() AND fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2)`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&live)
	if err != nil {
		return err
	}
	if !live {
		return ErrDenied
	}
	return nil
}
