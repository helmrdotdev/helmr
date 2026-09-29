package identity

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

// Account is what a signed-in user sees about themselves: their profile and,
// when their session selects an organization, its name and whether it has a
// project yet. Without a selected organization, OrganizationExists reports
// whether any organization exists at all.
type Account struct {
	DisplayName        string
	ProfileImageURL    string
	OrgName            string
	OrgSlug            string
	HasProjects        bool
	OrganizationExists bool
}

// LoadAccount loads the account of a session principal. A user that no longer
// exists is ErrUserNotFound.
func LoadAccount(ctx context.Context, q db.Querier, user auth.Actor) (Account, error) {
	orgID := pgtype.UUID{}
	if user.OrgID != uuid.Nil() {
		orgID = pgvalue.UUID(user.OrgID)
	}
	state, err := q.GetUserOnboardingState(ctx, db.GetUserOnboardingStateParams{
		UserID: pgvalue.UUID(user.UserID), OrgID: orgID,
	})
	if isNoRows(err) {
		return Account{}, ErrUserNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("load current user: %w", err)
	}
	account := Account{
		DisplayName:     state.DisplayName,
		ProfileImageURL: state.ProfileImageURL.String,
		OrgName:         state.OrgName.String,
		OrgSlug:         state.OrgSlug.String,
		HasProjects:     state.HasProjects,
	}
	if !orgID.Valid {
		orgIDs, err := q.ListOrganizationIDs(ctx, 1)
		if err != nil {
			return Account{}, fmt.Errorf("load current organization: %w", err)
		}
		account.OrganizationExists = len(orgIDs) > 0
	}
	return account, nil
}
