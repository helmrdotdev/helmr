package schema

import (
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestAuthenticationFoundationRetainsActorsAndInvalidatesLegacyProof(t *testing.T) {
	database := dbtest.Open(t)
	ctx := t.Context()
	if err := run(ctx, database.DSN, func(m *migrate.Migrate) error { return m.Migrate(2) }); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, database.Pool, `
 INSERT INTO organizations(id,name,slug) VALUES('00000000-0000-0000-0000-000000000001','Organization','organization');
 INSERT INTO users(id,display_name,primary_email,admin) VALUES('00000000-0000-0000-0000-000000000002','Existing user','Owner@Example.test',true);
 INSERT INTO org_members(id,org_id,user_id,is_owner) VALUES('00000000-0000-0000-0000-000000000003','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002',true);
 INSERT INTO auth_identities(id,user_id,provider,subject) VALUES
 ('00000000-0000-0000-0000-000000000004','00000000-0000-0000-0000-000000000002','github','123'),
 ('00000000-0000-0000-0000-000000000005','00000000-0000-0000-0000-000000000002','magic-link','Owner@Example.test');
 INSERT INTO login_sessions(id,org_id,user_id,token_hash,expires_at) VALUES('00000000-0000-0000-0000-000000000006','00000000-0000-0000-0000-000000000001','00000000-0000-0000-0000-000000000002','legacy-token',clock_timestamp()+interval '1 day');
 INSERT INTO device_codes(id,user_code_hash,device_code_hash,expires_at,poll_interval_seconds) VALUES('00000000-0000-0000-0000-000000000007','legacy-user','legacy-device',clock_timestamp()+interval '10 minutes',5);
 `)
	if err := Up(ctx, database.DSN); err != nil {
		t.Fatal(err)
	}
	var valid bool
	err := database.Pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM users WHERE id='00000000-0000-0000-0000-000000000002' AND primary_email='owner@example.test' AND octet_length(webauthn_user_handle)=16) AND
 EXISTS(SELECT 1 FROM org_members WHERE id='00000000-0000-0000-0000-000000000003' AND is_owner AND removed_at IS NULL) AND
 EXISTS(SELECT 1 FROM auth_identities WHERE id='00000000-0000-0000-0000-000000000004' AND revoked_at IS NULL) AND
 EXISTS(SELECT 1 FROM auth_identities WHERE id='00000000-0000-0000-0000-000000000005' AND revoked_at IS NOT NULL) AND
 NOT EXISTS(SELECT 1 FROM login_sessions) AND
 NOT EXISTS(SELECT 1 FROM device_codes) AND NOT EXISTS(SELECT 1 FROM platform_operators)`).Scan(&valid)
	if err != nil || !valid {
		t.Fatalf("retained actors and retired proof: valid=%v err=%v", valid, err)
	}
}
