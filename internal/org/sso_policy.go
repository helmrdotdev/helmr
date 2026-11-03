package org

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// SSOPolicy is an organization's single sign-on policy. SessionSeconds is nil
// for the 24-hour default.
type SSOPolicy struct {
	Required       bool
	SessionSeconds *int32
}

// LockSSOPolicy reads the policy under the organization row lock, which every
// SSO policy change and issuing path takes.
func LockSSOPolicy(ctx context.Context, tx pgx.Tx, org uuid.UUID) (SSOPolicy, error) {
	var p SSOPolicy
	err := tx.QueryRow(ctx, `SELECT sso_required,sso_session_seconds FROM organizations WHERE id=$1 FOR UPDATE`, org).Scan(&p.Required, &p.SessionSeconds)
	return p, err
}

// SetSSOPolicy writes the policy. The caller holds LockSSOPolicy and has
// checked its preconditions.
func SetSSOPolicy(ctx context.Context, tx pgx.Tx, org uuid.UUID, p SSOPolicy) error {
	_, err := tx.Exec(ctx, `UPDATE organizations SET sso_required=$2,sso_session_seconds=$3,updated_at=clock_timestamp() WHERE id=$1`, org, p.Required, p.SessionSeconds)
	return err
}
