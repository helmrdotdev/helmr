-- Organization SSO connections. A connection's trust identity (the OIDC issuer
-- and client, or the SAML IdP entity) never changes; replacing it is a new
-- connection. Key material can rotate, and any rotation clears the test result.
CREATE TABLE org_sso_connections (
 id uuid PRIMARY KEY,
 org_id uuid NOT NULL REFERENCES organizations(id),
 protocol text NOT NULL CHECK (protocol IN ('oidc','saml')),
 state text NOT NULL CHECK (state IN ('candidate','active','retired')),
 oidc_issuer text CHECK (oidc_issuer LIKE 'https://%'),
 oidc_client_id text CHECK (oidc_client_id<>''),
 oidc_secret_ciphertext bytea CHECK (octet_length(oidc_secret_ciphertext)>=16),
 oidc_secret_nonce bytea CHECK (octet_length(oidc_secret_nonce)=12),
 saml_idp_entity_id text CHECK (saml_idp_entity_id<>''),
 saml_sso_url text CHECK (saml_sso_url LIKE 'https://%'),
 saml_certificates text[] CHECK (cardinality(saml_certificates)>=1),
 tested_at timestamptz,
 created_by_member_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 activated_at timestamptz,
 retired_at timestamptz,
 FOREIGN KEY (org_id,created_by_member_id) REFERENCES org_members(org_id,id),
 UNIQUE (id,org_id),
 CONSTRAINT org_sso_connections_protocol_shape CHECK (
  (protocol='oidc' AND num_nonnulls(oidc_issuer,oidc_client_id,oidc_secret_ciphertext,oidc_secret_nonce)=4
   AND num_nonnulls(saml_idp_entity_id,saml_sso_url,saml_certificates)=0)
  OR (protocol='saml' AND num_nonnulls(saml_idp_entity_id,saml_sso_url,saml_certificates)=3
   AND num_nonnulls(oidc_issuer,oidc_client_id,oidc_secret_ciphertext,oidc_secret_nonce)=0)
 ),
 CONSTRAINT org_sso_connections_state_shape CHECK (
  (state='candidate' AND activated_at IS NULL AND retired_at IS NULL)
  OR (state='active' AND activated_at IS NOT NULL AND retired_at IS NULL)
  OR (state='retired' AND retired_at IS NOT NULL)
 )
);
CREATE UNIQUE INDEX org_sso_connections_candidate ON org_sso_connections(org_id) WHERE state='candidate';
CREATE UNIQUE INDEX org_sso_connections_active ON org_sso_connections(org_id) WHERE state='active';

-- A validated SAML assertion ID is consumed once per connection until it can no
-- longer be valid.
CREATE TABLE saml_consumed_assertions (
 connection_id uuid NOT NULL REFERENCES org_sso_connections(id) ON DELETE CASCADE,
 assertion_id text NOT NULL CHECK (assertion_id<>''),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY (connection_id,assertion_id)
);
CREATE INDEX saml_consumed_assertions_expiry ON saml_consumed_assertions(expires_at);

-- Required SSO admits only the organization's SSO proof, except that owners
-- keep their ordinary sign-in. sso_session_seconds is how long a browser SSO
-- proof admits the organization.
ALTER TABLE organizations ADD COLUMN sso_required boolean NOT NULL DEFAULT false;
ALTER TABLE organizations ADD COLUMN sso_session_seconds integer NOT NULL DEFAULT 86400 CHECK (sso_session_seconds BETWEEN 86400 AND 2592000);

-- An IdP subject of one connection. It belongs to one user and is never
-- reassigned.
CREATE TABLE org_sso_identities (
 id uuid PRIMARY KEY,
 org_id uuid NOT NULL,
 connection_id uuid NOT NULL,
 subject text NOT NULL CHECK (subject<>''),
 user_id uuid NOT NULL REFERENCES users(id),
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 FOREIGN KEY (connection_id,org_id) REFERENCES org_sso_connections(id,org_id),
 UNIQUE (connection_id,subject),
 UNIQUE (id,org_id,user_id)
);

