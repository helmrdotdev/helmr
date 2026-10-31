package dbtest

import (
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"testing"
	"uuid"
)

// BrowserPrincipal creates a real independent credential and browser proof for
// fixtures that exercise current authority. It never creates a membership/grant.
func BrowserPrincipal(t *testing.T, q db.DBTX, org, user uuid.UUID) auth.Principal {
	t.Helper()
	source, session := uuid.NewV7(), uuid.NewV7()
	MustExec(t, t.Context(), q, `INSERT INTO auth_identities(id,user_id,provider,subject,last_authenticated_at) VALUES($1,$2,'fixture',$1::uuid::text,clock_timestamp())`, source, user)
	MustExec(t, t.Context(), q, `INSERT INTO login_sessions(id,user_id,channel,token_hash,identity_id,authenticated_at,expires_at) SELECT $1,id,'browser',uuid_send($1::uuid),$3,clock_timestamp(),clock_timestamp()+interval '1 hour' FROM users WHERE id=$2`, session, user, source)
	p, err := access.SessionPrincipal(t.Context(), q, session, org)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// OperatorPrincipal creates a separate deployment identity with a live key and
// fixed-duration session. It has no ordinary account or tenant membership.
func OperatorPrincipal(t *testing.T, q db.DBTX) auth.Principal {
	t.Helper()
	op, key, session := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	MustExec(t, t.Context(), q, `INSERT INTO platform_operators(id,label,webauthn_user_handle) VALUES($1,'Fixture operator',uuid_send($1::uuid))`, op)
	MustExec(t, t.Context(), q, `INSERT INTO passkey_credentials(id,operator_id,credential_id,label,credential_data) VALUES($1,$2,uuid_send($1::uuid),'Fixture key','{}')`, key, op)
	p := auth.Principal{Kind: auth.PrincipalKindOperator, Channel: "operator", OperatorID: op, PasskeyID: key, SessionID: session}
	if err := q.QueryRow(t.Context(), `INSERT INTO login_sessions(id,operator_id,channel,passkey_id,issued_at,authenticated_at,token_hash,expires_at) VALUES($1,$2,'operator',$3,statement_timestamp(),statement_timestamp(),uuid_send($1::uuid),statement_timestamp()+interval '12 hours') RETURNING authenticated_at`, session, op, key).Scan(&p.AuthenticatedAt); err != nil {
		t.Fatal(err)
	}
	return p
}
