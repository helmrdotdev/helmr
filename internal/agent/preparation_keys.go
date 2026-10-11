package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// DataKeyWrapper keeps wrapping credentials in the control plane. Hosts
// receive only the data keys bound to their exact physical allocation.
type DataKeyWrapper interface {
	Wrap(context.Context, string, string, []byte) (computerkey.Envelope, error)
	Unwrap(context.Context, string, string, computerkey.Envelope) ([]byte, error)
}

type PreparationKeyBroker struct {
	pool    db.TxBeginner
	wrapper DataKeyWrapper
}

func NewPreparationKeyBroker(pool db.TxBeginner, wrapper DataKeyWrapper) (*PreparationKeyBroker, error) {
	if pool == nil || wrapper == nil {
		return nil, errors.New("preparation database and key wrapper are required")
	}
	return &PreparationKeyBroker{pool: pool, wrapper: wrapper}, nil
}

// PreparationKey is host-only material. Its receiver clears Key after use.
type PreparationKey struct {
	Scope string
	ID    uuid.UUID
	Key   []byte
}

type preparationKeyPin struct {
	scope    string
	id       uuid.UUID
	envelope computerkey.Envelope
}

// WriteKey creates or recovers this preparation's sole write key. Provider I/O
// runs without SQL locks. Every result is reauthorized after I/O, including
// revocation and expiry that occur while wrapping or unwrapping is in flight.
func (b *PreparationKeyBroker) WriteKey(ctx context.Context, host workergroup.HostPrincipal, ref PreparationExecutor) (PreparationKey, error) {
	pin, err := b.pin(ctx, host, ref, nil)
	if err != nil {
		return PreparationKey{}, err
	}
	if pin.id == uuid.Nil() {
		plain := make([]byte, computerkey.Size)
		if _, err = rand.Read(plain); err != nil {
			return PreparationKey{}, err
		}
		candidate := preparationKeyPin{scope: pin.scope, id: uuid.NewV7()}
		candidate.envelope, err = b.wrapper.Wrap(ctx, candidate.scope, candidate.id.String(), plain)
		clear(plain)
		if err != nil {
			return PreparationKey{}, err
		}
		pin, err = b.pin(ctx, host, ref, &candidate)
		if err != nil {
			return PreparationKey{}, err
		}
	}
	plain, err := b.wrapper.Unwrap(ctx, pin.scope, pin.id.String(), pin.envelope)
	if err != nil {
		clear(plain)
		return PreparationKey{}, err
	}
	if len(plain) != computerkey.Size {
		clear(plain)
		return PreparationKey{}, errors.New("preparation key provider returned invalid key length")
	}
	current, err := b.pin(ctx, host, ref, nil)
	if err != nil {
		clear(plain)
		return PreparationKey{}, err
	}
	if current.scope != pin.scope || current.id != pin.id || current.envelope.WrappingKeyID != pin.envelope.WrappingKeyID || !bytes.Equal(current.envelope.Ciphertext, pin.envelope.Ciphertext) {
		clear(plain)
		return PreparationKey{}, ErrConflict
	}
	return PreparationKey{Scope: pin.scope, ID: pin.id, Key: plain}, nil
}

func (b *PreparationKeyBroker) pin(ctx context.Context, host workergroup.HostPrincipal, ref PreparationExecutor, candidate *preparationKeyPin) (preparationKeyPin, error) {
	var result preparationKeyPin
	if !ref.valid() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, b.pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		if err := lockPreparationSecretOwners(ctx, tx, ref); err != nil {
			return err
		}
		if err := lockPreparationExecutorOnHost(ctx, tx, host, ref, true); err != nil {
			return err
		}
		var org uuid.UUID
		var id *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT e.org_id,p.write_key_id FROM computer_preparations p JOIN environments e ON e.id=p.environment_id WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, ref.PreparationID).Scan(&org, &id); err != nil {
			return err
		}
		var err error
		result.scope, err = computerkey.EncryptionScope(org.String(), ref.EnvironmentID.String())
		if err != nil {
			return err
		}
		if candidate != nil && candidate.scope != result.scope {
			return ErrConflict
		}
		if id == nil && candidate != nil {
			if candidate.id == uuid.Nil() || len(candidate.envelope.WrappingKeyID) == 0 || len(candidate.envelope.WrappingKeyID) > 2048 || len(candidate.envelope.Ciphertext) == 0 || len(candidate.envelope.Ciphertext) > computerkey.MaxWrappedSize {
				return ErrInvalidInput
			}
			if _, err = tx.Exec(ctx, `INSERT INTO computer_data_keys(id,environment_id,writer_preparation_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,$4,$5)`, candidate.id, ref.EnvironmentID, ref.PreparationID, candidate.envelope.WrappingKeyID, candidate.envelope.Ciphertext); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE computer_preparations SET write_key_id=$3 WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.PreparationID, candidate.id); err != nil {
				return err
			}
			id = &candidate.id
		}
		if id != nil {
			result.id = *id
			if err = tx.QueryRow(ctx, `SELECT wrapping_key_id,wrapped_key FROM computer_data_keys WHERE environment_id=$1 AND writer_preparation_id=$2 AND id=$3 AND available FOR KEY SHARE`, ref.EnvironmentID, ref.PreparationID, *id).Scan(&result.envelope.WrappingKeyID, &result.envelope.Ciphertext); err != nil {
				return err
			}
		}
		return checkPreparationExecutor(ctx, tx, host, ref, true)
	})
	if err != nil {
		return preparationKeyPin{}, hideMissing(err)
	}
	return result, nil
}

// Secrets precede spec/attempt locks. Lock declared owners even before first
// exposure so rotation/revocation cannot cross the deciding authorization check.
func lockPreparationSecretOwners(ctx context.Context, tx pgx.Tx, ref PreparationExecutor) error {
	rows, err := tx.Query(ctx, `SELECT s.status FROM secrets s WHERE s.environment_id=$1 AND s.id IN (
 SELECT binding.secret_id FROM computer_preparations p
 JOIN computer_secret_bindings binding ON (binding.environment_id,binding.preparation_spec_id)=(p.environment_id,p.preparation_spec_id)
 WHERE p.environment_id=$1 AND p.id=$2
 ) ORDER BY s.id FOR SHARE`, ref.EnvironmentID, ref.PreparationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			return err
		}
		if status != "active" {
			return ErrDenied
		}
	}
	return rows.Err()
}
