package run

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5"
)

// ComputerCreation is a Computer a live source Run creates from its pinned
// deployment.
type ComputerCreation struct {
	DeclaredID     string
	Key            *string
	Secrets        []secretbinding.Binding
	IdempotencyKey string
}

// CreateComputer creates a Computer for the fenced live source Run. A first
// transaction resolves the source. The creating transaction then locks the
// requested Secrets under the source Computer's secret ceiling, locks the
// live source again and runs the creation, which acquires its idempotency
// claim last; replays take the same locks and recheck the ceiling against
// the replayed Computer.
func CreateComputer(ctx context.Context, txb db.TxBeginner, creator computer.Creator, fence ExecutionFence, creation ComputerCreation) (computer.Created, error) {
	source, err := lockLiveSourceTx(ctx, txb, fence)
	if err != nil {
		return computer.Created{}, err
	}
	prepared, err := creator.PrepareRunCreation(computer.Request{
		Scope:      sourceScope(source),
		DeclaredID: creation.DeclaredID, Key: creation.Key, Secrets: creation.Secrets,
		IdempotencyKey: creation.IdempotencyKey,
	}, pgvalue.MustUUIDValue(source.RunID()), pgvalue.MustUUIDValue(source.ComputerID()))
	if err != nil {
		return computer.Created{}, err
	}
	var created computer.Created
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		// Secret rows precede mutable source instance authority, including replays.
		if err := computer.LockSecretsWithinCeiling(ctx, db.New(tx), source.ComputerID(), source.EnvironmentID(), creation.Secrets); err != nil {
			return err
		}
		if _, err := LockLiveSource(ctx, tx, fence); err != nil {
			return err
		}
		created, err = prepared.Create(ctx, tx)
		return err
	})
	return created, err
}

// ReadComputer reads a Computer in the fenced live source Run's scope, in the
// transaction that holds the source.
func ReadComputer(ctx context.Context, txb db.TxBeginner, fence ExecutionFence, computerID uuid.UUID) (computer.Snapshot, error) {
	var snapshot computer.Snapshot
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := LockLiveSource(ctx, tx, fence)
		if err != nil {
			return err
		}
		snapshot, err = computer.Read(ctx, db.New(tx), sourceScope(source), computerID)
		return err
	})
	return snapshot, err
}

// ListComputerMembers lists a page of a Computer's members in the fenced live
// source Run's scope, in the transaction that holds the source. A query the
// addressed Computer rejects is an outcome of a committed transaction: its
// computer.InputError is returned only after the commit, and a transaction
// failure takes precedence over it.
func ListComputerMembers(ctx context.Context, txb db.TxBeginner, fence ExecutionFence, computerID uuid.UUID, query computer.MembersQuery) (computer.MembersPage, error) {
	var page computer.MembersPage
	var rejected error
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		source, err := LockLiveSource(ctx, tx, fence)
		if err != nil {
			return err
		}
		page, err = computer.ListMembers(ctx, db.New(tx), sourceScope(source), computerID, query)
		var input computer.InputError
		if errors.As(err, &input) {
			rejected = err
			return nil
		}
		return err
	})
	if err != nil {
		return computer.MembersPage{}, err
	}
	if rejected != nil {
		return computer.MembersPage{}, rejected
	}
	return page, nil
}

// DeleteComputer deletes a Computer for the fenced live source Run. A first
// transaction resolves the source. The deleting transaction acquires the
// idempotency claim, then locks the live source together with the target
// Computer, including for a replay, and only then applies the deletion under
// the Computer lock.
func DeleteComputer(ctx context.Context, txb db.TxBeginner, fence ExecutionFence, computerID uuid.UUID, idempotencyKey string) (computer.Deleted, error) {
	source, err := lockLiveSourceTx(ctx, txb, fence)
	if err != nil {
		return computer.Deleted{}, err
	}
	var deleted computer.Deleted
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		claim, err := computer.ClaimDeletion(ctx, tx, computer.Deletion{
			Scope: sourceScope(source), ComputerID: computerID, IdempotencyKey: idempotencyKey,
		})
		if err != nil {
			return err
		}
		if _, err := LockLiveSourceForComputer(ctx, tx, fence, pgvalue.UUID(computerID)); err != nil {
			return err
		}
		if claim.Replayed() {
			deleted, err = claim.Receipt()
			return err
		}
		deleted, err = claim.Apply(ctx, tx)
		return err
	})
	return deleted, err
}

func sourceScope(source LiveSource) computer.Scope {
	return computer.Scope{
		OrgID:         pgvalue.MustUUIDValue(source.OrgID()),
		ProjectID:     pgvalue.MustUUIDValue(source.ProjectID()),
		EnvironmentID: pgvalue.MustUUIDValue(source.EnvironmentID()),
	}
}
