package computer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrKeyUnavailable reports that the persisted key or retained source a
// preparation needs is absent or violates its invariants, so no key can be
// delivered for it. A preparation that no longer authorizes delivery reports
// ErrAuthorityChanged instead.
var ErrKeyUnavailable = errors.New("computer key is unavailable")

// ErrKeyProviderUnavailable reports that the wrapping provider could not
// serve a wrap or unwrap: a recognized dependency unavailability that may
// succeed when retried.
var ErrKeyProviderUnavailable = errors.New("computer key provider is unavailable")

// keyUnavailable reports ErrKeyUnavailable with its cause as text. The cause
// is never wrapped: an absence signalled by pgx.ErrNoRows must not read as a
// changed authority.
func keyUnavailable(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrKeyUnavailable, fmt.Sprintf(format, args...))
}

// providerFailure classifies a failed wrap or unwrap. A cancelled or expired
// request keeps its cause, as a database failure would; recognized provider
// unavailability reports ErrKeyProviderUnavailable, an invalid persisted
// envelope ErrKeyUnavailable, and any other failure keeps its cause.
func providerFailure(ctx context.Context, operation string, err error) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("%s: %w", operation, err)
	case errors.Is(err, computerkey.ErrUnavailable):
		return fmt.Errorf("%w: %s: %w", ErrKeyProviderUnavailable, operation, err)
	case errors.Is(err, computerkey.ErrInvalidEnvelope):
		return keyUnavailable("%s: %v", operation, err)
	default:
		return fmt.Errorf("%s: %w", operation, err)
	}
}

// providerKeyLength rejects unwrapped plaintext of the wrong length, which
// breaks the provider's contract rather than the persisted key.
func providerKeyLength(key []byte) error {
	if len(key) == computerkey.Size {
		return nil
	}
	return fmt.Errorf("computer key provider returned %d key bytes", len(key))
}

// KeyWrapper is the provider that wraps Computer data keys. Only the key
// broker chooses envelopes, wrapping keys, scopes and data-key identifiers;
// worker hosts never do.
type KeyWrapper interface {
	Wrap(ctx context.Context, scope, id string, key []byte) (computerkey.Envelope, error)
	Unwrap(ctx context.Context, scope, id string, envelope computerkey.Envelope) ([]byte, error)
}

// KeyMaterial is one plaintext Computer data key in its wrapping scope. It
// is host-only material; its receiver clears Key after use.
type KeyMaterial struct {
	Scope, ID string
	Key       []byte
}

// SourceMaterial is the retained source version of a source preparation, its
// write key and the plaintext read keys of its closure.
type SourceMaterial struct {
	VersionID, Scope, WriteKeyID string
	Root                         disk.GenerationRoot
	Keys                         []KeyMaterial
}

// Clear clears every plaintext key.
func (s *SourceMaterial) Clear() {
	for _, k := range s.Keys {
		clear(k.Key)
	}
}

// KeyBroker delivers Computer data keys to the worker host preparing an
// Instance. Provider calls run outside SQL transactions; the preparation
// authority is revalidated after them before any plaintext is returned.
type KeyBroker struct {
	txb     db.TxBeginner
	wrapper KeyWrapper
}

// NewKeyBroker returns a broker over the transactions and wrapping provider.
func NewKeyBroker(txb db.TxBeginner, wrapper KeyWrapper) (*KeyBroker, error) {
	if txb == nil || wrapper == nil {
		return nil, errors.New("computer key transactions and wrapping provider are required")
	}
	return &KeyBroker{txb: txb, wrapper: wrapper}, nil
}

// encryptionScope binds ciphertext to immutable server-owned identities. It is
// authenticated context, not authorization; callers must obtain these
// identities from the admitted Computer, never from guest input or
// human-readable labels.
func encryptionScope(orgID, environmentID, computerID string) (string, error) {
	for _, id := range []string{orgID, environmentID, computerID} {
		if id == "" || !utf8.ValidString(id) {
			return "", errors.New("invalid computer encryption identity")
		}
	}
	encoded, err := json.Marshal([3]string{orgID, environmentID, computerID})
	if err != nil {
		return "", err
	}
	scope := "helmr.computer.v1:" + string(encoded)
	if len(scope) > 256 {
		return "", errors.New("computer encryption scope exceeds codec limit")
	}
	return scope, nil
}

