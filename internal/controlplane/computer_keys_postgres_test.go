package controlplane

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgconn"
)

type observingKeyWrapper struct {
	computerKeyWrapper
	wrap      func()
	unwrap    func()
	returned  []byte
	unwrapErr error
}

func (w *observingKeyWrapper) Wrap(ctx context.Context, scope, id string, key []byte) (computerkey.Envelope, error) {
	if w.wrap != nil {
		w.wrap()
	}
	return w.computerKeyWrapper.Wrap(ctx, scope, id, key)
}
func (w *observingKeyWrapper) Unwrap(ctx context.Context, scope, id string, e computerkey.Envelope) ([]byte, error) {
	key, err := w.computerKeyWrapper.Unwrap(ctx, scope, id, e)
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
	fence := computerKeyFence{ComputerPreparationFence: dispatch.ComputerPreparationFence{RuntimeID: f.runtime, WorkerID: pgvalue.UUID(f.worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(f.worker.WorkerGroupID), WorkerEpoch: f.worker.WorkerEpoch, DesiredVersion: 1}}
	if err = f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,g.claim_version FROM worker_instances w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1`, f.runtimeWorker()).Scan(&fence.ClaimVersion, &fence.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	return f, broker, fence
}
func (f initialPublicationFixture) runtimeWorker() any {
	return pgvalue.UUID(f.worker.WorkerInstanceID)
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
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_keys WHERE computer_id=(SELECT workspace_id FROM runtime_instances WHERE id=$1)`, f.runtime).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate keys %d %v", count, err)
	}
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, first.ID)
	requireKeyFK(t, err)
	// Removing the current pointer cannot release an unreclaimed runtime's key.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspaces SET write_key_id=NULL WHERE id=(SELECT workspace_id FROM runtime_instances WHERE id=$1)`, f.runtime)
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, first.ID)
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
		if _, err = tx.Exec(ctx, `SELECT id FROM runtime_instances WHERE id=$1 FOR UPDATE`, f.runtime); err != nil {
			t.Fatal("provider called under runtime lock", err)
		}
		if _, err = tx.Exec(ctx, `SELECT id FROM workspaces WHERE id=(SELECT workspace_id FROM runtime_instances WHERE id=$1) FOR UPDATE`, f.runtime); err != nil {
			t.Fatal("provider called under Computer lock", err)
		}
	}
	b.wrapper = &observingKeyWrapper{computerKeyWrapper: b.wrapper, wrap: check, unwrap: check}
	key, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	clear(key.Key)
}
func TestInitialComputerKeyRevocationDuringProviderIO(t *testing.T) {
	for _, phase := range []string{"wrap", "unwrap"} {
		for _, change := range []string{"close", "worker epoch", "claim", "group claim", "expiry", "cancel"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				f, b, fence := initialKeyFixture(t)
				revoke := func() {
					switch change {
					case "close":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
					case "worker epoch":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_instances SET current_epoch=current_epoch+1 WHERE id=$1`, f.runtimeWorker())
					case "claim":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_instances SET claim_version=claim_version+1 WHERE id=$1`, f.runtimeWorker())
					case "group claim":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=$1`, pgvalue.UUID(f.worker.WorkerGroupID))
					case "expiry":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.runtime)
					case "cancel":
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancel_requested' WHERE id=(SELECT reserved_run_id FROM runtime_instances WHERE id=$1)`, f.runtime)
					}
				}
				observer := &observingKeyWrapper{computerKeyWrapper: b.wrapper}
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
	wrap := &barrierKeyWrapper{computerKeyWrapper: local, entered: entered, release: release}
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
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_keys`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("orphan keys %d %v", count, err)
	}
}

type barrierKeyWrapper struct {
	computerKeyWrapper
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
	return w.computerKeyWrapper.Wrap(ctx, scope, id, key)
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
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) SELECT $2,environment_id,computer_id,wrapping_key_id,wrapped_key FROM computer_keys WHERE id=$1`, first.ID, next)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspaces SET write_key_id=$2 WHERE id=(SELECT workspace_id FROM runtime_instances WHERE id=$1)`, f.runtime, next)
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
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, first.ID)
	requireKeyFK(t, err)
	// Terminal observation alone is insufficient to release the key.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='test',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_workspace_version_id=NULL WHERE id=$1`, f.runtime)
	_, err = f.Pool.Exec(t.Context(), `UPDATE computer_keys SET retired_at=now(),wrapped_key=NULL WHERE id=$1`, first.ID)
	requireKeyFK(t, err)
	// This models the trusted reclaimer recording physical exclusion, not a VM test.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_keys SET retired_at=clock_timestamp(),wrapped_key=NULL WHERE id=$1`, first.ID)
	var retained bool
	var audit string
	if err = f.Pool.QueryRow(t.Context(), `SELECT retained_computer_write_key_id IS NOT NULL,computer_write_key_id::text FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&retained, &audit); err != nil || retained || audit != first.ID {
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
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_keys SET wrapped_key=decode('00','hex') WHERE id=$1`, first.ID)
	if result, err := b.initial(t.Context(), fence); err == nil || len(result.Key) > 0 {
		t.Fatal("corrupt envelope accepted")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_keys`).Scan(&count); err != nil || count != 1 {
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
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO workspaces(id,environment_id,region_id,deployment_definition_id,status,desired_state,deleted_at) SELECT $2,environment_id,region_id,deployment_definition_id,'deleted','deleted',clock_timestamp() FROM workspaces WHERE id=(SELECT workspace_id FROM runtime_instances WHERE id=$1)`, f.runtime, other)
	foreign := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('00','hex'))`, foreign, pgvalue.UUID(f.EnvironmentID), other)
	_, err = f.Pool.Exec(t.Context(), `UPDATE runtime_instances SET computer_write_key_id=$2 WHERE id=$1`, f.runtime, foreign)
	requireKeyFK(t, err)
	_, err = f.Pool.Exec(t.Context(), `UPDATE workspaces SET write_key_id=$2 WHERE id=(SELECT workspace_id FROM runtime_instances WHERE id=$1)`, f.runtime, foreign)
	requireKeyFK(t, err)
}

func TestInitialComputerKeyTransientProviderFailureRetainsIdentity(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	observer := &observingKeyWrapper{computerKeyWrapper: b.wrapper, unwrapErr: errors.New("provider timeout")}
	b.wrapper = observer
	if result, err := b.initial(t.Context(), fence); err == nil || len(result.Key) != 0 {
		t.Fatal("provider error returned key")
	}
	if !bytes.Equal(observer.returned, make([]byte, 32)) {
		t.Fatal("failed provider material not cleared")
	}
	var pinned string
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_write_key_id::text FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&pinned); err != nil {
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
