DO $$ DECLARE relation text; retained boolean; BEGIN
 FOREACH relation IN ARRAY ARRAY['sessions','turns','turn_messages','commands'] LOOP
  EXECUTE format('SELECT EXISTS(SELECT 1 FROM %I WHERE human_user_id IS NOT NULL)',relation) INTO retained;
  IF retained THEN RAISE EXCEPTION 'cannot remove retained human admission provenance'; END IF;
  EXECUTE format('ALTER TABLE %I DROP COLUMN human_user_id,DROP COLUMN human_org_id,DROP COLUMN human_member_id,DROP COLUMN human_session_id,DROP COLUMN human_slack_link_id,DROP COLUMN human_identity_id,DROP COLUMN human_passkey_id,DROP COLUMN human_access_epoch,DROP COLUMN human_authenticated_at',relation);
 END LOOP;
END $$;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM account_security_notifications) THEN
  RAISE EXCEPTION 'account security notification history requires backup before rollback';
 END IF;
END $$;
DROP TABLE account_security_notifications;
DO $$ BEGIN
 IF EXISTS(SELECT 1 FROM audit_events WHERE action='identity.email_code_requested' OR actor_operator_id IS NOT NULL OR credential_kind IN ('email_code','passkey','slack_link')) THEN
 RAISE EXCEPTION 'authentication downgrade requires a pre-cutover backup'; END IF;
END $$;
ALTER TABLE audit_events DROP CONSTRAINT audit_anonymous_operation;
ALTER TABLE audit_events ADD CHECK (authentication_result NOT IN ('failed','absent') OR outcome IN ('denied','failed') OR (authentication_result='absent' AND action IN ('identity.device_started','identity.magic_link_requested')));
ALTER TABLE audit_events DROP CONSTRAINT audit_actor_check_0;
ALTER TABLE audit_events DROP CONSTRAINT audit_actor_check_1;
ALTER TABLE audit_events DROP CONSTRAINT audit_actor_check_2;
ALTER TABLE audit_events DROP CONSTRAINT audit_actor_check_3;
ALTER TABLE audit_events DROP CONSTRAINT audit_actor_check_4;
ALTER TABLE audit_events DROP CONSTRAINT audit_actor_check_5;
ALTER TABLE audit_events DROP CONSTRAINT audit_actor_check_6;
DROP INDEX audit_events_actor;
ALTER TABLE audit_events DROP COLUMN actor_operator_id;
CREATE INDEX audit_events_actor ON audit_events(org_id,(COALESCE(actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id)),recorded_at DESC,id DESC);
ALTER TABLE audit_events ADD CHECK (num_nonnulls(actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id,actor_component)<=1);
ALTER TABLE audit_events ADD CHECK ((num_nonnulls(actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id,actor_component)=0) = (authentication_result IN ('failed','absent')));
ALTER TABLE audit_events ADD CHECK ((actor_user_id IS NOT NULL) = (actor_identity IS NOT NULL));
ALTER TABLE audit_events ADD CHECK (actor_user_id IS NULL OR
           (actor_identity ?& ARRAY['source','display_name'] AND
            jsonb_typeof(actor_identity->'source')='string' AND actor_identity->>'source'='account' AND
            jsonb_typeof(actor_identity->'display_name')='string' AND octet_length(actor_identity->>'display_name')>0));
ALTER TABLE audit_events ADD CHECK (actor_user_id IS NULL OR credential_kind IN ('login_session','provider_assertion','magic_link','device_code'));
ALTER TABLE audit_events ADD CHECK (actor_user_id IS NULL OR credential_kind NOT IN ('login_session','device_code','magic_link') OR credential_id IS NOT NULL);
ALTER TABLE audit_events ADD CHECK (actor_user_id IS NULL OR credential_kind NOT IN ('provider_assertion') OR identity_provider IS NOT NULL);
ALTER TABLE audit_events DROP CONSTRAINT audit_events_credential_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_credential_kind_check CHECK (credential_kind IN ('login_session','api_key','worker_host','capacity_token','provider_assertion','magic_link','device_code'));
-- Downgrade cannot restore proofless credentials or operator authority. Refuse
-- to discard newly enrolled keys; a deployed rollback uses its pre-cutover backup.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM passkey_credentials) THEN
  RAISE EXCEPTION 'authentication downgrade requires a pre-cutover backup';
 END IF;
END $$;
DROP TABLE slack_link_requests;
DROP TABLE webauthn_challenges;
DROP TABLE account_repair_enrollments;
DROP TABLE credential_enrollments;
DROP TABLE email_challenges;
ALTER TABLE device_codes DROP COLUMN user_id, DROP COLUMN identity_id, DROP COLUMN passkey_id,
 DROP COLUMN authenticated_at, DROP COLUMN access_epoch, DROP COLUMN browser_session_id, DROP COLUMN replaces_session_id;