// InitialKey delivers the write key of an initial preparation, creating and
// pinning it on first delivery. It never initializes a continuation: an
// absent key on a restored or continued Instance is unavailability. A
// discovery transaction authorizes the scope, the key is wrapped outside any
// transaction, a second transaction creates or adopts the persisted key and
// pins it to the Instance, the key is unwrapped outside it, and a third
// transaction revalidates the preparation and pin before delivery. A racing
// first delivery uses the winner's persisted key, and a lost reply keeps the
// same pin for its retry. A preparation that no longer authorizes delivery
// reports ErrAuthorityChanged, stale claims report workergroup.ErrStaleClaims,
// an absent, changed or invalid persisted key reports ErrKeyUnavailable and
// an unavailable provider reports ErrKeyProviderUnavailable; any other
// failure, including a cancelled request, keeps its cause.
// Plaintext is cleared on every failure.
func (b *KeyBroker) InitialKey(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef) (KeyMaterial, error) {
	pin, err := b.pinInitialKey(ctx, principal, ref, nil, "")
	if err != nil {
		return KeyMaterial{}, err
	}
	if pin.absent {
		// Scope discovery was authorized, but no secret leaves the control
		// plane here. Provider work must not hold SQL locks; a second
		// transaction revalidates before insert.
		key := make([]byte, computerkey.Size)
		if _, err = rand.Read(key); err != nil {
			return KeyMaterial{}, fmt.Errorf("generate computer key: %w", err)
		}
		keyID := pgvalue.UUID(uuid.NewV7())
		envelope, wrapErr := b.wrapper.Wrap(ctx, pin.scope, pgvalue.UUIDString(keyID), key)
		clear(key)
		if wrapErr != nil {
			return KeyMaterial{}, providerFailure(ctx, "wrap computer key", wrapErr)
		}
		candidate := db.ComputerDataKey{ID: keyID, WrappingKeyID: envelope.WrappingKeyID, WrappedKey: envelope.Ciphertext}
		if pin, err = b.pinInitialKey(ctx, principal, ref, &candidate, pin.scope); err != nil {
			return KeyMaterial{}, err
		}
	}
	keyID := pgvalue.UUIDString(pin.key.ID)
	plain, err := b.wrapper.Unwrap(ctx, pin.scope, keyID, computerkey.Envelope{WrappingKeyID: pin.key.WrappingKeyID, Ciphertext: pin.key.WrappedKey})
	if err != nil {
		clear(plain)
		return KeyMaterial{}, providerFailure(ctx, "unwrap computer key", err)
	}
	if err = providerKeyLength(plain); err != nil {
		clear(plain)
		return KeyMaterial{}, err
	}
	// A successful unwrap is not a delivery grant. Revocation, expiry,
	// cancellation or a changed reservation during provider I/O must suppress
	// the response.
	current, err := b.pinInitialKey(ctx, principal, ref, nil, "")
	if err != nil {
		clear(plain)
		return KeyMaterial{}, err
	}
	if current.absent || current.scope != pin.scope || current.key.ID != pin.key.ID || current.key.WrappingKeyID != pin.key.WrappingKeyID || !bytes.Equal(current.key.WrappedKey, pin.key.WrappedKey) {
		clear(plain)
		return KeyMaterial{}, keyUnavailable("pinned computer key changed during delivery")
	}
	return KeyMaterial{Scope: pin.scope, ID: keyID, Key: plain}, nil
}

// initialKeyPin is the write key one initial-key transaction pinned, or its
// authorized absence together with the scope a new key must be wrapped for.
type initialKeyPin struct {
	key    db.ComputerDataKey
	scope  string
	absent bool
}

