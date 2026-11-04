package sso

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	ProtocolOIDC = "oidc"
	ProtocolSAML = "saml"
)

var (
	ErrCandidateExists    = errors.New("the organization already has a candidate connection")
	ErrConnectionNotFound = errors.New("SSO connection not found")
)

// Connection is one organization's SSO trust configuration.
type Connection struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	Protocol    string
	State       string
	OIDC        OIDCConfig
	SAML        SAMLIdP
	TestedAt    *time.Time
	CreatedAt   time.Time
	ActivatedAt *time.Time
}

// OIDCConfig is an OIDC connection's trust identity and its client secret.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

// Store reads and writes connections. It does not authorize; callers do.
type Store struct{ sealer sealer }

func NewStore(encryptionKey []byte) (*Store, error) {
	s, err := newSealer(encryptionKey)
	if err != nil {
		return nil, err
	}
	return &Store{sealer: s}, nil
}

// CreateOIDC records a candidate OIDC connection.
func (s *Store) CreateOIDC(ctx context.Context, tx pgx.Tx, org, creator uuid.UUID, cfg OIDCConfig) (Connection, error) {
	cfg.Issuer, cfg.ClientID = strings.TrimSpace(cfg.Issuer), strings.TrimSpace(cfg.ClientID)
	if !strings.HasPrefix(cfg.Issuer, "https://") || cfg.ClientID == "" || cfg.ClientSecret == "" {
		return Connection{}, errors.New("an HTTPS issuer, client ID and client secret are required")
	}
	id := uuid.NewV7()
	ciphertext, nonce, err := s.sealer.seal(org, id, cfg.ClientSecret)
	if err != nil {
		return Connection{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO sso_connections(id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_client_secret_ciphertext,oidc_client_secret_nonce,created_by_member_id)
 VALUES($1,$2,'oidc','candidate',$3,$4,$5,$6,$7)`, id, org, cfg.Issuer, cfg.ClientID, ciphertext, nonce, creator)
	if err != nil {
		return Connection{}, candidateError(err)
	}
	return s.Get(ctx, tx, org, id)
}

// CreateSAML records a candidate SAML connection.
func (s *Store) CreateSAML(ctx context.Context, tx pgx.Tx, org, creator uuid.UUID, idp SAMLIdP) (Connection, error) {
	idp = idp.normalized()
	if err := idp.Validate(); err != nil {
		return Connection{}, err
	}
	id := uuid.NewV7()
	_, err := tx.Exec(ctx, `INSERT INTO sso_connections(id,org_id,protocol,state,saml_entity_id,saml_sso_url,saml_certificates,created_by_member_id)
 VALUES($1,$2,'saml','candidate',$3,$4,$5,$6)`, id, org, idp.EntityID, idp.SSOURL, idp.Certificates, creator)
	if err != nil {
		return Connection{}, candidateError(err)
	}
	return s.Get(ctx, tx, org, id)
}

// RotateOIDCSecret replaces the client secret. Any change clears the test
// result, so a connection must be tested again before activation.
func (s *Store) RotateOIDCSecret(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, secret string) error {
	if secret == "" {
		return errors.New("the OIDC client secret is required")
	}
	ciphertext, nonce, err := s.sealer.seal(org, id, secret)
	if err != nil {
		return err
	}
	return affected(tx.Exec(ctx, `UPDATE sso_connections SET oidc_client_secret_ciphertext=$3,oidc_client_secret_nonce=$4,tested_at=NULL
 WHERE org_id=$1 AND id=$2 AND protocol='oidc' AND state<>'retired'`, org, id, ciphertext, nonce))
}

// UpdateSAMLMaterial replaces the sign-in URL and signing certificates. The
// IdP entity ID is the trust identity and cannot change.
func (s *Store) UpdateSAMLMaterial(ctx context.Context, tx pgx.Tx, org, id uuid.UUID, idp SAMLIdP) error {
	idp = idp.normalized()
	current, err := s.Get(ctx, tx, org, id)
	if err != nil {
		return err
	}
	if current.Protocol != ProtocolSAML || idp.EntityID != current.SAML.EntityID {
		return errors.New("the SAML identity provider entity ID cannot change; create a new connection")
	}
	if err := idp.Validate(); err != nil {
		return err
	}
	return affected(tx.Exec(ctx, `UPDATE sso_connections SET saml_sso_url=$3,saml_certificates=$4,tested_at=NULL
 WHERE org_id=$1 AND id=$2 AND protocol='saml' AND state<>'retired'`, org, id, idp.SSOURL, idp.Certificates))
}

// MarkTested records a successful test. The caller loads the connection with
// Get (which locks it) and validates the test sign-in against that material in
// the same transaction, so a concurrent material change cannot be marked tested.
func (s *Store) MarkTested(ctx context.Context, tx pgx.Tx, org, id uuid.UUID) error {
	return affected(tx.Exec(ctx, `UPDATE sso_connections SET tested_at=clock_timestamp()
 WHERE org_id=$1 AND id=$2 AND state='candidate'`, org, id))
}

// DiscardCandidate removes an unused candidate connection.
func (s *Store) DiscardCandidate(ctx context.Context, tx pgx.Tx, org, id uuid.UUID) error {
	return affected(tx.Exec(ctx, `DELETE FROM sso_connections WHERE org_id=$1 AND id=$2 AND state='candidate'`, org, id))
}

// Get reads one connection with its client secret, locking it for the caller's
// administrative transaction. Sign-in reads must not hold this lock.
func (s *Store) Get(ctx context.Context, tx pgx.Tx, org, id uuid.UUID) (Connection, error) {
	return s.scan(tx.QueryRow(ctx, `SELECT id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_client_secret_ciphertext,oidc_client_secret_nonce,
 saml_entity_id,saml_sso_url,saml_certificates,tested_at,created_at,activated_at
 FROM sso_connections WHERE org_id=$1 AND id=$2 FOR UPDATE`, org, id))
}

// Active reads an organization's active connection without locking it.
func (s *Store) Active(ctx context.Context, q db.DBTX, org uuid.UUID) (Connection, error) {
	return s.scan(q.QueryRow(ctx, `SELECT id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_client_secret_ciphertext,oidc_client_secret_nonce,
 saml_entity_id,saml_sso_url,saml_certificates,tested_at,created_at,activated_at
 FROM sso_connections WHERE org_id=$1 AND state='active'`, org))
}

// ByID reads a connection in any state without locking it.
func (s *Store) ByID(ctx context.Context, q db.DBTX, id uuid.UUID) (Connection, error) {
	return s.scan(q.QueryRow(ctx, `SELECT id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_client_secret_ciphertext,oidc_client_secret_nonce,
 saml_entity_id,saml_sso_url,saml_certificates,tested_at,created_at,activated_at
 FROM sso_connections WHERE id=$1`, id))
}

// HasActive reports whether an organization has an active connection.
func HasActive(ctx context.Context, q db.DBTX, org uuid.UUID) (bool, error) {
	var active bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sso_connections WHERE org_id=$1 AND state='active')`, org).Scan(&active)
	return active, err
}

// ActiveByID reads an active connection without locking it, for callbacks that
// name the connection. The transaction acting on the callback rechecks it.
func (s *Store) ActiveByID(ctx context.Context, q db.DBTX, id uuid.UUID) (Connection, error) {
	return s.scan(q.QueryRow(ctx, `SELECT id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_client_secret_ciphertext,oidc_client_secret_nonce,
 saml_entity_id,saml_sso_url,saml_certificates,tested_at,created_at,activated_at
 FROM sso_connections WHERE id=$1 AND state='active'`, id))
}

// ActiveBySlug reads the active connection of the organization with slug
// without locking it.
func (s *Store) ActiveBySlug(ctx context.Context, q db.DBTX, slug string) (Connection, error) {
	return s.scan(q.QueryRow(ctx, `SELECT c.id,c.org_id,c.protocol,c.state,c.oidc_issuer,c.oidc_client_id,c.oidc_client_secret_ciphertext,c.oidc_client_secret_nonce,
 c.saml_entity_id,c.saml_sso_url,c.saml_certificates,c.tested_at,c.created_at,c.activated_at
 FROM sso_connections c JOIN organizations o ON o.id=c.org_id WHERE o.slug=$1 AND c.state='active'`, slug))
}

func (s *Store) scan(row pgx.Row) (Connection, error) {
	var c Connection
	var issuer, clientID, entityID, ssoURL *string
	var ciphertext, nonce []byte
	var certs []string
	err := row.Scan(&c.ID, &c.OrgID, &c.Protocol, &c.State,
		&issuer, &clientID, &ciphertext, &nonce, &entityID, &ssoURL, &certs, &c.TestedAt, &c.CreatedAt, &c.ActivatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ErrConnectionNotFound
	}
	if err != nil {
		return Connection{}, err
	}
	switch c.Protocol {
	case ProtocolOIDC:
		secret, err := s.sealer.open(c.OrgID, c.ID, ciphertext, nonce)
		if err != nil {
			return Connection{}, err
		}
		c.OIDC = OIDCConfig{Issuer: *issuer, ClientID: *clientID, ClientSecret: secret}
	case ProtocolSAML:
		c.SAML = SAMLIdP{EntityID: *entityID, SSOURL: *ssoURL, Certificates: certs}
	}
	return c, nil
}

// ConsumeAssertion records a SAML assertion ID once per connection. A second
// use within its validity window is a replay.
func ConsumeAssertion(ctx context.Context, q db.DBTX, connection uuid.UUID, a SAMLAssertion) error {
	tag, err := q.Exec(ctx, `INSERT INTO sso_consumed_assertions(connection_id,assertion_id,expires_at) VALUES($1,$2,$3)
 ON CONFLICT DO NOTHING`, connection, a.ID, a.NotOnOrAfter.Add(clockSkew))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAssertionReplayed
	}
	return nil
}

// RunAssertionRetention removes consumed SAML assertion IDs once they can no
// longer be valid.
func RunAssertionRetention(ctx context.Context, q db.DBTX, log *slog.Logger) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		batchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		_, err := purgeConsumedAssertions(batchCtx, q)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "purge consumed SAML assertions", "error", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func purgeConsumedAssertions(ctx context.Context, q db.DBTX) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM sso_consumed_assertions WHERE expires_at<clock_timestamp()`)
	return tag.RowsAffected(), err
}

func affected(tag pgconn.CommandTag, err error) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

func candidateError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "sso_connections_candidate" {
		return ErrCandidateExists
	}
	return err
}
