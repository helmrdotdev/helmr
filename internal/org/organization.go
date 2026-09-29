package org

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// OrganizationInput describes an organization to create and the user who owns it.
type OrganizationInput struct {
	Slug        string
	Name        string
	OwnerUserID uuid.UUID
	// InitialSetup admits only the installation's first organization and makes
	// its owner an administrator. Self-hosted installations create their
	// organization this way.
	InitialSetup bool
}

// CreateOrganization creates an organization with its owner as the first
// member. Initial setup serializes on the organizations table before counting,
// so concurrent setups admit one organization.
func CreateOrganization(ctx context.Context, txb db.TxBeginner, input OrganizationInput) (db.Organization, error) {
	slug, name, err := normalizeSlugName(input.Slug, input.Name)
	if err != nil {
		return db.Organization{}, err
	}
	var organization db.Organization
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if input.InitialSetup {
			if err := q.LockOrganizationsForSelfHostedSetup(ctx); err != nil {
				return fmt.Errorf("lock organizations: %w", err)
			}
			count, err := q.CountOrganizations(ctx)
			if err != nil {
				return fmt.Errorf("count organizations: %w", err)
			}
			if count > 0 {
				return ErrOrganizationExists
			}
		}
		organization, err = q.CreateOrganization(ctx, db.CreateOrganizationParams{
			ID:   pgvalue.UUID(uuid.NewV7()),
			Name: name,
			Slug: slug,
		})
		if db.IsUniqueViolation(err) {
			return ErrOrganizationSlugInUse
		}
		if err != nil {
			return fmt.Errorf("create organization: %w", err)
		}
		if _, err := q.EnsureOrgMember(ctx, db.EnsureOrgMemberParams{
			OrgID:       organization.ID,
			UserID:      pgvalue.UUID(input.OwnerUserID),
			Role:        db.OrgMemberRoleOwner,
			DisplayName: pgtype.Text{},
		}); err != nil {
			return fmt.Errorf("create organization owner: %w", err)
		}
		if input.InitialSetup {
			if err := q.GrantUserAdmin(ctx, pgvalue.UUID(input.OwnerUserID)); err != nil {
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
