DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM audit_events WHERE credential_kind='sso_assertion') OR EXISTS(SELECT 1 FROM login_sessions WHERE sso_proof_id IS NOT NULL) OR EXISTS(SELECT 1 FROM login_sessions WHERE authenticated_at IS NULL) THEN
 RAISE EXCEPTION 'SSO downgrade requires a pre-cutover backup'; END IF;
END $$;
ALTER TABLE audit_events DROP CONSTRAINT audit_events_credential_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_credential_kind_check CHECK (credential_kind IN ('login_session','api_key','worker_host','capacity_token','provider_assertion','device_code','email_code','passkey','slack_link'));
ALTER TABLE audit_events DROP CONSTRAINT audit_user_credential_kind;
ALTER TABLE audit_events ADD CONSTRAINT audit_user_credential_kind CHECK (actor_user_id IS NULL OR credential_kind IN ('login_session','provider_assertion','device_code','email_code','passkey','slack_link'));

ALTER TABLE login_sessions DROP CONSTRAINT login_sessions_shape;
ALTER TABLE login_sessions ADD CONSTRAINT login_sessions_shape CHECK (
 (channel='browser' AND user_id IS NOT NULL
  AND org_id IS NULL AND member_id IS NULL
  AND num_nonnulls(identity_id,passkey_id)=1)
 OR (channel='cli' AND user_id IS NOT NULL
  AND org_id IS NOT NULL AND member_id IS NOT NULL
  AND num_nonnulls(identity_id,passkey_id)=1)
 OR (channel='operator' AND operator_id IS NOT NULL AND passkey_id IS NOT NULL
  AND num_nonnulls(org_id,member_id,identity_id)=0
  AND authenticated_at>=issued_at AND authenticated_at<=expires_at)
);
ALTER TABLE login_sessions ALTER COLUMN authenticated_at SET NOT NULL;

ALTER TABLE login_sessions DROP COLUMN sso_proof_id;
DROP TABLE browser_sso_proofs;
ALTER TABLE login_sessions DROP CONSTRAINT login_sessions_id_user_id_key;
DROP TABLE org_sso_member_bindings;
DROP TABLE org_sso_identities;
ALTER TABLE organizations DROP COLUMN sso_session_seconds, DROP COLUMN sso_required;
DROP TABLE saml_consumed_assertions;
DROP TABLE org_sso_connections;
