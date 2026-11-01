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
 connection_id uuid NOT NULL REFERENCES org_sso_connections(id),
 assertion_id text NOT NULL CHECK (assertion_id<>''),
 expires_at timestamptz NOT NULL,
 PRIMARY KEY (connection_id,assertion_id)
);
CREATE INDEX saml_consumed_assertions_expiry ON saml_consumed_assertions(expires_at);