// pinInitialKey runs one initial-key transaction under the preparation
// locks. Without a candidate it pins an existing key or reports its
// authorized absence; with a candidate it creates that key when none exists
// yet. No plaintext or provider I/O occurs inside the transaction. A fence
// that no longer holds, including a passed deadline, reports
// ErrAuthorityChanged.
func (b *KeyBroker) pinInitialKey(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, candidate *db.ComputerDataKey, expectedScope string) (initialKeyPin, error) {
	var pin initialKeyPin
	err := db.RunTx(ctx, b.txb, func(tx pgx.Tx) error {
		p, err := lockInitialPreparation(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		pin, err = p.pinKey(ctx, candidate, expectedScope)
		return err
	})
	if err != nil {
		return initialKeyPin{}, authorityChanged(err)
	}
	return pin, nil
}

// pinKey pins the Instance's write key, creating the Computer's first key
// from the candidate when neither the Computer nor the Instance has one. A
// missing key without a candidate is an authorized absence with the scope the
// candidate must be wrapped for. It rechecks the preparation deadlines after
// its writes.
func (p initialPreparation) pinKey(ctx context.Context, candidate *db.ComputerDataKey, expectedScope string) (initialKeyPin, error) {
	scope, err := p.encryptionScope()
	if err != nil {
		return initialKeyPin{}, keyUnavailable("%v", err)
	}
	if expectedScope != "" && expectedScope != scope {
		return initialKeyPin{}, keyUnavailable("computer encryption scope changed")
	}
	q := db.New(p.tx)
	runtimeID := p.instance.ID
	row, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: runtimeID, EnvironmentID: p.environmentID, ComputerID: p.computerID})
	if errors.Is(err, pgx.ErrNoRows) {
		// A missing row is initialization only when both authoritative
		// pointers are empty. Never replace an unavailable or corrupt
		// persisted key with a fresh one.
		var current, runtime pgtype.UUID
		if err = p.tx.QueryRow(ctx, `SELECT c.write_key_id,r.write_key_id FROM computers c JOIN computer_instances r ON r.environment_id=c.environment_id AND r.computer_id=c.id WHERE r.id=$1`, runtimeID).Scan(&current, &runtime); err != nil {
			return initialKeyPin{}, fmt.Errorf("read computer write key pointers: %w", err)
		}
		if current.Valid || runtime.Valid {
			return initialKeyPin{}, keyUnavailable("persisted computer write key is unavailable")
		}
		if candidate == nil {
			if err = p.checkDeadlines(ctx); err != nil {
				return initialKeyPin{}, err
			}
			return initialKeyPin{scope: scope, absent: true}, nil
		}
		row, err = q.CreateComputerKey(ctx, db.CreateComputerKeyParams{ID: candidate.ID, EnvironmentID: p.environmentID, ComputerID: p.computerID, WrappingKeyID: candidate.WrappingKeyID, WrappedKey: candidate.WrappedKey})
		if err != nil {
			return initialKeyPin{}, fmt.Errorf("create computer key: %w", err)
		}
		n, err := q.InitializeComputerWriteKey(ctx, db.InitializeComputerWriteKeyParams{KeyID: row.ID, EnvironmentID: row.EnvironmentID, ComputerID: row.ComputerID})
		if err != nil {
			return initialKeyPin{}, fmt.Errorf("initialize computer write key: %w", err)
		}
		if n != 1 {
			return initialKeyPin{}, keyUnavailable("computer write key is already initialized")
		}
	} else if err != nil {
		return initialKeyPin{}, fmt.Errorf("read computer write key: %w", err)
	}
	n, err := q.PinRuntimeComputerKey(ctx, db.PinRuntimeComputerKeyParams{KeyID: row.ID, ComputerInstanceID: runtimeID, EnvironmentID: row.EnvironmentID, ComputerID: row.ComputerID})
	if err != nil {
		return initialKeyPin{}, fmt.Errorf("pin computer write key: %w", err)
	}
	if n != 1 {
		return initialKeyPin{}, keyUnavailable("computer write key pin was rejected")
	}
	if err = p.checkDeadlines(ctx); err != nil {
		return initialKeyPin{}, err
	}
	return initialKeyPin{key: row, scope: scope}, nil
}

// SourceKeys pins the Instance's write key and delivers the plaintext keys of
// the retained source version's closure, with the write key appended when
// the closure lacks it. One transaction reads the envelopes under the
// preparation locks, the keys are unwrapped outside it, and a second
// transaction revalidates the preparation and envelopes before delivery. It
// grants no execution or publication. Errors are classified as InitialKey
// classifies them. The caller clears the returned plaintext; it is cleared
// on every failure.
func (b *KeyBroker) SourceKeys(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef) (_ SourceMaterial, retErr error) {
	source, rows, err := b.sourceEnvelopes(ctx, principal, ref)
	if err != nil {
		return SourceMaterial{}, err
	}
	defer func() {
		if retErr != nil {
			source.Clear()
		}
	}()
	for _, row := range rows {
		id := pgvalue.UUIDString(row.ID)
		key, err := b.wrapper.Unwrap(ctx, source.Scope, id, computerkey.Envelope{WrappingKeyID: row.WrappingKeyID, Ciphertext: row.WrappedKey})
		if err != nil {
			clear(key)
			return SourceMaterial{}, providerFailure(ctx, "unwrap computer key", err)
		}
		if err = providerKeyLength(key); err != nil {
			clear(key)
			return SourceMaterial{}, err
		}
		source.Keys = append(source.Keys, KeyMaterial{Scope: source.Scope, ID: id, Key: key})
	}
	current, currentRows, err := b.sourceEnvelopes(ctx, principal, ref)
	if err != nil {
		return SourceMaterial{}, err
	}
	if current.VersionID != source.VersionID || current.Scope != source.Scope || current.WriteKeyID != source.WriteKeyID || current.Root != source.Root || len(currentRows) != len(rows) {
		return SourceMaterial{}, keyUnavailable("retained computer source changed during delivery")
	}
	for i, row := range rows {
		now := currentRows[i]
		if now.ID != row.ID || now.WrappingKeyID != row.WrappingKeyID || !bytes.Equal(now.WrappedKey, row.WrappedKey) {
			return SourceMaterial{}, keyUnavailable("retained computer source key changed during delivery")
		}
	}
	return source, nil
}

