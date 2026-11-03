package workergroup

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/jackc/pgx/v5"
)

// Operator is the authority a worker group operation acts under. The
// deployment operator manages deployment groups; an organization manages only
// the groups it owns. A group outside the operator's ownership reads exactly
// like a missing group. Ownership is fixed at creation, so checking it needs
// no lock.
type Operator struct {
	orgID uuid.UUID
	// authorize returns the actor once its current authority is confirmed in
	// the operation's transaction, before any worker group lock.
	authorize func(context.Context, pgx.Tx) (audit.Evidence, error)
	// safety lets the deployment operator pause, revoke and disable any group
	// for platform safety, and nothing else.
	safety bool
}

// DeploymentOperator manages deployment groups as actor. Its authority is
// checked when the request is admitted and again by the audit record it
// appends.
func DeploymentOperator(actor audit.Evidence) Operator {
	return Operator{authorize: fixedActor(actor)}
}

// PlatformSafetyOperator lets the deployment operator pause any group,
// including an organization's, revoke its host credentials and disable it.
// The audit record then belongs to the owning organization.
func PlatformSafetyOperator(actor audit.Evidence) Operator {
	return Operator{authorize: fixedActor(actor), safety: true}
}

// OrganizationOperator manages the groups orgID owns. authorize rechecks the
// acting member's current authority first in each operation's transaction
// and returns the actor it records.
func OrganizationOperator(orgID uuid.UUID, authorize func(context.Context, pgx.Tx) (audit.Evidence, error)) Operator {
	return Operator{orgID: orgID, authorize: authorize}
}

var errSafetyOperation = errors.New("platform safety allows only pausing a worker group, revoking its host credentials and disabling it")

func fixedActor(actor audit.Evidence) func(context.Context, pgx.Tx) (audit.Evidence, error) {
	return func(context.Context, pgx.Tx) (audit.Evidence, error) { return actor, nil }
}

// admit authorizes the operator for the group and returns the actor and the
// group's owner, Nil for a deployment group, or ErrGroupNotFound.
func (o Operator) admit(ctx context.Context, tx pgx.Tx, groupID uuid.UUID) (audit.Evidence, uuid.UUID, error) {
	actor, err := o.authorize(ctx, tx)
	if err != nil {
		return audit.Evidence{}, uuid.Nil(), err
	}
	var owner *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT owner_org_id FROM worker_groups WHERE id=$1`, groupID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return audit.Evidence{}, uuid.Nil(), ErrGroupNotFound
	}
	if err != nil {
		return audit.Evidence{}, uuid.Nil(), fmt.Errorf("read worker group owner: %w", err)
	}
	ownerID := uuid.Nil()
	if owner != nil {
		ownerID = *owner
	}
	if !o.safety && ownerID != o.orgID {
		return audit.Evidence{}, uuid.Nil(), ErrGroupNotFound
	}
	return actor, ownerID, nil
}
