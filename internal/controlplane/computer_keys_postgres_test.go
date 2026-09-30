package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type observingKeyWrapper struct {
	ComputerKeyWrapper
	wrap      func()
	unwrap    func()
	returned  []byte
	unwrapErr error
}

func (w *observingKeyWrapper) Wrap(ctx context.Context, scope, id string, key []byte) (computerkey.Envelope, error) {
	if w.wrap != nil {
		w.wrap()
	}
	return w.ComputerKeyWrapper.Wrap(ctx, scope, id, key)
}
func (w *observingKeyWrapper) Unwrap(ctx context.Context, scope, id string, e computerkey.Envelope) ([]byte, error) {
	key, err := w.ComputerKeyWrapper.Unwrap(ctx, scope, id, e)
	w.returned = key
	if w.unwrap != nil {
		w.unwrap()
	}
	if w.unwrapErr != nil {
		return key, w.unwrapErr
	}
	return key, err
}
func initialKeyFixture(t *testing.T) (initialPublicationFixture, *computerKeyBroker, computerKeyFence) {
	t.Helper()
	f := newInitialPublicationFixture(t)
	local, err := computerkey.NewLocal("local-1", bytes.Repeat([]byte{0x63}, 32))
	if err != nil {
		t.Fatal(err)
	}
	broker, err := newComputerKeyBroker(f.Pool, local)
	if err != nil {
		t.Fatal(err)
	}
	fence := computerKeyFence{ComputerPreparationFence: dispatch.ComputerPreparationFence{RuntimeID: f.runtime, WorkerID: pgvalue.UUID(f.worker.HostID), WorkerGroupID: pgvalue.UUID(f.worker.GroupID), WorkerEpoch: f.worker.Epoch, DesiredVersion: 1}}
	if err = f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,g.claim_version FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1`, f.runtimeWorker()).Scan(&fence.ClaimVersion, &fence.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	return f, broker, fence
}
func (f initialPublicationFixture) runtimeWorker() any {
	return pgvalue.UUID(f.worker.HostID)
}
func requireKeyFK(t *testing.T, err error) {
	t.Helper()
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23503" {
		t.Fatalf("expected FK violation: %v", err)
	}
}
func TestInitialComputerKeyRetryAndDurablePin(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	first, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first.Key)
	again, err := b.initial(t.Context(), fence)
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
	f, b, fence := initialKeyFixture(t)
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
	b.wrapper = &observingKeyWrapper{ComputerKeyWrapper: b.wrapper, wrap: check, unwrap: check}
	key, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	clear(key.Key)
}
func TestInitialComputerKeyRevocationDuringProviderIO(t *testing.T) {
	for _, phase := range []string{"wrap", "unwrap"} {
		for _, change := range []string{"close", "worker epoch", "claim", "group claim", "expiry", "computer stop"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f, b, fence := initialKeyFixture(t)
				revoke := func() {
					switch change {
					case "close":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
					case "worker epoch":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET current_epoch=current_epoch+1 WHERE id=$1`, f.runtimeWorker())
					case "claim":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, f.runtimeWorker())
					case "group claim":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=$1`, pgvalue.UUID(f.worker.GroupID))
					case "expiry":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.runtime)
					case "computer stop":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET desired_state='stopped' WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime)
					}
				}
				observer := &observingKeyWrapper{ComputerKeyWrapper: b.wrapper}
				if phase == "wrap" {
					observer.wrap = revoke
				} else {
					observer.unwrap = revoke
				}
				b.wrapper = observer
				result, err := b.initial(t.Context(), fence)
				if err == nil || len(result.Key) != 0 {
					t.Fatal("revoked authority received key")
				}
				want := errComputerKeyUnavailable
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
	f, b, fence := initialKeyFixture(t)
	// Hold both requests in provider I/O after their authorized no-key discovery.
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	local := b.wrapper
	wrap := &barrierKeyWrapper{ComputerKeyWrapper: local, entered: entered, release: release}
	b.wrapper = wrap
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var results [2]computerKeyMaterial
	var errs [2]error
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results[i], errs[i] = b.initial(ctx, fence) }()
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
	ComputerKeyWrapper
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
	return w.ComputerKeyWrapper.Wrap(ctx, scope, id, key)
}
func TestInitialComputerKeyRejectsAnotherWorker(t *testing.T) {
	_, b, fence := initialKeyFixture(t)
	fence.WorkerID = pgvalue.UUID(uuid.NewV7())
	if result, err := b.initial(t.Context(), fence); err == nil || len(result.Key) > 0 {
		t.Fatal("foreign Worker received key")
	}
}

func TestInitialComputerKeyRotationKeepsAdmittedRuntime(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	first, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first.Key)
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) SELECT $2,environment_id,computer_id,wrapping_key_id,wrapped_key FROM computer_data_keys WHERE id=$1`, first.ID, next)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET write_key_id=$2 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime, next)
	// The new current key is deliberately not decryptable under its new ID. A
	// re-fetch must use the runtime's original pin, not mutable current state.
	again, err := b.initial(t.Context(), fence)
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
	f, b, fence := initialKeyFixture(t)
	first, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	clear(first.Key)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_data_keys SET wrapped_key=decode('00','hex') WHERE id=$1`, first.ID)
	if result, err := b.initial(t.Context(), fence); err == nil || len(result.Key) > 0 {
		t.Fatal("corrupt envelope accepted")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys`).Scan(&count); err != nil || count != 1 {
		t.Fatal("corruption generated replacement", err)
	}
}
func TestInitialComputerKeyForeignComputerPointersRejected(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	key, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	clear(key.Key)
	other := uuid.NewV7()
	// A deleted sibling is sufficient to exercise scope FKs without inventing
	// another active runtime/reservation or changing the fixture's head authority.
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computers(id,environment_id,region_id,status,desired_state,deleted_at, computer_spec_id, creation_deployment_id) SELECT $2,environment_id,region_id,'deleted','deleted',clock_timestamp(), computer_spec_id, creation_deployment_id
 FROM computers WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime, other)
	foreign := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('00','hex'))`, foreign, pgvalue.UUID(f.EnvironmentID), other)
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_instances SET write_key_id=$2 WHERE id=$1`, f.runtime, foreign)
	requireKeyFK(t, err)
	_, err = f.Pool.Exec(t.Context(), `UPDATE computers SET write_key_id=$2 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, f.runtime, foreign)
	requireKeyFK(t, err)
}

func TestInitialComputerKeyTransientProviderFailureRetainsIdentity(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	observer := &observingKeyWrapper{ComputerKeyWrapper: b.wrapper, unwrapErr: errors.New("provider timeout")}
	b.wrapper = observer
	if result, err := b.initial(t.Context(), fence); err == nil || len(result.Key) != 0 {
		t.Fatal("provider error returned key")
	}
	if !bytes.Equal(observer.returned, make([]byte, 32)) {
		t.Fatal("failed provider material not cleared")
	}
	var pinned string
	if err := f.Pool.QueryRow(t.Context(), `SELECT write_key_id::text FROM computer_instances WHERE id=$1`, f.runtime).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	observer.unwrapErr = nil
	result, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(result.Key)
	if result.ID != pinned {
		t.Fatal("provider retry replaced persisted key")
	}
}

var errInjectedClaimRead = errors.New("injected Worker claim read failure")

// claimReadFaults fails the Worker claim read once armed. Authority locks still
// run, so the injected failure reaches exactly the post-lock claim comparison.
type claimReadFaults struct {
	db.TxBeginner
	armed bool
}

func (f *claimReadFaults) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return claimReadFaultTx{Tx: tx, faults: f}, nil
}

type claimReadFaultTx struct {
	pgx.Tx
	faults *claimReadFaults
}

func (t claimReadFaultTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if t.faults.armed && strings.HasPrefix(sql, "SELECT w.claim_version,g.claim_version") {
		return claimReadFaultRow{}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

type claimReadFaultRow struct{}

func (claimReadFaultRow) Scan(...any) error { return errInjectedClaimRead }

// finalClaimReadFailure arms the claim-read fault during provider unwrap, so the
// first authority read succeeds and only the final revalidation fails.
func finalClaimReadFailure(b *computerKeyBroker) (*observingKeyWrapper, func()) {
	faults := &claimReadFaults{TxBeginner: b.tx}
	observer := &observingKeyWrapper{ComputerKeyWrapper: b.wrapper, unwrap: func() { faults.armed = true }}
	previousTx, previousWrapper := b.tx, b.wrapper
	b.tx, b.wrapper = faults, observer
	return observer, func() { b.tx, b.wrapper = previousTx, previousWrapper }
}

func invokeComputerKeyHandler(t *testing.T, handler http.HandlerFunc, fence computerKeyFence, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	worker := workergroup.HostPrincipal{HostID: pgvalue.MustUUIDValue(fence.WorkerID), GroupID: pgvalue.MustUUIDValue(fence.WorkerGroupID), Epoch: fence.WorkerEpoch, HostClaimVersion: fence.ClaimVersion, GroupClaimVersion: fence.GroupClaimVersion}
	request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).WithContext(context.WithValue(t.Context(), workerContextKey{}, worker))
	response := httptest.NewRecorder()
	handler(response, request)
	return response
}

func TestInitialComputerKeyFinalClaimReadFailureIsUnavailable(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	f.server.computerKeys = b
	observer, restore := finalClaimReadFailure(b)
	defer restore()
	response := invokeComputerKeyHandler(t, f.server.workerInitialComputerKey, fence, workerapi.InitialComputerKeyRequest{ComputerInstanceID: pgvalue.UUIDString(fence.RuntimeID), DesiredVersion: fence.DesiredVersion})
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("final claim read failure status=%d body=%s", response.Code, response.Body.String())
	}
	if len(observer.returned) == 0 || !bytes.Equal(observer.returned, make([]byte, len(observer.returned))) {
		t.Fatal("plaintext not cleared after final claim read failure")
	}
}
