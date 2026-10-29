-- Authentication credentials require fresh enrollment under the new proof model.
DELETE FROM login_sessions;
DELETE FROM device_codes;
DROP TABLE magic_links;
ALTER TABLE invitations DROP CONSTRAINT invitations_org_id_id_key;
DROP TYPE magic_link_purpose;

ALTER TABLE users DROP COLUMN admin;
ALTER TABLE users ADD COLUMN access_epoch bigint NOT NULL DEFAULT 0 CHECK (access_epoch>=0);
ALTER TABLE users ADD COLUMN webauthn_user_handle bytea NOT NULL DEFAULT uuid_send(gen_random_uuid());
ALTER TABLE users ADD UNIQUE (webauthn_user_handle);
-- New account handles also have an opaque, independently random database default.
UPDATE users SET primary_email=lower(btrim(primary_email)) WHERE primary_email IS NOT NULL;

ALTER TABLE auth_identities RENAME COLUMN email TO observed_email;
ALTER TABLE auth_identities RENAME COLUMN last_login_at TO last_authenticated_at;
ALTER TABLE auth_identities ADD COLUMN revoked_at timestamptz;
ALTER TABLE auth_identities ADD UNIQUE (id,user_id);
ALTER TABLE auth_identities DROP CONSTRAINT auth_identities_provider_subject_key;
CREATE UNIQUE INDEX auth_identities_live_subject ON auth_identities(provider,subject) WHERE revoked_at IS NULL;
-- Existing email aliases do not become additional active credentials.
UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE provider IN ('email','magic-link');
ALTER TABLE auth_identities DROP CONSTRAINT auth_identities_user_id_fkey;
ALTER TABLE auth_identities ADD FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE RESTRICT;

ALTER TABLE org_members ADD COLUMN suspended_at timestamptz;
ALTER TABLE org_members ADD COLUMN reauthenticate_after timestamptz;
ALTER TABLE org_members ADD UNIQUE (org_id,id,user_id);

