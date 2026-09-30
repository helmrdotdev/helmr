package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type observingKeyWrapper struct {
	KeyWrapper
	wrap      func()
	unwrap    func()
	returned  []byte
	unwrapErr error
}

func (w *observingKeyWrapper) Wrap(ctx context.Context, scope, id string, key []byte) (computerkey.Envelope, error) {
	if w.wrap != nil {
		w.wrap()
	}
	return w.KeyWrapper.Wrap(ctx, scope, id, key)
}

func (w *observingKeyWrapper) Unwrap(ctx context.Context, scope, id string, e computerkey.Envelope) ([]byte, error) {
	key, err := w.KeyWrapper.Unwrap(ctx, scope, id, e)
	w.returned = key
	if w.unwrap != nil {
		w.unwrap()
	}
	if w.unwrapErr != nil {
		return key, w.unwrapErr
	}
	return key, err
}

func TestInitialComputerKeyRetryAndDurablePin(t *testing.T) {
	f := newPreparationFixture(t)
	b := f.broker
	first, err := b.InitialKey(t.Context(), f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first.Key)
	again, err := b.InitialKey(t.Context(), f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(again.Key)
	if first.ID != again.ID || first.Scope != again.Scope || !bytes.Equal(first.Key, again.Key) {
		t.Fatal("retry changed pinned key")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys WHERE computer_id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate keys %d %v", count, err)
	}
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, first.ID)
	requireKeyFK(t, err)
	// Removing the current pointer cannot release an unreclaimed runtime's key.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET write_key_id=NULL WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime)
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, first.ID)
	requireKeyFK(t, err)
}

func TestInitialComputerKeyProviderRunsOutsideLocks(t *testing.T) {
	f := newPreparationFixture(t)
	b := f.broker
	check := func() {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		tx, err := f.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err = tx.Exec(ctx, `SELECT id FROM computer_instances WHERE id=$1 FOR UPDATE`, f.runtime); err != nil {
			t.Fatal("provider called under runtime lock", err)
		}
		if _, err = tx.Exec(ctx, `SELECT id FROM computers WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1) FOR UPDATE`, f.runtime); err != nil {
			t.Fatal("provider called under Computer lock", err)
		}
	}
	b.wrapper = &observingKeyWrapper{KeyWrapper: b.wrapper, wrap: check, unwrap: check}
	key, err := b.InitialKey(t.Context(), f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	clear(key.Key)
}

func TestInitialComputerKeyRevocationDuringProviderIO(t *testing.T) {
	for _, phase := range []string{"wrap", "unwrap"} {
		for _, change := range []string{"close", "worker epoch", "claim", "group claim", "expiry", "computer stop"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f := newPreparationFixture(t)
				b := f.broker
				revoke := func() {
					switch change {
					case "close":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
					case "worker epoch":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET current_epoch=current_epoch+1 WHERE id=$1`, f.runtimeWorker())
					case "claim":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, f.runtimeWorker())
					case "group claim":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=$1`, pgvalue.UUID(f.principal.GroupID))
					case "expiry":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.runtime)
					case "computer stop":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET desired_state='stopped' WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime)
					}
				}
				observer := &observingKeyWrapper{KeyWrapper: b.wrapper}
				if phase == "wrap" {
					observer.wrap = revoke
				} else {
					observer.unwrap = revoke
				}
				b.wrapper = observer
				result, err := b.InitialKey(t.Context(), f.principal, f.ref)
				if err == nil || len(result.Key) != 0 {
					t.Fatal("revoked authority received key")
				}
				want := ErrAuthorityChanged
				if change == "claim" || change == "group claim" {
					want = workergroup.ErrStaleClaims
				}
				if !errors.Is(err, want) {
					t.Fatalf("%s classified as %v, want %v", change, err, want)
				}
				if len(observer.returned) > 0 && !bytes.Equal(observer.returned, make([]byte, len(observer.returned))) {
					t.Fatal("rejected plaintext not cleared")
				}
			})
		}
	}
}

