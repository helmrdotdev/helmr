-- Authentication credentials require fresh enrollment under the new proof model.
DELETE FROM login_sessions;
DELETE FROM device_codes;
DROP TABLE magic_links;
ALTER TABLE invitations DROP CONSTRAINT invitations_org_id_id_key;
DROP TYPE magic_link_purpose;

ALTER TABLE users DROP COLUMN admin;
ALTER TABLE users ADD COLUMN webauthn_user_handle bytea NOT NULL DEFAULT uuid_send(gen_random_uuid());
ALTER TABLE users ADD UNIQUE (webauthn_user_handle);
-- New account handles also have an opaque, independently random database default.
UPDATE users SET primary_email=lower(btrim(primary_email)) WHERE primary_email IS NOT NULL;

ALTER TABLE auth_identities DROP COLUMN email;
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
ALTER TABLE login_sessions ADD COLUMN channel text NOT NULL CHECK (channel IN ('browser','cli','operator'));
ALTER TABLE login_sessions ADD COLUMN member_id uuid;
ALTER TABLE login_sessions ADD COLUMN identity_id uuid;
ALTER TABLE login_sessions ADD COLUMN passkey_id uuid;
ALTER TABLE login_sessions ADD COLUMN authenticated_at timestamptz NOT NULL;
ALTER TABLE login_sessions ADD FOREIGN KEY (org_id,member_id,user_id) REFERENCES org_members(org_id,id,user_id);
ALTER TABLE login_sessions ADD FOREIGN KEY (identity_id,user_id) REFERENCES auth_identities(id,user_id);
ALTER TABLE login_sessions ADD FOREIGN KEY (passkey_id,user_id) REFERENCES passkey_credentials(id,user_id);
ALTER TABLE login_sessions ADD FOREIGN KEY (passkey_id,operator_id) REFERENCES passkey_credentials(id,operator_id);
ALTER TABLE login_sessions ADD CONSTRAINT login_sessions_owner CHECK (num_nonnulls(user_id,operator_id)=1);
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

CREATE TABLE email_challenges (
 id uuid PRIMARY KEY,
 email text NOT NULL CHECK (email=lower(btrim(email)) AND email<>''),
 purpose text NOT NULL CHECK (purpose IN ('login','email_change')),
 code_mac bytea NOT NULL,
 generation bigint NOT NULL DEFAULT 0 CHECK (generation>=0),
 failed_attempts integer NOT NULL DEFAULT 0 CHECK (failed_attempts>=0),
 next_send_at timestamptz NOT NULL,
 browser_hash bytea NOT NULL,
 session_id uuid REFERENCES login_sessions(id),
 safe_next text NOT NULL DEFAULT '/',
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 sent_at timestamptz,
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz,
 CHECK (purpose<>'email_change' OR session_id IS NOT NULL)
);

CREATE INDEX email_challenges_recent ON email_challenges(email,created_at DESC);