CREATE TABLE platform_operators (
 id uuid PRIMARY KEY,
 label text NOT NULL CHECK (btrim(label)<>''),
 webauthn_user_handle bytea NOT NULL UNIQUE,
 access_epoch bigint NOT NULL DEFAULT 0 CHECK (access_epoch>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE passkey_credentials (
 id uuid PRIMARY KEY,
 credential_id bytea NOT NULL UNIQUE,
 user_id uuid REFERENCES users(id),
 operator_id uuid REFERENCES platform_operators(id),
 label text NOT NULL CHECK (btrim(label)<>''),
 credential_data jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 revoked_at timestamptz,
 last_used_at timestamptz,
 CHECK (num_nonnulls(user_id,operator_id)=1),
 UNIQUE (id,user_id), UNIQUE (id,operator_id)
);
ALTER TABLE login_sessions DROP CONSTRAINT login_sessions_org_id_fkey;
ALTER TABLE login_sessions DROP CONSTRAINT login_sessions_user_id_fkey;
ALTER TABLE login_sessions ALTER COLUMN user_id DROP NOT NULL;
ALTER TABLE login_sessions ADD FOREIGN KEY (user_id) REFERENCES users(id);
ALTER TABLE login_sessions RENAME COLUMN created_at TO issued_at;
ALTER TABLE login_sessions RENAME COLUMN last_seen_at TO last_human_activity_at;
ALTER TABLE login_sessions ADD COLUMN operator_id uuid REFERENCES platform_operators(id);
ALTER TABLE login_sessions ADD COLUMN channel text NOT NULL DEFAULT 'browser' CHECK (channel IN ('browser','cli','operator'));
ALTER TABLE login_sessions ALTER COLUMN channel DROP DEFAULT;
ALTER TABLE login_sessions ADD COLUMN access_epoch bigint CHECK (access_epoch>=0);
UPDATE login_sessions SET access_epoch=0;
ALTER TABLE login_sessions ADD COLUMN member_id uuid;
ALTER TABLE login_sessions ADD COLUMN identity_id uuid;
ALTER TABLE login_sessions ADD COLUMN passkey_id uuid;
ALTER TABLE login_sessions ADD COLUMN authenticated_at timestamptz;
ALTER TABLE login_sessions ADD FOREIGN KEY (org_id,member_id,user_id) REFERENCES org_members(org_id,id,user_id);
ALTER TABLE login_sessions ADD FOREIGN KEY (identity_id,user_id) REFERENCES auth_identities(id,user_id);
ALTER TABLE login_sessions ADD FOREIGN KEY (passkey_id,user_id) REFERENCES passkey_credentials(id,user_id);
ALTER TABLE login_sessions ADD FOREIGN KEY (passkey_id,operator_id) REFERENCES passkey_credentials(id,operator_id);
ALTER TABLE login_sessions ADD CONSTRAINT login_sessions_owner CHECK (num_nonnulls(user_id,operator_id)=1);
ALTER TABLE login_sessions ADD CONSTRAINT login_sessions_shape CHECK (
 (channel='browser' AND user_id IS NOT NULL AND access_epoch IS NOT NULL
  AND org_id IS NULL AND member_id IS NULL
  AND num_nonnulls(identity_id,passkey_id)=1 AND authenticated_at IS NOT NULL)
 OR (channel='cli' AND user_id IS NOT NULL AND access_epoch IS NOT NULL
  AND org_id IS NOT NULL AND member_id IS NOT NULL AND authenticated_at IS NOT NULL
  AND num_nonnulls(identity_id,passkey_id)=1)
 OR (channel='operator' AND operator_id IS NOT NULL AND passkey_id IS NOT NULL
  AND access_epoch IS NOT NULL AND authenticated_at IS NOT NULL
  AND num_nonnulls(org_id,member_id,identity_id)=0
  AND expires_at<=issued_at+interval '12 hours'
  AND authenticated_at>=issued_at AND authenticated_at<=expires_at)
);

CREATE TABLE email_challenges (
 id uuid PRIMARY KEY,
 email text NOT NULL CHECK (email=lower(btrim(email)) AND email<>''),
 purpose text NOT NULL CHECK (purpose IN ('login','invite','email_change')),
 code_mac bytea NOT NULL,
 generation bigint NOT NULL DEFAULT 0 CHECK (generation>=0),
 failed_attempts integer NOT NULL DEFAULT 0 CHECK (failed_attempts>=0),
 next_send_at timestamptz NOT NULL,
 browser_hash bytea NOT NULL,
 user_id uuid REFERENCES users(id),
 session_id uuid REFERENCES login_sessions(id),
 access_epoch bigint CHECK (access_epoch>=0),
 invitation_id uuid REFERENCES invitations(id),
 safe_next text NOT NULL DEFAULT '/',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 sent_at timestamptz,
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz,
 CHECK ((purpose='login' AND invitation_id IS NULL AND (num_nonnulls(user_id,session_id,access_epoch)=0 OR num_nonnulls(user_id,session_id,access_epoch)=3))
  OR (purpose='invite' AND invitation_id IS NOT NULL AND (num_nonnulls(user_id,session_id,access_epoch)=0 OR num_nonnulls(user_id,session_id,access_epoch)=3))
  OR (purpose='email_change' AND user_id IS NOT NULL AND session_id IS NOT NULL AND access_epoch IS NOT NULL AND invitation_id IS NULL))
);
CREATE INDEX email_challenges_recent ON email_challenges(email,created_at DESC);

CREATE TABLE operator_passkey_enrollments (
 id uuid PRIMARY KEY,
 operator_id uuid NOT NULL REFERENCES platform_operators(id),
 purpose text NOT NULL CHECK (purpose IN ('bootstrap','repair','add_key')),
 capability_hash bytea UNIQUE,
 expected_access_epoch bigint NOT NULL CHECK (expected_access_epoch>=0),
 issuer_session_id uuid REFERENCES login_sessions(id),
 browser_hash bytea,
 provisional_credential jsonb,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz,
 CHECK ((purpose='add_key' AND issuer_session_id IS NOT NULL AND browser_hash IS NOT NULL AND capability_hash IS NULL)
  OR (purpose IN ('bootstrap','repair') AND issuer_session_id IS NULL AND num_nonnulls(capability_hash,browser_hash)=1)),
 CHECK (provisional_credential IS NULL OR browser_hash IS NOT NULL)
);
CREATE TABLE account_repair_enrollments (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES users(id),
 operator_id uuid NOT NULL REFERENCES platform_operators(id),
 capability_hash bytea NOT NULL UNIQUE,
 expected_access_epoch bigint NOT NULL CHECK (expected_access_epoch>=0),
 browser_hash bytea,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz
);
CREATE TABLE webauthn_challenges (
 id uuid PRIMARY KEY,
 ceremony text NOT NULL CHECK (ceremony IN ('register','assert')),
 purpose text NOT NULL CHECK (purpose IN ('login','confirm','renew','register','enroll','repair')),
 operator_scope boolean NOT NULL DEFAULT false,
 verification_state jsonb NOT NULL,
 browser_hash bytea NOT NULL,
 user_id uuid REFERENCES users(id),
 operator_id uuid REFERENCES platform_operators(id),
 session_id uuid REFERENCES login_sessions(id),
 operator_passkey_enrollment_id uuid REFERENCES operator_passkey_enrollments(id),
 account_repair_enrollment_id uuid REFERENCES account_repair_enrollments(id),
 expected_access_epoch bigint CHECK (expected_access_epoch>=0),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz,
 CHECK (num_nonnulls(user_id,operator_id)<=1),
 CHECK (operator_id IS NULL OR operator_scope),
 CHECK (user_id IS NULL OR NOT operator_scope),
 CHECK (purpose NOT IN ('confirm','renew') OR ceremony='assert'),
 CHECK (purpose<>'register' OR (ceremony='register' AND NOT operator_scope)),
 CHECK ((purpose='login' AND ceremony='assert' AND num_nonnulls(user_id,operator_id,session_id,operator_passkey_enrollment_id,account_repair_enrollment_id,expected_access_epoch)=0)
  OR (purpose IN ('confirm','renew','register') AND session_id IS NOT NULL AND expected_access_epoch IS NOT NULL AND num_nonnulls(user_id,operator_id)=1 AND num_nonnulls(operator_passkey_enrollment_id,account_repair_enrollment_id)=0)
  OR (purpose='enroll' AND operator_scope AND operator_id IS NOT NULL AND operator_passkey_enrollment_id IS NOT NULL AND account_repair_enrollment_id IS NULL AND expected_access_epoch IS NOT NULL)
  OR (purpose='repair' AND ceremony='register' AND NOT operator_scope AND user_id IS NOT NULL AND account_repair_enrollment_id IS NOT NULL AND operator_passkey_enrollment_id IS NULL AND expected_access_epoch IS NOT NULL))
);
ALTER TABLE device_codes DROP CONSTRAINT device_codes_org_id_decided_by_member_id_fkey;
ALTER TABLE device_codes ADD COLUMN user_id uuid;
ALTER TABLE device_codes ADD COLUMN identity_id uuid;
ALTER TABLE device_codes ADD COLUMN passkey_id uuid;
ALTER TABLE device_codes ADD COLUMN authenticated_at timestamptz;
ALTER TABLE device_codes ADD COLUMN access_epoch bigint CHECK (access_epoch>=0);
ALTER TABLE device_codes ADD COLUMN browser_session_id uuid REFERENCES login_sessions(id);
ALTER TABLE device_codes ADD COLUMN replaces_session_id uuid REFERENCES login_sessions(id);
ALTER TABLE device_codes ADD FOREIGN KEY (org_id,decided_by_member_id,user_id) REFERENCES org_members(org_id,id,user_id);
ALTER TABLE device_codes ADD FOREIGN KEY (identity_id,user_id) REFERENCES auth_identities(id,user_id);
ALTER TABLE device_codes ADD FOREIGN KEY (passkey_id,user_id) REFERENCES passkey_credentials(id,user_id);
ALTER TABLE device_codes ADD CHECK (status NOT IN ('approved','consumed') OR
 (user_id IS NOT NULL AND decided_by_member_id IS NOT NULL AND authenticated_at IS NOT NULL AND access_epoch IS NOT NULL AND browser_session_id IS NOT NULL AND num_nonnulls(identity_id,passkey_id)=1));

-- Existing associations must complete fresh, scoped consent.
DELETE FROM slack_user_links;
ALTER TABLE slack_user_links DROP CONSTRAINT slack_user_links_pkey;
ALTER TABLE slack_user_links DROP CONSTRAINT slack_user_links_team_id_user_id_key;
ALTER TABLE slack_user_links ADD COLUMN id uuid NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY;
ALTER TABLE slack_user_links ALTER COLUMN id DROP DEFAULT;
ALTER TABLE slack_user_links ADD COLUMN installation_id uuid;
ALTER TABLE slack_user_links ADD COLUMN org_id uuid;
ALTER TABLE slack_user_links ADD COLUMN member_id uuid;
ALTER TABLE slack_user_links ADD COLUMN access_epoch bigint CHECK (access_epoch>=0);
ALTER TABLE slack_user_links ADD COLUMN identity_id uuid;
ALTER TABLE slack_user_links ADD COLUMN passkey_id uuid;
ALTER TABLE slack_user_links ADD COLUMN authenticated_at timestamptz;
ALTER TABLE slack_user_links ADD COLUMN revoked_at timestamptz;
ALTER TABLE slack_user_links ADD COLUMN resume_previous_delivery boolean NOT NULL DEFAULT false;
ALTER TABLE slack_user_links ADD FOREIGN KEY (installation_id,org_id,team_id) REFERENCES slack_installations(id,org_id,team_id);
ALTER TABLE slack_user_links ADD FOREIGN KEY (org_id,member_id,user_id) REFERENCES org_members(org_id,id,user_id);
ALTER TABLE slack_user_links ADD FOREIGN KEY (identity_id,user_id) REFERENCES auth_identities(id,user_id);
ALTER TABLE slack_user_links ADD FOREIGN KEY (passkey_id,user_id) REFERENCES passkey_credentials(id,user_id);
ALTER TABLE slack_user_links ADD CHECK (installation_id IS NOT NULL AND org_id IS NOT NULL AND member_id IS NOT NULL AND access_epoch IS NOT NULL AND authenticated_at IS NOT NULL AND num_nonnulls(identity_id,passkey_id)=1);
CREATE UNIQUE INDEX slack_user_links_live ON slack_user_links(installation_id,team_id,slack_user_id) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX slack_user_links_live_account ON slack_user_links(installation_id,user_id) WHERE revoked_at IS NULL;
CREATE TABLE slack_link_requests (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES users(id),
 member_id uuid NOT NULL,
 identity_id uuid,
 passkey_id uuid,
 CHECK(num_nonnulls(identity_id,passkey_id)=1),
 FOREIGN KEY(org_id,member_id,user_id) REFERENCES org_members(org_id,id,user_id),
 FOREIGN KEY(identity_id,user_id) REFERENCES auth_identities(id,user_id),
 FOREIGN KEY(passkey_id,user_id) REFERENCES passkey_credentials(id,user_id),
 installation_id uuid NOT NULL,
 org_id uuid NOT NULL,
 team_id text NOT NULL,
 slack_user_id text NOT NULL,
 browser_hash bytea NOT NULL,
 browser_session_id uuid NOT NULL REFERENCES login_sessions(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz,
 FOREIGN KEY (installation_id,org_id,team_id) REFERENCES slack_installations(id,org_id,team_id)
);

-- Operator audit actors are independent of account actors; retained IDs have no live FK.
ALTER TABLE audit_events ADD COLUMN actor_operator_id uuid;
DROP INDEX audit_events_actor;
CREATE INDEX audit_events_actor ON audit_events(org_id,(COALESCE(actor_operator_id,actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id)),recorded_at DESC,id DESC);
DO $$ DECLARE c record; BEGIN
 FOR c IN SELECT conname FROM pg_constraint WHERE conrelid='audit_events'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%actor_user_id%' LOOP
  EXECUTE format('ALTER TABLE audit_events DROP CONSTRAINT %I',c.conname);
 END LOOP;
END $$;
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_check_0 CHECK (num_nonnulls(actor_operator_id,actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id,actor_component)<=1);
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_check_1 CHECK ((num_nonnulls(actor_operator_id,actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id,actor_component)=0) = (authentication_result IN ('failed','absent')));
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_check_2 CHECK ((actor_user_id IS NOT NULL OR actor_operator_id IS NOT NULL) = (actor_identity IS NOT NULL));
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_check_3 CHECK (actor_user_id IS NULL OR
           (actor_identity ?& ARRAY['source','display_name'] AND
            jsonb_typeof(actor_identity->'source')='string' AND actor_identity->>'source'='account' AND
            jsonb_typeof(actor_identity->'display_name')='string' AND octet_length(actor_identity->>'display_name')>0));
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_check_4 CHECK (actor_user_id IS NULL OR credential_kind IN ('login_session','provider_assertion','magic_link','device_code','email_code','passkey','slack_link'));
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_check_5 CHECK (actor_user_id IS NULL OR credential_kind NOT IN ('login_session','device_code','magic_link') OR credential_id IS NOT NULL);
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_check_6 CHECK (actor_user_id IS NULL OR credential_kind NOT IN ('provider_assertion') OR identity_provider IS NOT NULL);
ALTER TABLE audit_events ADD CONSTRAINT audit_operator_identity CHECK (actor_operator_id IS NULL OR (actor_identity ?& ARRAY['source','display_name'] AND jsonb_typeof(actor_identity->'source')='string' AND actor_identity->>'source'='operator' AND jsonb_typeof(actor_identity->'display_name')='string' AND octet_length(actor_identity->>'display_name')>0 AND credential_kind IN ('login_session','passkey') AND credential_id IS NOT NULL));
ALTER TABLE audit_events DROP CONSTRAINT audit_events_credential_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_credential_kind_check CHECK (credential_kind IN ('login_session','api_key','worker_host','capacity_token','provider_assertion','magic_link','device_code','email_code','passkey','slack_link'));

-- Security mail is durable in the same transaction as the account mutation.
CREATE TABLE account_security_notifications (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL,
 event text NOT NULL CHECK (event IN ('identity.email_changed','identity.credential_added','identity.credential_removed','identity.access_revoked','identity.account_repaired')),
 recipient text NOT NULL,
 status text NOT NULL CHECK (status IN ('pending','delivered')),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts>=0),
 claim_id uuid,
 lease_until timestamptz,
 delivered_at timestamptz,
 CHECK ((status='delivered')=(delivered_at IS NOT NULL)),
 CHECK ((claim_id IS NULL)=(lease_until IS NULL))
);
CREATE INDEX account_security_notifications_pending ON account_security_notifications(next_attempt_at) WHERE status='pending';

DO $$ DECLARE c record; BEGIN
 FOR c IN SELECT conname FROM pg_constraint WHERE conrelid='audit_events'::regclass AND contype='c' AND pg_get_constraintdef(oid) LIKE '%identity.magic_link_requested%' LOOP
  EXECUTE format('ALTER TABLE audit_events DROP CONSTRAINT %I',c.conname);
 END LOOP;
END $$;
ALTER TABLE audit_events ADD CONSTRAINT audit_anonymous_operation CHECK (authentication_result NOT IN ('failed','absent') OR outcome IN ('denied','failed') OR (authentication_result='absent' AND action IN ('identity.device_started','identity.magic_link_requested','identity.email_code_requested')));

-- Delivery consent and input presentation belong to the Slack transport.
-- Historical link IDs do not retain disposable authorization rows.
ALTER TABLE slack_threads ADD COLUMN initial_recipient_link_id uuid,
 ADD CHECK (left(slack_channel_id,1)<>'D' OR initial_recipient_link_id IS NOT NULL);
ALTER TABLE slack_requests ADD COLUMN source_link_id uuid,
 ADD CHECK (status<>'accepted' OR operation NOT IN ('start','enqueue') OR source_link_id IS NOT NULL);
CREATE UNIQUE INDEX slack_requests_turn_admission ON slack_requests(environment_id,session_id,turn_id)
 WHERE status='accepted' AND operation IN ('start','enqueue');

-- Related-work lookup retains initiator/contributor provenance independently of access.
CREATE INDEX sessions_related_user ON sessions(environment_id,origin_user_id,id) WHERE origin_user_id IS NOT NULL AND parent_session_id IS NULL AND requester_session_id IS NULL;
CREATE INDEX turns_related_user ON turns(environment_id,caller_id,session_id) WHERE caller_kind='user';
CREATE INDEX turn_messages_related_user ON turn_messages(environment_id,caller_id,session_id) WHERE caller_kind='user';
CREATE INDEX commands_related_user ON commands(environment_id,caller_id,id) WHERE caller_kind='user' AND (terminal_at IS NULL OR (computer_lease_epoch IS NOT NULL AND process_reconciled_at IS NULL));

CREATE INDEX turn_asks_related_user ON turn_asks(environment_id,answered_by_user_id,session_id) WHERE answered_by_user_id IS NOT NULL;

ALTER TABLE slack_posts DROP COLUMN source_thread_id;