func TestInitialComputerKeyConcurrentFirstFetch(t *testing.T) {
	f := newPreparationFixture(t)
	b := f.broker
	// Hold both requests in provider I/O after their authorized no-key discovery.
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	b.wrapper = &barrierKeyWrapper{KeyWrapper: b.wrapper, entered: entered, release: release}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var results [2]KeyMaterial
	var errs [2]error
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results[i], errs[i] = b.InitialKey(ctx, f.principal, f.ref) }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-ctx.Done():
			close(release)
			wg.Wait()
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	wg.Wait()
	for i := range 2 {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		defer clear(results[i].Key)
	}
	if results[0].ID != results[1].ID || !bytes.Equal(results[0].Key, results[1].Key) {
		t.Fatal("racing requests returned different keys")
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("orphan keys %d %v", count, err)
	}
}

type barrierKeyWrapper struct {
	KeyWrapper
	entered chan struct{}
	release chan struct{}
}

func (w *barrierKeyWrapper) Wrap(ctx context.Context, scope, id string, key []byte) (computerkey.Envelope, error) {
	w.entered <- struct{}{}
	select {
	case <-w.release:
	case <-ctx.Done():
		return computerkey.Envelope{}, ctx.Err()
	}
	return w.KeyWrapper.Wrap(ctx, scope, id, key)
}

func TestInitialComputerKeyRejectsAnotherWorker(t *testing.T) {
	f := newPreparationFixture(t)
	foreign := f.principal
	foreign.HostID = uuid.NewV7()
	if result, err := f.broker.InitialKey(t.Context(), foreign, f.ref); err == nil || len(result.Key) > 0 {
		t.Fatal("foreign Worker received key")
	}
}

func TestInitialComputerKeyRotationKeepsAdmittedRuntime(t *testing.T) {
	f := newPreparationFixture(t)
	b := f.broker
	first, err := b.InitialKey(t.Context(), f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first.Key)
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) SELECT $2,environment_id,computer_id,wrapping_key_id,wrapped_key FROM computer_data_keys WHERE id=$1`, first.ID, next)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET write_key_id=$2 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime, next)
	// The new current key is deliberately not decryptable under its new ID. A
	// re-fetch must use the runtime's original pin, not mutable current state.
	again, err := b.InitialKey(t.Context(), f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(again.Key)
	if again.ID != first.ID || !bytes.Equal(again.Key, first.Key) {
		t.Fatal("rotation changed admitted runtime key")
	}
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, first.ID)
	requireKeyFK(t, err)
	// Terminal observation alone is insufficient to release the key.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='test' WHERE id=$1`, f.runtime)
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_data_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, first.ID)
	requireKeyFK(t, err)
	// This models the trusted reclaimer recording physical exclusion, not a VM test.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='closed',mount_state='lost',reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_data_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE id=$1`, first.ID)
	var retained bool
	var audit string
	if err = f.Pool.QueryRow(t.Context(), `SELECT retained_write_key_id IS NOT NULL,write_key_id::text FROM computer_instances WHERE id=$1`, f.runtime).Scan(&retained, &audit); err != nil || retained || audit != first.ID {
		t.Fatalf("bad retention transfer: %v %s %v", retained, audit, err)
	}
}

func TestInitialComputerKeyCorruptEnvelopeDoesNotReinitialize(t *testing.T) {
	f := newPreparationFixture(t)
	b := f.broker
	first, err := b.InitialKey(t.Context(), f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	clear(first.Key)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_data_keys SET wrapped_key=decode('00','hex') WHERE id=$1`, first.ID)
	if result, err := b.InitialKey(t.Context(), f.principal, f.ref); !errors.Is(err, ErrKeyUnavailable) || errors.Is(err, ErrKeyProviderUnavailable) || len(result.Key) > 0 {
		t.Fatalf("corrupt envelope = %v", err)
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys`).Scan(&count); err != nil || count != 1 {
		t.Fatal("corruption generated replacement", err)
	}
}