-- Binds an identity to one membership lifetime. Each identity and each member
-- has at most one live binding.
CREATE TABLE org_sso_member_bindings (
 id uuid PRIMARY KEY,
 org_id uuid NOT NULL,
 identity_id uuid NOT NULL,
 member_id uuid NOT NULL,
 user_id uuid NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 retired_at timestamptz,
 FOREIGN KEY (identity_id,org_id,user_id) REFERENCES org_sso_identities(id,org_id,user_id),
 FOREIGN KEY (org_id,member_id,user_id) REFERENCES org_members(org_id,id,user_id),
 UNIQUE (id,org_id,member_id,user_id)
);
CREATE UNIQUE INDEX org_sso_member_bindings_live_identity ON org_sso_member_bindings(identity_id) WHERE retired_at IS NULL;
CREATE UNIQUE INDEX org_sso_member_bindings_live_member ON org_sso_member_bindings(member_id) WHERE retired_at IS NULL;

-- The SSO proof one browser session holds for one organization. Admission
-- computes its expiry from authenticated_at and the organization's lifetime.
ALTER TABLE login_sessions ADD UNIQUE (id,user_id);
CREATE TABLE browser_sso_proofs (
 id uuid PRIMARY KEY,
 session_id uuid NOT NULL,
 org_id uuid NOT NULL,
 member_id uuid NOT NULL,
 user_id uuid NOT NULL,
 binding_id uuid NOT NULL,
 authenticated_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 revoked_at timestamptz,
 FOREIGN KEY (session_id,user_id) REFERENCES login_sessions(id,user_id),
 FOREIGN KEY (binding_id,org_id,member_id,user_id) REFERENCES org_sso_member_bindings(id,org_id,member_id,user_id),
 UNIQUE (id,user_id)
);
CREATE UNIQUE INDEX browser_sso_proofs_live_session ON browser_sso_proofs(session_id,org_id) WHERE revoked_at IS NULL;
CREATE INDEX browser_sso_proofs_live_member ON browser_sso_proofs(org_id,member_id) WHERE revoked_at IS NULL;
CREATE INDEX browser_sso_proofs_live_binding ON browser_sso_proofs(binding_id) WHERE revoked_at IS NULL;

-- A CLI session approved under SSO records the browser's proof as its source.
ALTER TABLE login_sessions ADD COLUMN sso_proof_id uuid;
ALTER TABLE login_sessions ADD FOREIGN KEY (sso_proof_id,user_id) REFERENCES browser_sso_proofs(id,user_id);
CREATE INDEX login_sessions_sso_proof ON login_sessions(sso_proof_id) WHERE sso_proof_id IS NOT NULL AND revoked_at IS NULL;

-- A browser session signed in through SSO alone has no ordinary source and no
-- ordinary proof time; its organization access comes from its SSO proofs.
ALTER TABLE login_sessions ALTER COLUMN authenticated_at DROP NOT NULL;
ALTER TABLE login_sessions DROP CONSTRAINT login_sessions_shape;
ALTER TABLE login_sessions ADD CONSTRAINT login_sessions_shape CHECK (
 (channel='browser' AND user_id IS NOT NULL
  AND org_id IS NULL AND member_id IS NULL AND sso_proof_id IS NULL
  AND num_nonnulls(identity_id,passkey_id)<=1
  AND (authenticated_at IS NULL)=(num_nonnulls(identity_id,passkey_id)=0))
 OR (channel='cli' AND user_id IS NOT NULL
  AND org_id IS NOT NULL AND member_id IS NOT NULL
  AND num_nonnulls(identity_id,passkey_id,sso_proof_id)=1 AND authenticated_at IS NOT NULL)
 OR (channel='operator' AND operator_id IS NOT NULL AND passkey_id IS NOT NULL
  AND num_nonnulls(org_id,member_id,identity_id,sso_proof_id)=0
  AND authenticated_at IS NOT NULL AND authenticated_at>=issued_at AND authenticated_at<=expires_at)
);

ALTER TABLE audit_events DROP CONSTRAINT audit_user_credential_kind;
ALTER TABLE audit_events ADD CONSTRAINT audit_user_credential_kind CHECK (actor_user_id IS NULL OR credential_kind IN ('login_session','provider_assertion','device_code','email_code','passkey','slack_link','sso_assertion'));
ALTER TABLE audit_events DROP CONSTRAINT audit_events_credential_kind_check;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_credential_kind_check CHECK (credential_kind IN ('login_session','api_key','worker_host','capacity_token','provider_assertion','device_code','email_code','passkey','slack_link','sso_assertion'));