// sourceEnvelopes reads the source envelopes in one transaction under the
// source preparation authority. A fence that no longer holds, including a
// passed deadline, reports ErrAuthorityChanged.
func (b *KeyBroker) sourceEnvelopes(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef) (SourceMaterial, []db.ComputerDataKey, error) {
	var source SourceMaterial
	var envelopes []db.ComputerDataKey
	err := db.RunTx(ctx, b.txb, func(tx pgx.Tx) error {
		fence, err := lockSourceFence(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		p, err := fence.claim(ctx, principal)
		if err != nil {
			return err
		}
		source, envelopes, err = p.envelopes(ctx)
		return err
	})
	if err != nil {
		return SourceMaterial{}, nil, authorityChanged(err)
	}
	return source, envelopes, nil
}

// envelopes pins the Instance write key and reads the wrapped keys of the
// retained source closure, the write key last when the closure lacks it. It
// rechecks the preparation deadlines after its writes.
func (p sourcePreparation) envelopes(ctx context.Context) (SourceMaterial, []db.ComputerDataKey, error) {
	q := db.New(p.tx)
	runtimeID := p.instance.ID
	retained, root, keys, err := loadRetainedGeneration(ctx, q, runtimeID)
	if err != nil {
		return SourceMaterial{}, nil, err
	}
	if retained.VersionID != p.versionID || root.LogicalBytes != p.logicalBytes {
		return SourceMaterial{}, nil, keyUnavailable("retained computer source differs from preparation")
	}
	writeKey, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: runtimeID, EnvironmentID: p.environmentID, ComputerID: p.computerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return SourceMaterial{}, nil, keyUnavailable("computer write key is absent")
	}
	if err != nil {
		return SourceMaterial{}, nil, fmt.Errorf("read computer write key: %w", err)
	}
	n, err := q.PinRuntimeComputerKey(ctx, db.PinRuntimeComputerKeyParams{KeyID: writeKey.ID, ComputerInstanceID: runtimeID, EnvironmentID: p.environmentID, ComputerID: p.computerID})
	if err != nil {
		return SourceMaterial{}, nil, fmt.Errorf("pin computer write key: %w", err)
	}
	if n != 1 {
		return SourceMaterial{}, nil, keyUnavailable("computer write key pin was rejected")
	}
	found := false
	for _, k := range keys {
		found = found || k.ID == writeKey.ID
	}
	if !found {
		keys = append(keys, writeKey)
	}
	scope, err := p.encryptionScope()
	if err != nil {
		return SourceMaterial{}, nil, keyUnavailable("%v", err)
	}
	if err = p.checkDeadlines(ctx); err != nil {
		return SourceMaterial{}, nil, err
	}
	return SourceMaterial{VersionID: pgvalue.UUIDString(retained.VersionID), Scope: scope, Root: root, WriteKeyID: pgvalue.UUIDString(writeKey.ID)}, keys, nil
}

// loadRetainedGeneration reads one retained generation and its complete
// certified key closure inside the caller's fenced transaction. Retention is
// not permission: it neither authorizes delivery nor unwraps keys, and
// callers revalidate live authority after any provider operation. An absent,
// invalid or incomplete retained generation reports ErrKeyUnavailable.
func loadRetainedGeneration(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) (db.GetInstanceComputerSourceRootRow, disk.GenerationRoot, []db.ComputerDataKey, error) {
	source, err := q.GetInstanceComputerSourceRoot(ctx, runtimeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return source, disk.GenerationRoot{}, nil, keyUnavailable("computer source is not retained")
	}
	if err != nil {
		return source, disk.GenerationRoot{}, nil, fmt.Errorf("read retained computer source: %w", err)
	}
	root, err := disk.ParseGenerationRoot(source.Locator, source.LogicalBytes)
	if err != nil {
		return source, disk.GenerationRoot{}, nil, keyUnavailable("retained computer source root is invalid: %v", err)
	}
	keys, err := q.ListInstanceComputerSourceKeys(ctx, runtimeID)
	if err != nil {
		return source, disk.GenerationRoot{}, nil, fmt.Errorf("read retained computer source keys: %w", err)
	}
	hasRootKey := false
	for _, key := range keys {
		if !key.Available.Valid || !key.Available.Bool || len(key.WrappedKey) == 0 {
			return source, disk.GenerationRoot{}, nil, keyUnavailable("retained computer source key is unavailable")
		}
		hasRootKey = hasRootKey || pgvalue.UUIDString(key.ID) == root.Page.KeyID
	}
	if !hasRootKey {
		return source, disk.GenerationRoot{}, nil, keyUnavailable("retained computer root key is missing")
	}
	return source, root, keys, nil
}
