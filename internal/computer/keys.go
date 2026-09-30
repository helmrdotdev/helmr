package computer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
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

// ErrKeyUnavailable reports that the preparation no longer authorizes key
// delivery, or that the persisted key cannot be delivered for it.
var ErrKeyUnavailable = errors.New("computer key authority is unavailable")

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
// same pin for its retry. Authority rejections report ErrKeyUnavailable;
// stale claims report workergroup.ErrStaleClaims; other failures keep their
// own classification.
func (b *KeyBroker) InitialKey(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef) (KeyMaterial, error) {
	row, scope, err := b.pinInitialKey(ctx, principal, ref, nil, "")
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return KeyMaterial{}, err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		// Scope discovery was authorized, but no secret leaves the control
		// plane here. Provider work must not hold SQL locks; a second
		// transaction revalidates before insert.
		if scope == "" {
			return KeyMaterial{}, ErrKeyUnavailable
		}
		key := make([]byte, computerkey.Size)
		if _, err = rand.Read(key); err != nil {
			return KeyMaterial{}, err
		}
		keyID := pgvalue.UUID(uuid.NewV7())
		envelope, wrapErr := b.wrapper.Wrap(ctx, scope, pgvalue.UUIDString(keyID), key)
		clear(key)
		if wrapErr != nil {
			return KeyMaterial{}, errors.New("wrap computer key")
		}
		candidate := db.ComputerDataKey{ID: keyID, WrappingKeyID: envelope.WrappingKeyID, WrappedKey: envelope.Ciphertext}
		row, scope, err = b.pinInitialKey(ctx, principal, ref, &candidate, scope)
		if err != nil {
			return KeyMaterial{}, err
		}
	}
	keyID := pgvalue.UUIDString(row.ID)
	plain, err := b.wrapper.Unwrap(ctx, scope, keyID, computerkey.Envelope{WrappingKeyID: row.WrappingKeyID, Ciphertext: row.WrappedKey})
	if err != nil {
		clear(plain)
		return KeyMaterial{}, errors.New("unwrap computer key")
	}
	// A successful unwrap is not a delivery grant. Revocation, expiry,
	// cancellation or a changed reservation during provider I/O must suppress
	// the response. Only authority rejections become unavailability; stale
	// claims and database failures keep their own classification.
	current, currentScope, err := b.pinInitialKey(ctx, principal, ref, nil, "")
	if err != nil && !errors.Is(err, ErrKeyUnavailable) && !errors.Is(err, pgx.ErrNoRows) {
		clear(plain)
		return KeyMaterial{}, err
	}
	if err != nil || currentScope != scope || current.ID != row.ID || current.WrappingKeyID != row.WrappingKeyID || !bytes.Equal(current.WrappedKey, row.WrappedKey) || len(plain) != computerkey.Size {
		clear(plain)
		return KeyMaterial{}, ErrKeyUnavailable
	}
	return KeyMaterial{Scope: scope, ID: keyID, Key: plain}, nil
}

// pinInitialKey runs one initial-key transaction under the preparation
// locks. Without a candidate it pins an existing key, or reports an
// authorized absence as pgx.ErrNoRows together with the discovered scope;
// with a candidate it creates that key when none exists yet. No plaintext or
// provider I/O occurs inside the transaction.
func (b *KeyBroker) pinInitialKey(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, candidate *db.ComputerDataKey, expectedScope string) (db.ComputerDataKey, string, error) {
	var pinned db.ComputerDataKey
	var pinnedScope, discoveredScope string
	err := db.RunTx(ctx, b.txb, func(tx pgx.Tx) error {
		fence, err := lockInitialFence(ctx, tx, principal, ref)
		if err != nil {
			return ErrKeyUnavailable
		}
		p, err := fence.claim(ctx, principal)
		if err != nil {
			return err
		}
		row, scope, err := p.pinKey(ctx, candidate, expectedScope)
		if errors.Is(err, pgx.ErrNoRows) {
			discoveredScope = scope
			return err
		}
		if err != nil {
			return err
		}
		pinned, pinnedScope = row, scope
		return nil
	})
	if err != nil {
		return db.ComputerDataKey{}, discoveredScope, err
	}
	return pinned, pinnedScope, nil
}

