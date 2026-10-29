package org

import (
	"context"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// OrganizationInput describes an organization to create and the user who owns it.
type OrganizationInput struct {
	AuditPolicy auditpolicy.Policy
	Actor       audit.Evidence
	Slug        string
	Name        string
	OwnerUserID uuid.UUID
	// InitialSetup admits only the platform's first organization and makes
	// its first owner. Self-hosted instances create their
	// organization this way.
	InitialSetup bool
}

// CreateOrganization creates an organization with its owner as the first
// member. Initial setup serializes on the organizations table before counting,
// so concurrent setups admit one organization. The caller commits the owner
// membership and initial organization role grants in this transaction.
func CreateOrganization(ctx context.Context, tx pgx.Tx, input OrganizationInput) (db.Organization, error) {
	if input.Actor.Authorization.Decision == "" {
		input.Actor.Authorization = audit.NotEvaluated()
	}
	slug, name, err := normalizeSlugName(input.Slug, input.Name)
	if err != nil {
		return db.Organization{}, err
	}
	q := db.New(tx)
	if input.InitialSetup {
		if err := q.LockOrganizationsForSelfHostedSetup(ctx); err != nil {
			return db.Organization{}, fmt.Errorf("lock organizations: %w", err)
		}
		count, err := q.CountOrganizations(ctx)
		if err != nil {
			return db.Organization{}, fmt.Errorf("count organizations: %w", err)
		}
		if count > 0 {
			return db.Organization{}, ErrOrganizationExists
		}
	}
	organization, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{
		ID:   pgvalue.UUID(uuid.NewV7()),
		Name: name,
		Slug: slug,
	})
	if db.IsUniqueViolation(err) {
		return db.Organization{}, ErrOrganizationSlugInUse
	}
	if err != nil {
		return db.Organization{}, fmt.Errorf("create organization: %w", err)
	}
	owner, err := q.EnsureOrgMember(ctx, db.EnsureOrgMemberParams{ID: pgvalue.NewUUIDv7(),
		OrgID:       organization.ID,
		UserID:      pgvalue.UUID(input.OwnerUserID),
		IsOwner:     true,
		DisplayName: pgtype.Text{},
	})
	if err != nil {
		return db.Organization{}, fmt.Errorf("create organization owner: %w", err)
	}
	orgID := pgvalue.MustUUIDValue(organization.ID)
	if err := SeedStandardRoles(ctx, tx, input.AuditPolicy, orgID, input.Actor); err != nil {
		return db.Organization{}, err
	}
	for _, key := range []string{"access_administrator", "organization_structure_administrator"} {
		var role uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM roles WHERE org_id=$1 AND builtin_key=$2`, orgID, key).Scan(&role); err != nil {
			return db.Organization{}, err
		}
		grantID := uuid.NewV7()
		if _, err := tx.Exec(ctx, `INSERT INTO role_grants(id,org_id,role_id,member_id,created_by_user_id) VALUES($1,$2,$3,$4,$5)`, grantID, orgID, role, owner.ID, input.OwnerUserID); err != nil {
			return db.Organization{}, err
		}
		if err := accessAudit(ctx, tx, ManagingMember{Actor: input.Actor, OrgID: orgID, AuditPolicy: input.AuditPolicy}, "role_grant.created", "role_grant", grantID, nil, &audit.AccessSnapshot{SubjectKind: "member", MemberID: pgvalue.MustUUIDValue(owner.ID).String(), RoleID: role.String(), TargetKind: "organization"}); err != nil {
			return db.Organization{}, err
		}
	}
	if err := audit.Append(ctx, tx, input.AuditPolicy, audit.Event{Evidence: input.Actor, OrgID: orgID, Action: "organization.created", TargetKind: "organization", TargetID: orgID}); err != nil {
		return db.Organization{}, err
	}
	if err := audit.Append(ctx, tx, input.AuditPolicy, audit.Event{Evidence: input.Actor, OrgID: orgID, Action: "member.granted", TargetKind: "member", TargetID: pgvalue.MustUUIDValue(owner.ID), Details: audit.MembershipDetails{MembershipID: pgvalue.UUIDString(owner.ID), IsOwner: true}}); err != nil {
		return db.Organization{}, err
	}
	return organization, nil
}
