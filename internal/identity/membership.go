package identity

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// CreateOrganization commits the organization and owner membership together
// with the initial administrator grant when setting up an installation.
func CreateOrganization(ctx context.Context, database db.TxBeginner, input org.OrganizationInput) (db.Organization, error) {
	var organization db.Organization
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var err error
		organization, err = org.CreateOrganization(ctx, tx, input)
		if err != nil {
			return err
		}
		if input.InitialSetup {
			if err := db.New(tx).GrantUserAdmin(ctx, pgvalue.UUID(input.OwnerUserID)); err != nil {
				return fmt.Errorf("grant initial administrator: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return db.Organization{}, err
	}
	return organization, nil
}

// RemoveMember disables membership and revokes organization-pinned and unpinned
// login sessions in one transaction, taking membership locks before session locks.
func RemoveMember(ctx context.Context, database db.TxBeginner, manager org.ManagingMember, user uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := org.DisableMember(ctx, q, manager, user); err != nil {
			return err
		}
		if err := q.RevokeLoginSessionsForMembership(ctx, db.RevokeLoginSessionsForMembershipParams{UserID: pgvalue.UUID(user), OrgID: pgvalue.UUID(manager.OrgID)}); err != nil {
			return fmt.Errorf("revoke member login sessions: %w", err)
		}
		return nil
	})
}

// AcceptInvitation runs in the caller's transaction, changing membership before
// revoking existing login sessions. Login can then issue a new session in the
// same transaction without an intermediate authority change.
func AcceptInvitation(ctx context.Context, q db.Querier, invitation org.PendingInvitation, user uuid.UUID, displayName string) error {
	if err := org.AcceptInvitation(ctx, q, invitation, user, displayName); err != nil {
		return err
	}
	if _, err := q.RevokeLoginSessionsForUser(ctx, pgvalue.UUID(user)); err != nil {
		return fmt.Errorf("revoke login sessions: %w", err)
	}
	return nil
}