DELETE FROM login_sessions WHERE user_id IS NULL;
ALTER TABLE login_sessions DROP CONSTRAINT login_sessions_shape, DROP CONSTRAINT login_sessions_owner;
ALTER TABLE login_sessions DROP COLUMN operator_id, DROP COLUMN channel, DROP COLUMN access_epoch,
 DROP COLUMN member_id, DROP COLUMN identity_id, DROP COLUMN passkey_id, DROP COLUMN authenticated_at;
ALTER TABLE login_sessions RENAME COLUMN issued_at TO created_at;
ALTER TABLE login_sessions RENAME COLUMN last_human_activity_at TO last_seen_at;
ALTER TABLE login_sessions ALTER COLUMN user_id SET NOT NULL;
ALTER TABLE login_sessions ADD FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE SET NULL;
ALTER TABLE login_sessions DROP CONSTRAINT login_sessions_user_id_fkey;
ALTER TABLE login_sessions ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
-- Revoked old links convey no usable authorization in the previous application.
DELETE FROM slack_user_links;
ALTER TABLE slack_user_links DROP COLUMN id, DROP COLUMN installation_id, DROP COLUMN org_id, DROP COLUMN member_id,
 DROP COLUMN access_epoch, DROP COLUMN identity_id, DROP COLUMN passkey_id, DROP COLUMN authenticated_at, DROP COLUMN revoked_at;
ALTER TABLE slack_user_links ADD PRIMARY KEY (team_id,slack_user_id);
ALTER TABLE slack_user_links ADD UNIQUE (team_id,user_id);
DROP TABLE passkey_credentials;
DROP TABLE platform_operators;
ALTER TABLE org_members DROP COLUMN suspended_at, DROP COLUMN reauthenticate_after;
ALTER TABLE org_members DROP CONSTRAINT org_members_org_id_id_user_id_key;
ALTER TABLE users DROP COLUMN access_epoch, DROP COLUMN webauthn_user_handle;
ALTER TABLE users ADD COLUMN admin boolean NOT NULL DEFAULT false;
-- Multiple retired lifetimes cannot be represented by the former schema.
DO $$ BEGIN
 IF EXISTS (SELECT 1 FROM auth_identities GROUP BY provider,subject HAVING count(*)>1) THEN
  RAISE EXCEPTION 'authentication downgrade requires a pre-cutover backup';
 END IF;
END $$;
DROP INDEX auth_identities_live_subject;
ALTER TABLE auth_identities DROP COLUMN revoked_at;
ALTER TABLE auth_identities DROP CONSTRAINT auth_identities_user_id_fkey;
ALTER TABLE auth_identities ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE auth_identities DROP CONSTRAINT auth_identities_id_user_id_key;
ALTER TABLE auth_identities ADD UNIQUE (provider,subject);
ALTER TABLE auth_identities RENAME COLUMN observed_email TO email;
ALTER TABLE auth_identities RENAME COLUMN last_authenticated_at TO last_login_at;
CREATE TYPE magic_link_purpose AS ENUM ('login','invite_accept');
ALTER TABLE invitations ADD UNIQUE (org_id,id);
CREATE TABLE magic_links (
    id uuid PRIMARY KEY,
    purpose magic_link_purpose NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    email text NOT NULL,
    org_id uuid,
    invitation_id uuid,
    redirect_after text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    sent_at timestamptz,
    delivery_failed_at timestamptz,
    expires_at timestamptz NOT NULL,
    consumed_at timestamptz,
    consumed_by_user_id uuid,
    revoked_at timestamptz,
    FOREIGN KEY (org_id) REFERENCES organizations(id) ON DELETE CASCADE,
    CONSTRAINT magic_links_purpose_context_check CHECK (
        (purpose='login' AND org_id IS NULL AND invitation_id IS NULL)
        OR (purpose='invite_accept' AND org_id IS NOT NULL AND invitation_id IS NOT NULL)
    ),
    FOREIGN KEY (org_id, invitation_id) REFERENCES invitations(org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (consumed_by_user_id) REFERENCES users(id) ON DELETE SET NULL
);

CREATE INDEX magic_links_active_token_idx ON magic_links(token_hash) WHERE sent_at IS NOT NULL AND consumed_at IS NULL AND revoked_at IS NULL;

CREATE INDEX magic_links_email_purpose_recent_idx ON magic_links(email, purpose, created_at DESC) WHERE delivery_failed_at IS NULL;

CREATE INDEX magic_links_invitation_active_idx ON magic_links(invitation_id, created_at DESC) WHERE invitation_id IS NOT NULL AND sent_at IS NOT NULL AND consumed_at IS NULL AND revoked_at IS NULL;