func TestInitialComputerKeyForeignComputerPointersRejected(t *testing.T) {
	f := newPreparationFixture(t)
	key := f.initialKey(t)
	clear(key.Key)
	other := uuid.NewV7()
	// A deleted sibling is sufficient to exercise scope FKs without inventing
	// another active runtime/reservation or changing the fixture's head authority.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computers(id,environment_id,region_id,status,desired_state,deleted_at, computer_spec_id, creation_deployment_id) SELECT $2,environment_id,region_id,'deleted','deleted',clock_timestamp(), computer_spec_id, creation_deployment_id
 FROM computers WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime, other)
	foreign := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('00','hex'))`, foreign, pgvalue.UUID(f.EnvironmentID), other)
	_, err := f.Pool.Exec(t.Context(), `UPDATE computer_instances SET write_key_id=$2 WHERE id=$1`, f.runtime, foreign)
	requireKeyFK(t, err)
	_, err = f.Pool.Exec(t.Context(), `UPDATE computers SET write_key_id=$2 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime, foreign)
	requireKeyFK(t, err)
}

func TestInitialComputerKeyTransientProviderFailureRetainsIdentity(t *testing.T) {
	f := newPreparationFixture(t)
	b := f.broker
	observer := &observingKeyWrapper{KeyWrapper: b.wrapper, unwrapErr: fmt.Errorf("%w: provider timeout", computerkey.ErrUnavailable)}
	b.wrapper = observer
	if result, err := b.InitialKey(t.Context(), f.principal, f.ref); !errors.Is(err, ErrKeyProviderUnavailable) || len(result.Key) != 0 {
		t.Fatalf("provider error = %v", err)
	}
	if !bytes.Equal(observer.returned, make([]byte, 32)) {
		t.Fatal("failed provider material not cleared")
	}
	var pinned string
	if err := f.Pool.QueryRow(t.Context(), `SELECT write_key_id::text FROM computer_instances WHERE id=$1`, f.runtime).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	observer.unwrapErr = nil
	result, err := b.InitialKey(t.Context(), f.principal, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(result.Key)
	if result.ID != pinned {
		t.Fatal("provider retry replaced persisted key")
	}
}

var errInjectedSQL = errors.New("injected SQL failure")

// sqlFaults fails, once armed, one statement whose text contains match after
// letting skip of them through, or the next commit. Authority locks before
// the fault still run, so the failure reaches exactly the chosen step.
type sqlFaults struct {
	db.TxBeginner
	mu     sync.Mutex
	armed  bool
	match  string
	skip   int
	commit bool
}

// arm fails the statement containing match after skip of them.
func (f *sqlFaults) arm(match string, skip int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.match, f.skip = true, match, skip
}

func (f *sqlFaults) armCommit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed, f.commit = true, true
}

func (f *sqlFaults) fails(sql string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed || f.commit || !strings.Contains(sql, f.match) {
		return false
	}
	if f.skip > 0 {
		f.skip--
		return false
	}
	f.armed = false
	return true
}

func (f *sqlFaults) failsCommit() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.armed || !f.commit {
		return false
	}
	f.armed = false
	return true
}

func (f *sqlFaults) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return sqlFaultTx{Tx: tx, faults: f}, nil
}

type sqlFaultTx struct {
	pgx.Tx
	faults *sqlFaults
}