CREATE TABLE operator_passkey_enrollments (
 id uuid PRIMARY KEY,
 operator_id uuid NOT NULL REFERENCES platform_operators(id),
 capability_hash bytea UNIQUE,
 browser_hash bytea,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz,
 CHECK (num_nonnulls(capability_hash,browser_hash)=1)
);
CREATE TABLE account_repair_enrollments (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES users(id),
 operator_id uuid NOT NULL REFERENCES platform_operators(id),
 capability_hash bytea NOT NULL UNIQUE,
 browser_hash bytea,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz
);
CREATE TABLE webauthn_challenges (
 id uuid PRIMARY KEY,
 ceremony text NOT NULL CHECK (ceremony IN ('register','assert')),
 purpose text NOT NULL CHECK (purpose IN ('login','confirm','register','enroll','repair')),
 operator_scope boolean NOT NULL DEFAULT false,
 verification_state jsonb NOT NULL,
 browser_hash bytea NOT NULL,
 session_id uuid REFERENCES login_sessions(id),
 operator_passkey_enrollment_id uuid REFERENCES operator_passkey_enrollments(id),
 account_repair_enrollment_id uuid REFERENCES account_repair_enrollments(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 expires_at timestamptz NOT NULL,
 consumed_at timestamptz,
 revoked_at timestamptz,
 CHECK ((purpose='login' AND ceremony='assert' AND num_nonnulls(session_id,operator_passkey_enrollment_id,account_repair_enrollment_id)=0)
  OR (purpose IN ('confirm','register') AND session_id IS NOT NULL AND num_nonnulls(operator_passkey_enrollment_id,account_repair_enrollment_id)=0
   AND ((purpose='confirm' AND ceremony='assert') OR (purpose='register' AND ceremony='register')))
  OR (purpose='enroll' AND ceremony='register' AND operator_scope AND operator_passkey_enrollment_id IS NOT NULL AND num_nonnulls(session_id,account_repair_enrollment_id)=0)
  OR (purpose='repair' AND ceremony='register' AND NOT operator_scope AND account_repair_enrollment_id IS NOT NULL AND num_nonnulls(session_id,operator_passkey_enrollment_id)=0))
);
ALTER TABLE device_codes DROP CONSTRAINT device_codes_org_id_decided_by_member_id_fkey;
ALTER TABLE device_codes ADD COLUMN user_id uuid;
ALTER TABLE device_codes ADD COLUMN browser_session_id uuid REFERENCES login_sessions(id);
ALTER TABLE device_codes ADD FOREIGN KEY (org_id,decided_by_member_id,user_id) REFERENCES org_members(org_id,id,user_id);
ALTER TABLE device_codes ADD CHECK (status NOT IN ('approved','consumed') OR
 (user_id IS NOT NULL AND decided_by_member_id IS NOT NULL AND browser_session_id IS NOT NULL));

-- Existing associations must complete fresh, scoped consent.
DELETE FROM slack_user_links;
ALTER TABLE slack_user_links DROP CONSTRAINT slack_user_links_pkey;
ALTER TABLE slack_user_links DROP CONSTRAINT slack_user_links_team_id_user_id_key;
ALTER TABLE slack_user_links ADD COLUMN id uuid PRIMARY KEY;
ALTER TABLE slack_user_links ADD COLUMN installation_id uuid NOT NULL;
ALTER TABLE slack_user_links ADD COLUMN org_id uuid NOT NULL;
ALTER TABLE slack_user_links ADD COLUMN member_id uuid NOT NULL;
ALTER TABLE slack_user_links ADD COLUMN revoked_at timestamptz;
ALTER TABLE slack_user_links ADD FOREIGN KEY (installation_id,org_id,team_id) REFERENCES slack_installations(id,org_id,team_id);
ALTER TABLE slack_user_links ADD FOREIGN KEY (org_id,member_id,user_id) REFERENCES org_members(org_id,id,user_id);
CREATE UNIQUE INDEX slack_user_links_live ON slack_user_links(installation_id,team_id,slack_user_id) WHERE revoked_at IS NULL;
CREATE UNIQUE INDEX slack_user_links_live_account ON slack_user_links(installation_id,user_id) WHERE revoked_at IS NULL;
CREATE TABLE slack_link_requests (
 id uuid PRIMARY KEY,
 user_id uuid NOT NULL REFERENCES users(id),
 member_id uuid NOT NULL,
 FOREIGN KEY(org_id,member_id,user_id) REFERENCES org_members(org_id,id,user_id),
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
ALTER TABLE audit_events DROP CONSTRAINT audit_actor_count,
 DROP CONSTRAINT audit_actor_authentication,
 DROP CONSTRAINT audit_actor_identity_presence,
 DROP CONSTRAINT audit_user_credential_kind,
 DROP CONSTRAINT audit_anonymous_operation;
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_count CHECK (num_nonnulls(actor_operator_id,actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id,actor_component)<=1);
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_authentication CHECK ((num_nonnulls(actor_operator_id,actor_user_id,actor_api_key_id,actor_session_id,actor_worker_host_id,actor_component)=0) = (authentication_result IN ('failed','absent')));
ALTER TABLE audit_events ADD CONSTRAINT audit_actor_identity_presence CHECK ((actor_user_id IS NOT NULL OR actor_operator_id IS NOT NULL) = (actor_identity IS NOT NULL));
ALTER TABLE audit_events ADD CONSTRAINT audit_user_credential_kind CHECK (actor_user_id IS NULL OR credential_kind IN ('login_session','provider_assertion','device_code','email_code','passkey','slack_link'));
ALTER TABLE audit_events ADD CONSTRAINT audit_operator_identity CHECK (actor_operator_id IS NULL OR (actor_identity ?& ARRAY['source','display_name'] AND jsonb_typeof(actor_identity->'source')='string' AND actor_identity->>'source'='operator' AND jsonb_typeof(actor_identity->'display_name')='string' AND octet_length(actor_identity->>'display_name')>0 AND credential_kind IN ('login_session','passkey') AND credential_id IS NOT NULL));
ALTER TABLE audit_events DROP CONSTRAINT audit_events_credential_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_credential_kind_check CHECK (credential_kind IN ('login_session','api_key','worker_host','capacity_token','provider_assertion','device_code','email_code','passkey','slack_link'));

-- Security mail is durable in the same transaction as the account mutation.
CREATE TABLE account_security_notifications (
 id uuid PRIMARY KEY,
 event text NOT NULL CHECK (event IN ('identity.email_changed','identity.credential_added','identity.credential_removed','identity.access_revoked','identity.other_access_revoked','identity.account_repaired')),
 recipient text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts>=0),
 claim_id uuid,
 lease_until timestamptz,
 CHECK ((claim_id IS NULL)=(lease_until IS NULL))
);
CREATE INDEX account_security_notifications_pending ON account_security_notifications(next_attempt_at);

-- Historical link IDs do not retain disposable authorization rows.
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
