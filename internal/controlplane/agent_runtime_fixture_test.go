package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Tests drive Turn transitions through the production runtime operations with
// the fixture Worker's host principal.

func fixtureHostFor(t *testing.T, pool db.TxBeginner, e session.Execution) workergroup.HostPrincipal {
	t.Helper()
	var group uuid.UUID
	if err := db.RunTx(t.Context(), pool, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT worker_group_id FROM worker_hosts WHERE id=$1`, e.WorkerHostID).Scan(&group)
	}); err != nil {
		t.Fatal(err)
	}
	return workergroup.HostPrincipal{HostID: e.WorkerHostID, GroupID: group, Epoch: e.WorkerEpoch, HostClaimVersion: 1, GroupClaimVersion: 1}
}

func dispatchRuntimeTurn(t *testing.T, pool db.TxBeginner, e session.Execution) (session.TurnDispatch, error) {
	t.Helper()
	host := fixtureHostFor(t, pool, e)
	var sequence int64
	if err := db.RunTx(t.Context(), pool, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT attachment_sequence FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&sequence)
	}); err != nil {
		return session.TurnDispatch{}, err
	}
	if sequence == 0 {
		a, err := session.AcquireRuntimeAttachment(t.Context(), pool, host, e)
		if err != nil {
			return session.TurnDispatch{}, err
		}
		sequence = a.Sequence
	}
	dispatch, err := session.DispatchRuntimeTurn(t.Context(), pool, host, e, sequence, nil)
	if err != nil {
		return session.TurnDispatch{}, err
	}
	if dispatch == nil {
		return session.TurnDispatch{}, session.ErrNotReady
	}
	return *dispatch, nil
}

func closeRuntimeProcessing(t *testing.T, pool db.TxBeginner, e session.Execution, turn uuid.UUID) error {
	t.Helper()
	return session.RuntimeCloseProcessing(t.Context(), pool, fixtureHostFor(t, pool, e), e, turn)
}

func finalizeRuntimeTurn(t *testing.T, pool db.TxBeginner, e session.Execution, turn uuid.UUID, result json.RawMessage, evidence string) (computer.SaveRequest, error) {
	t.Helper()
	if _, err := session.RuntimeFinalize(t.Context(), pool, fixtureHostFor(t, pool, e), e, turn, result, evidence); err != nil {
		return computer.SaveRequest{}, err
	}
	var save computer.SaveRequest
	err := db.RunTx(t.Context(), pool, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `SELECT id,computer_id,computer_lease_epoch,seq,requested_at FROM computer_saves WHERE environment_id=$1 AND turn_id=$2`, e.EnvironmentID, turn).Scan(&save.ID, &save.ComputerID, &save.LeaseEpoch, &save.Sequence, &save.RequestedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return computer.SaveRequest{}, session.ErrNotReady
	}
	return save, err
}

// bindFixtureLeaseDisk binds the fixture lease's disk source through the
// production key broker and returns the write key the Worker would hold.
// bindFixtureLeaseDisk binds the lease disk through the key broker exactly as a
// host does and returns the write key it may encrypt captures with. The shared
// fixture seeds the Computer's initial data key with a placeholder envelope, so
// the seeded rows are first re-wrapped under the fixture wrapper.
func bindFixtureLeaseDisk(t *testing.T, pool *pgxpool.Pool, host workergroup.HostPrincipal, env, computerID uuid.UUID, epoch int64) (uuid.UUID, []byte) {
	t.Helper()
	wrapper := fixtureKeyWrapper(t)
	var org string
	if err := pool.QueryRow(t.Context(), `SELECT org_id::text FROM environments WHERE id=$1`, env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	scope, err := computerkey.EncryptionScope(org, env.String())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(t.Context(), `SELECT id FROM disk_encryption_keys WHERE environment_id=$1`, env)
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range seeded {
		key := bytes.Repeat([]byte{byte(len(id.String()))}, computerkey.Size)
		envelope, err := wrapper.Wrap(t.Context(), scope, id.String(), key)
		if err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), pool, `UPDATE disk_encryption_keys SET wrapping_key_id=$3,wrapped_key=$4 WHERE environment_id=$1 AND id=$2`, env, id, envelope.WrappingKeyID, envelope.Ciphertext)
	}
	broker, err := computer.NewKeyBroker(pool, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	source, err := broker.Source(t.Context(), host, computer.LeaseIdentity{EnvironmentID: env, ComputerID: computerID, InstanceID: computerID, Epoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range source.Keys {
		if key.ID == source.WriteKeyID {
			return key.ID, append([]byte(nil), key.Key...)
		}
	}
	t.Fatal("broker returned no write key")
	return uuid.Nil(), nil
}

func fixtureKeyWrapper(t *testing.T) *computerkey.Local {
	t.Helper()
	wrapper, err := computerkey.NewLocal("runtime-fixture", bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return wrapper
}
