package computer

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

type ImageBuildKeyBroker struct {
	pool    db.TxBeginner
	wrapper DataKeyWrapper
}

func NewImageBuildKeyBroker(pool db.TxBeginner, wrapper DataKeyWrapper) (*ImageBuildKeyBroker, error) {
	if pool == nil || wrapper == nil {
		return nil, errors.New("preparation database and key wrapper are required")
	}
	return &ImageBuildKeyBroker{pool: pool, wrapper: wrapper}, nil
}

// ImageBuildKey is host-only material. Its receiver clears Key after use.
type ImageBuildKey struct {
	Scope string
	ID    uuid.UUID
	Key   []byte
}

type imageBuildKeyPin struct {
	scope    string
	id       uuid.UUID
	envelope computerkey.Envelope
}

// WriteKey creates or recovers this preparation's sole write key. Provider I/O
// runs without SQL locks. Every result is reauthorized after I/O, including
// executor stop and expiry that occur while wrapping or unwrapping is in flight.
func (b *ImageBuildKeyBroker) WriteKey(ctx context.Context, host workergroup.HostPrincipal, ref ImageBuildExecutor) (ImageBuildKey, error) {
	pin, err := b.pin(ctx, host, ref, nil)
	if err != nil {
		return ImageBuildKey{}, err
	}
	if pin.id == uuid.Nil() {
		plain := make([]byte, computerkey.Size)
		if _, err = rand.Read(plain); err != nil {
			return ImageBuildKey{}, err
		}
		candidate := imageBuildKeyPin{scope: pin.scope, id: uuid.NewV7()}
		candidate.envelope, err = b.wrapper.Wrap(ctx, candidate.scope, candidate.id.String(), plain)
		clear(plain)
		if err != nil {
			return ImageBuildKey{}, err
		}
		pin, err = b.pin(ctx, host, ref, &candidate)
		if err != nil {
			return ImageBuildKey{}, err
		}
	}
	plain, err := b.wrapper.Unwrap(ctx, pin.scope, pin.id.String(), pin.envelope)
	if err != nil {
		clear(plain)
		return ImageBuildKey{}, err
	}
	if len(plain) != computerkey.Size {
		clear(plain)
		return ImageBuildKey{}, errors.New("preparation key provider returned invalid key length")
	}
	current, err := b.pin(ctx, host, ref, nil)
	if err != nil {
		clear(plain)
		return ImageBuildKey{}, err
	}
	if current.scope != pin.scope || current.id != pin.id || current.envelope.WrappingKeyID != pin.envelope.WrappingKeyID || !bytes.Equal(current.envelope.Ciphertext, pin.envelope.Ciphertext) {
		clear(plain)
		return ImageBuildKey{}, ErrConflict
	}
	return ImageBuildKey{Scope: pin.scope, ID: pin.id, Key: plain}, nil
}

func (b *ImageBuildKeyBroker) pin(ctx context.Context, host workergroup.HostPrincipal, ref ImageBuildExecutor, candidate *imageBuildKeyPin) (imageBuildKeyPin, error) {
	var result imageBuildKeyPin
	if !ref.Valid() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, b.pool, func(tx pgx.Tx) error {
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		if err := lockImageBuildExecutorOnHost(ctx, tx, host, ref, true); err != nil {
			return err
		}
		var org uuid.UUID
		var id *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT e.org_id,p.write_key_id FROM image_builds p JOIN environments e ON e.id=p.environment_id WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, ref.ImageBuildID).Scan(&org, &id); err != nil {
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
			if _, err = tx.Exec(ctx, `INSERT INTO disk_encryption_keys(id,environment_id,writer_image_build_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,$4,$5)`, candidate.id, ref.EnvironmentID, ref.ImageBuildID, candidate.envelope.WrappingKeyID, candidate.envelope.Ciphertext); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE image_builds SET write_key_id=$3 WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.ImageBuildID, candidate.id); err != nil {
				return err
			}
			id = &candidate.id
		}
		if id != nil {
			result.id = *id
			if err = tx.QueryRow(ctx, `SELECT wrapping_key_id,wrapped_key FROM disk_encryption_keys WHERE environment_id=$1 AND writer_image_build_id=$2 AND id=$3 AND available FOR KEY SHARE`, ref.EnvironmentID, ref.ImageBuildID, *id).Scan(&result.envelope.WrappingKeyID, &result.envelope.Ciphertext); err != nil {
				return err
			}
		}
		return CheckImageBuildExecutor(ctx, tx, host, ref, true)
	})
	if err != nil {
		return imageBuildKeyPin{}, hideMissing(err)
	}
	return result, nil
}