func (t sqlFaultTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.faults.fails(sql) {
		return pgconn.CommandTag{}, errInjectedSQL
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (t sqlFaultTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if t.faults.fails(sql) {
		return nil, errInjectedSQL
	}
	return t.Tx.Query(ctx, sql, args...)
}

func (t sqlFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.faults.fails(sql) {
		return sqlFaultRow{}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t sqlFaultTx) Commit(ctx context.Context) error {
	if t.faults.failsCommit() {
		// The owner's transaction runner rolls back after a failed commit.
		return errInjectedSQL
	}
	return t.Tx.Commit(ctx)
}

type sqlFaultRow struct{}

func (sqlFaultRow) Scan(...any) error { return errInjectedSQL }

const (
	claimReadSQL     = "SELECT w.claim_version,g.claim_version"
	firstFenceSQL    = "SELECT environment_id,computer_id,region_id,observed_state FROM computer_instances"
	deadlineSQL      = "-- name: GetComputerPreparationDeadlinesValid"
	pinWriteSQL      = "-- name: PinRuntimeComputerKey"
	sourceKeyReadSQL = "-- name: ListInstanceComputerSourceKeys"
)

// faultBroker serves b's transactions through a SQL fault injector; the
// returned function restores b.
func faultBroker(b *KeyBroker) (*sqlFaults, func()) {
	faults := &sqlFaults{TxBeginner: b.txb}
	previous := b.txb
	b.txb = faults
	return faults, func() { b.txb = previous }
}

// finalClaimReadFailure arms the claim-read fault during provider unwrap, so
// the first authority read succeeds and only the final revalidation fails.
func finalClaimReadFailure(b *KeyBroker) (*observingKeyWrapper, func()) {
	faults, restoreTx := faultBroker(b)
	observer := &observingKeyWrapper{KeyWrapper: b.wrapper, unwrap: func() { faults.arm(claimReadSQL, 0) }}
	previousWrapper := b.wrapper
	b.wrapper = observer
	return observer, func() { restoreTx(); b.wrapper = previousWrapper }
}

// A database failure during the final revalidation keeps its own cause,
// which the worker is told is an internal failure, and the unwrapped
// plaintext is cleared.
func TestInitialComputerKeyFinalClaimReadFailureIsNotAuthority(t *testing.T) {
	f := newPreparationFixture(t)
	observer, restore := finalClaimReadFailure(f.broker)
	defer restore()
	result, err := f.broker.InitialKey(t.Context(), f.principal, f.ref)
	if !errors.Is(err, errInjectedSQL) || errors.Is(err, ErrKeyUnavailable) || errors.Is(err, ErrAuthorityChanged) || len(result.Key) != 0 {
		t.Fatalf("final claim read failure = %v", err)
	}
	if len(observer.returned) == 0 || !bytes.Equal(observer.returned, make([]byte, len(observer.returned))) {
		t.Fatal("plaintext not cleared after final claim read failure")
	}
}

// deliveryFault is one database failure on the key delivery path.
type deliveryFault struct {
	name   string
	source bool
	// afterUnwrap arms the fault during the provider unwrap, so only the
	// final revalidation meets it.
	afterUnwrap bool
	match       string
	skip        int
	commit      bool
}

// Every database failure on the delivery path keeps its cause instead of
// reading as a rejected authority or an unavailable key, and plaintext the
// provider returned before it is cleared.
func TestComputerKeyDeliveryDatabaseFaultsKeepTheirCause(t *testing.T) {
	for _, fault := range []deliveryFault{
		{name: "initial first fence", match: firstFenceSQL},
		{name: "initial pin write", match: pinWriteSQL},
		{name: "initial commit", commit: true},
		{name: "initial final deadline query", afterUnwrap: true, match: deadlineSQL, skip: 1},
		{name: "source first fence", source: true, match: firstFenceSQL},
		{name: "source key read", source: true, match: sourceKeyReadSQL},
		{name: "source pin write", source: true, match: pinWriteSQL},
		{name: "source final deadline query", source: true, afterUnwrap: true, match: deadlineSQL, skip: 1},
	} {
		t.Run(fault.name, func(t *testing.T) {
			var f preparationFixture
			if fault.source {
				var input InitialVersion
				f, input = newGenerationFixture(t)
				if _, err := f.publisher.PublishInitialVersion(t.Context(), f.principal, f.ref, input); err != nil {
					t.Fatal(err)
				}
			} else {
				f = newPreparationFixture(t)
			}
			faults, restore := faultBroker(f.broker)
			defer restore()
			arm := func() {
				if fault.commit {
					faults.armCommit()
				} else {
					faults.arm(fault.match, fault.skip)
				}
			}
			observer := &observingKeyWrapper{KeyWrapper: f.broker.wrapper}
			f.broker.wrapper = observer
			if fault.afterUnwrap {
				armed := false
				observer.unwrap = func() {
					if !armed {
						armed = true
						arm()
					}
				}
			} else {
				arm()
			}
			var err error
			var delivered int
			if fault.source {
				var material SourceMaterial
				material, err = f.broker.SourceKeys(t.Context(), f.principal, f.ref)
				delivered = len(material.Keys)
				material.Clear()
			} else {
				var material KeyMaterial
				material, err = f.broker.InitialKey(t.Context(), f.principal, f.ref)
				delivered = len(material.Key)
				clear(material.Key)
			}
			if !errors.Is(err, errInjectedSQL) || errors.Is(err, ErrKeyUnavailable) || errors.Is(err, ErrAuthorityChanged) || delivered != 0 {
				t.Fatalf("fault = %v, delivered %d", err, delivered)
			}
			if fault.afterUnwrap && (len(observer.returned) == 0 || !bytes.Equal(observer.returned, make([]byte, len(observer.returned)))) {
				t.Fatal("plaintext not cleared after final revalidation failure")
			}
		})
	}
}

// Each provider failure is classified at the adapter boundary: recognized
// unavailability is provider unavailability, an invalid persisted envelope
// is an unavailable key, and an unexpected failure, a cancelled request or a
// wrong-length key keeps its cause. Plaintext returned before the failure is
// cleared.
func TestComputerKeyProviderFailuresKeepTheirClass(t *testing.T) {
	for _, test := range []struct {
		name string
		// fail returns the unwrap failure; cancel ends the request first.
		fail        func(key []byte) ([]byte, error)
		cancel      bool
		unavailable bool
		keyMissing  bool
	}{
		{name: "unavailable", fail: func(key []byte) ([]byte, error) {
			return key, fmt.Errorf("%w: throttled", computerkey.ErrUnavailable)
		}, unavailable: true},
		{name: "invalid envelope", fail: func(key []byte) ([]byte, error) {
			return key, fmt.Errorf("%w: failed authentication", computerkey.ErrInvalidEnvelope)
		}, keyMissing: true},
		{name: "unexpected", fail: func(key []byte) ([]byte, error) { return key, errors.New("access denied") }},
		{name: "cancelled", cancel: true, fail: func(key []byte) ([]byte, error) {
			return key, fmt.Errorf("%w: %w", computerkey.ErrUnavailable, context.Canceled)
		}},
		{name: "wrong length", fail: func(key []byte) ([]byte, error) { return append(key, 0), nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPreparationFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wrapper := &classifiedFailureWrapper{KeyWrapper: f.broker.wrapper, fail: test.fail}
			if test.cancel {
				wrapper.before = cancel
			}
			f.broker.wrapper = wrapper
			result, err := f.broker.InitialKey(ctx, f.principal, f.ref)
			if err == nil || len(result.Key) != 0 {
				t.Fatal("provider failure delivered a key")
			}
			if errors.Is(err, ErrKeyProviderUnavailable) != test.unavailable || errors.Is(err, ErrKeyUnavailable) != test.keyMissing {
				t.Fatalf("classified as %v", err)
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost its cause: %v", err)
			}
			if len(wrapper.returned) == 0 || !bytes.Equal(wrapper.returned, make([]byte, len(wrapper.returned))) {
				t.Fatal("provider plaintext not cleared")
			}
		})
	}
}

// classifiedFailureWrapper unwraps through its provider and then fails as
// fail chooses, keeping the plaintext it hands back.
type classifiedFailureWrapper struct {
	KeyWrapper
	before   func()
	fail     func(key []byte) ([]byte, error)
	returned []byte
}

func (w *classifiedFailureWrapper) Unwrap(ctx context.Context, scope, id string, e computerkey.Envelope) ([]byte, error) {
	key, err := w.KeyWrapper.Unwrap(ctx, scope, id, e)
	if err != nil {
		return nil, err
	}
	if w.before != nil {
		w.before()
	}
	key, err = w.fail(key)
	w.returned = key
	return key, err
}
