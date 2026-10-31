package audit_test

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"testing"
	"uuid"
)

func TestEvidencePreservesIndependentOperatorAndAccountIdentity(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	user, credential := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Account name')`, user)
	ordinary := auth.Principal{Kind: auth.PrincipalKindSession, UserID: user, SessionID: credential}
	if _, err := audit.PlatformOperator(ordinary); err == nil {
		t.Fatal("ordinary account became operator")
	}
	operator := dbtest.OperatorPrincipal(t, d.Pool)
	admin, err := audit.PlatformOperator(operator)
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []audit.Evidence{audit.FromPrincipal(ordinary), admin} {
		if err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
			return audit.Append(t.Context(), tx, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Event{Evidence: evidence, Action: "worker_group.created", TargetKind: "worker_group", TargetID: uuid.NewV7()})
		}); err != nil {
			t.Fatal(err)
		}
	}
	var valid bool
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*)=2 AND count(*) FILTER(WHERE actor_user_id=$1 AND actor_operator_id IS NULL AND credential_id=$2 AND actor_identity->>'display_name'='Account name')=1 AND count(*) FILTER(WHERE actor_operator_id=$3 AND actor_user_id IS NULL AND credential_id=$4 AND authorization_evidence->'checks' @> '[{"type":"platform_operator"}]'::jsonb)=1 FROM audit_events`, user, credential, operator.OperatorID, operator.SessionID).Scan(&valid); err != nil || !valid {
		t.Fatal(valid, err)
	}
	dbtest.MustExec(t, t.Context(), d.Pool, `DELETE FROM users WHERE id=$1`, user)
	dbtest.MustExec(t, t.Context(), d.Pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, operator.SessionID)
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*)=2 AND count(*) FILTER(WHERE actor_user_id=$1 AND actor_identity->>'display_name'='Account name')=1 FROM audit_events`, user).Scan(&valid); err != nil || !valid {
		t.Fatal("history lost", valid, err)
	}
	err = db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
		return audit.Append(t.Context(), tx, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Event{Evidence: admin, Action: "worker_group.created", TargetKind: "worker_group", TargetID: uuid.NewV7()})
	})
	if err == nil {
		t.Fatal("retired operator session committed mutation")
	}
}

func TestEvidenceRejectsConflictingIdentitiesAndInventedAuthority(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	cases := map[string]audit.Evidence{}
	internal := audit.Internal(audit.ControlPlane)
	both := internal
	both.Actor.APIKeyID = uuid.NewV7()
	cases["two actors"] = both
	unverified := audit.Unauthenticated("failed", "api_key")
	unverified.Authentication.CredentialID = uuid.NewV7()
	cases["unverified credential id"] = unverified
	fabricated := internal
	fabricated.Authorization = audit.Authorization{Decision: "allowed"}
	cases["internal origin is not authorization"] = fabricated
	unknown := internal
	unknown.Actor.Component = "invitation_expiry"
	cases["operation is not component identity"] = unknown
	for name, evidence := range cases {
		t.Run(name, func(t *testing.T) {
			err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
				return audit.Append(t.Context(), tx, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Event{Evidence: evidence, Action: "worker_group.created", TargetKind: "worker_group", TargetID: uuid.NewV7()})
			})
			if !errors.Is(err, audit.ErrCapture) {
				t.Fatal("accepted invalid evidence", err)
			}
		})
	}
	// Successful internal execution need not claim a separate authorization check.
	if err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
		return audit.Append(t.Context(), tx, auditpolicy.Policy{RetentionSeconds: 3600}, audit.Event{Evidence: internal, Action: "worker_group.created", TargetKind: "worker_group", TargetID: uuid.NewV7()})
	}); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := d.Pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(authentication_origin='internal' AND authentication_result='not_attempted' AND authorization_evidence->>'decision'='not_evaluated' AND outcome='succeeded') FROM audit_events`).Scan(&valid); err != nil || !valid {
		t.Fatal(valid, err)
	}
	// JSON null is not an authorization decision; CHECK must reject it rather than
	// accepting SQL UNKNOWN from ->> comparisons.
	if _, err := d.Pool.Exec(t.Context(), `UPDATE audit_events SET authorization_evidence='{"decision":null,"checks":[]}'::jsonb`); err == nil {
		t.Fatal("database accepted null authorization decision")
	}
	// Database invariants independently prohibit contradictory actor references.
	if _, err := d.Pool.Exec(t.Context(), `UPDATE audit_events SET actor_api_key_id=$1`, uuid.NewV7()); err == nil {
		t.Fatal("database allowed two actors")
	}
}

func TestEvidenceDatabaseRejectsContradictoryCredentialShapes(t *testing.T) {
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	user, link, host, runtime := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Account')`, user)
	userEvent, providerEvent, runtimeEvent := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	for _, event := range []audit.Event{
		{ID: userEvent, Evidence: audit.VerifiedDevice(user, link), Action: "identity.sign_in", TargetKind: "login_session", Details: audit.AuthenticationDetails{Method: "device_authorization", DeviceCodeID: link}},
		{ID: providerEvent, Evidence: audit.VerifiedLogin(user, "github"), Action: "identity.sign_in", TargetKind: "login_session", Details: audit.AuthenticationDetails{Method: "provider", Provider: "github", Subject: "subject"}},
		{ID: runtimeEvent, OrgID: uuid.NewV7(), Evidence: audit.RuntimeActor(runtime, 1, 1, host, 1), Action: "mcp.admitted", Outcome: "accepted", TargetKind: "mcp", TargetID: runtime, Details: audit.MCPDetails{Tool: "list_agents"}},
	} {
		if err := db.RunTx(t.Context(), d.Pool, func(tx pgx.Tx) error {
			return audit.Append(t.Context(), tx, auditpolicy.Policy{RetentionSeconds: 3600}, event)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, update string
		id           uuid.UUID
	}{
		{"device sign-in requires consumed credential", "credential_id=NULL", userEvent},
		{"provider incompatible with login session", "credential_kind='login_session'", providerEvent},
		{"provider assertion requires provider", "identity_provider=NULL", providerEvent},
		{"internal execution cannot invent provider", "actor_user_id=NULL,actor_identity=NULL,actor_component='control_plane',authentication_origin='internal',authentication_result='not_attempted',credential_kind=NULL,credential_id=NULL", providerEvent},
		{"unproduced signature category", "credential_kind='request_signature'", userEvent},
		{"internal provenance cannot authenticate user", "authentication_origin='internal'", userEvent},
		{"unverified credential reference", "actor_user_id=NULL,actor_identity=NULL,authentication_result='failed',outcome='denied'", userEvent},
		{"runtime requires authenticated host", "credential_id=NULL", runtimeEvent},
		{"runtime requires host version", "credential_version=NULL", runtimeEvent},
		{"runtime requires generation", "runtime_generation=NULL", runtimeEvent},
		{"runtime cannot borrow login credential", "credential_kind='login_session'", runtimeEvent},
		{"user cannot carry runtime fence", "runtime_generation=1,process_epoch=1", userEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.Pool.Exec(t.Context(), "UPDATE audit_events SET "+tc.update+" WHERE id=$1", tc.id)
			var constraint *pgconn.PgError
			if !errors.As(err, &constraint) || constraint.Code != "23514" {
				t.Fatalf("expected CHECK rejection: %v", err)
			}
		})
	}
}