// pinKey pins the Instance's write key, creating the Computer's first key
// from the candidate when neither the Computer nor the Instance has one. A
// missing key without a candidate returns pgx.ErrNoRows with the scope the
// candidate must be wrapped for. It rechecks the preparation deadlines after
// its writes.
func (p initialPreparation) pinKey(ctx context.Context, candidate *db.ComputerDataKey, expectedScope string) (db.ComputerDataKey, string, error) {
	scope, err := p.encryptionScope()
	if err != nil || (expectedScope != "" && expectedScope != scope) {
		return db.ComputerDataKey{}, "", ErrKeyUnavailable
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
			return db.ComputerDataKey{}, "", err
		}
		if current.Valid || runtime.Valid {
			return db.ComputerDataKey{}, "", ErrKeyUnavailable
		}
		if candidate == nil {
			if err = p.checkDeadlines(ctx); err != nil {
				return db.ComputerDataKey{}, "", ErrKeyUnavailable
			}
			return db.ComputerDataKey{}, scope, pgx.ErrNoRows
		}
		row, err = q.CreateComputerKey(ctx, db.CreateComputerKeyParams{ID: candidate.ID, EnvironmentID: p.environmentID, ComputerID: p.computerID, WrappingKeyID: candidate.WrappingKeyID, WrappedKey: candidate.WrappedKey})
		if err != nil {
			return db.ComputerDataKey{}, "", err
		}
		n, err := q.InitializeComputerWriteKey(ctx, db.InitializeComputerWriteKeyParams{KeyID: row.ID, EnvironmentID: row.EnvironmentID, ComputerID: row.ComputerID})
		if err != nil {
			return db.ComputerDataKey{}, "", err
		}
		if n != 1 {
			return db.ComputerDataKey{}, "", ErrKeyUnavailable
		}
	} else if err != nil {
		return db.ComputerDataKey{}, "", err
	}
	n, err := q.PinRuntimeComputerKey(ctx, db.PinRuntimeComputerKeyParams{KeyID: row.ID, ComputerInstanceID: runtimeID, EnvironmentID: row.EnvironmentID, ComputerID: row.ComputerID})
	if err != nil {
		return db.ComputerDataKey{}, "", err
	}
	if n != 1 {
		return db.ComputerDataKey{}, "", ErrKeyUnavailable
	}
	if err = p.checkDeadlines(ctx); err != nil {
		return db.ComputerDataKey{}, "", ErrKeyUnavailable
	}
	return row, scope, nil
}

// SourceKeys pins the Instance's write key and delivers the plaintext keys of
// the retained source version's closure, with the write key appended when
// the closure lacks it. One transaction reads the envelopes under the
// preparation locks, the keys are unwrapped outside it, and a second
// transaction revalidates the preparation and envelopes before delivery. It
// grants no execution or publication. Authority rejections report
// ErrKeyUnavailable; stale claims report workergroup.ErrStaleClaims; other
// failures keep their own classification. The caller clears the returned
// plaintext.
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
		if err != nil || len(key) != computerkey.Size {
			clear(key)
			return SourceMaterial{}, ErrKeyUnavailable
		}
		source.Keys = append(source.Keys, KeyMaterial{Scope: source.Scope, ID: id, Key: key})
	}
	// Only authority rejections become unavailability; stale claims and
	// database failures keep their own classification.
	current, currentRows, err := b.sourceEnvelopes(ctx, principal, ref)
	if err != nil && !errors.Is(err, ErrKeyUnavailable) {
		return SourceMaterial{}, err
	}
	if err != nil || current.VersionID != source.VersionID || current.Scope != source.Scope || current.WriteKeyID != source.WriteKeyID || current.Root != source.Root || len(currentRows) != len(rows) {
		return SourceMaterial{}, ErrKeyUnavailable
	}
	for i, row := range rows {
		now := currentRows[i]
		if now.ID != row.ID || now.WrappingKeyID != row.WrappingKeyID || !bytes.Equal(now.WrappedKey, row.WrappedKey) {
			return SourceMaterial{}, ErrKeyUnavailable
		}
	}
	return source, nil
}

func (b *KeyBroker) sourceEnvelopes(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef) (SourceMaterial, []db.ComputerDataKey, error) {
	var source SourceMaterial
	var envelopes []db.ComputerDataKey
	err := db.RunTx(ctx, b.txb, func(tx pgx.Tx) error {
		fence, err := lockSourceFence(ctx, tx, principal, ref)
		if err != nil {
			return ErrKeyUnavailable
		}
		p, err := fence.claim(ctx, principal)
		if err != nil {
			return err
		}
		source, envelopes, err = p.envelopes(ctx)
		return err
	})
	if err != nil {
		return SourceMaterial{}, nil, err
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
	if err != nil || retained.VersionID != p.versionID || root.LogicalBytes != p.logicalBytes {
		return SourceMaterial{}, nil, ErrKeyUnavailable
	}
	writeKey, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: runtimeID, EnvironmentID: p.environmentID, ComputerID: p.computerID})
	if err != nil {
		return SourceMaterial{}, nil, ErrKeyUnavailable
	}
	n, err := q.PinRuntimeComputerKey(ctx, db.PinRuntimeComputerKeyParams{KeyID: writeKey.ID, ComputerInstanceID: runtimeID, EnvironmentID: p.environmentID, ComputerID: p.computerID})
	if err != nil || n != 1 {
		return SourceMaterial{}, nil, ErrKeyUnavailable
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
		return SourceMaterial{}, nil, ErrKeyUnavailable
	}
	if err = p.checkDeadlines(ctx); err != nil {
		return SourceMaterial{}, nil, ErrKeyUnavailable
	}
	return SourceMaterial{VersionID: pgvalue.UUIDString(retained.VersionID), Scope: scope, Root: root, WriteKeyID: pgvalue.UUIDString(writeKey.ID)}, keys, nil
}

// loadRetainedGeneration reads one retained generation and its complete
// certified key closure inside the caller's fenced transaction. Retention is
// not permission: it neither authorizes delivery nor unwraps keys, and
// callers revalidate live authority after any provider operation.
func loadRetainedGeneration(ctx context.Context, q *db.Queries, runtimeID pgtype.UUID) (db.GetInstanceComputerSourceRootRow, disk.GenerationRoot, []db.ComputerDataKey, error) {
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
