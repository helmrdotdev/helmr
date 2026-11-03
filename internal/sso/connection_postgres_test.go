package sso

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/sso/ssotest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type connectionFixture struct {
	db     dbtest.Database
	store  *Store
	org    uuid.UUID
	member uuid.UUID
}

func newConnectionFixture(t *testing.T) connectionFixture {
	t.Helper()
	d := dbtest.Open(t)
	if err := schema.Up(t.Context(), d.DSN); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := connectionFixture{db: d, store: store, org: uuid.NewV7(), member: uuid.NewV7()}
	user := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Owner')`, user)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO organizations(id,name,slug) VALUES($1,'Acme','acme')`, f.org)
	dbtest.MustExec(t, t.Context(), d.Pool, `INSERT INTO org_members(id,org_id,user_id,is_owner) VALUES($1,$2,$3,true)`, f.member, f.org, user)
	return f
}

func (f connectionFixture) tx(t *testing.T, fn func(pgx.Tx) error) error {
	t.Helper()
	return db.RunTx(t.Context(), f.db.Pool, fn)
}

func TestConnectionLifecycle(t *testing.T) {
	f := newConnectionFixture(t)
	var oidcID uuid.UUID
	if err := f.tx(t, func(tx pgx.Tx) error {
		c, err := f.store.CreateOIDC(t.Context(), tx, f.org, f.member, OIDCConfig{Issuer: "https://login.example.test/tenant/v2.0", ClientID: "helmr", ClientSecret: "first"})
		oidcID = c.ID
		if err == nil && (c.State != "candidate" || c.OIDC.ClientSecret != "first" || c.TestedAt != nil) {
			t.Fatalf("created %+v", c)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// One candidate at a time.
	err := f.tx(t, func(tx pgx.Tx) error {
		_, err := f.store.CreateSAML(t.Context(), tx, f.org, f.member, samlIdP(ssotest.NewIdP(t)))
		return err
	})
	if !errors.Is(err, ErrCandidateExists) {
		t.Fatalf("second candidate err = %v", err)
	}
	// The secret is stored encrypted and bound to its connection.
	var ciphertext []byte
	if err := f.db.Pool.QueryRow(t.Context(), `SELECT oidc_secret_ciphertext FROM sso_connections WHERE id=$1`, oidcID).Scan(&ciphertext); err != nil || bytes.Contains(ciphertext, []byte("first")) {
		t.Fatalf("ciphertext %x err %v", ciphertext, err)
	}
	// Testing, then rotating, clears the test result.
	if err := f.tx(t, func(tx pgx.Tx) error {
		if err := f.store.MarkTested(t.Context(), tx, f.org, oidcID); err != nil {
			return err
		}
		return f.store.RotateOIDCSecret(t.Context(), tx, f.org, oidcID, "second")
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.tx(t, func(tx pgx.Tx) error {
		c, err := f.store.Get(t.Context(), tx, f.org, oidcID)
		if err == nil && (c.OIDC.ClientSecret != "second" || c.TestedAt != nil) {
			t.Fatalf("after rotation %+v", c)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Another organization cannot read it.
	if err := f.tx(t, func(tx pgx.Tx) error {
		_, err := f.store.Get(t.Context(), tx, uuid.NewV7(), oidcID)
		return err
	}); !errors.Is(err, ErrConnectionNotFound) {
		t.Fatalf("cross-organization read err = %v", err)
	}
	// Discarding frees the candidate slot for a SAML connection.
	idp := ssotest.NewIdP(t)
	if err := f.tx(t, func(tx pgx.Tx) error {
		if err := f.store.DiscardCandidate(t.Context(), tx, f.org, oidcID); err != nil {
			return err
		}
		c, err := f.store.CreateSAML(t.Context(), tx, f.org, f.member, samlIdP(idp))
		if err != nil {
			return err
		}
		changed := samlIdP(idp)
		changed.EntityID = "https://other-idp.example.test"
		if err := f.store.UpdateSAMLMaterial(t.Context(), tx, f.org, c.ID, changed); err == nil {
			t.Fatal("trust identity changed")
		}
		rotated := samlIdP(idp)
		rotated.Certificates = append(rotated.Certificates, ssotest.NewIdP(t).CertPEM)
		return f.store.UpdateSAMLMaterial(t.Context(), tx, f.org, c.ID, rotated)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestConnectionShapeConstraints(t *testing.T) {
	f := newConnectionFixture(t)
	for name, stmt := range map[string]string{
		"oidc with saml fields":       `INSERT INTO sso_connections(id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_secret_ciphertext,oidc_secret_nonce,saml_idp_entity_id,created_by_member_id) VALUES($1,$2,'oidc','candidate','https://i','c',decode(repeat('00',16),'hex'),decode(repeat('00',12),'hex'),'e',$3)`,
		"oidc without secret":         `INSERT INTO sso_connections(id,org_id,protocol,state,oidc_issuer,oidc_client_id,created_by_member_id) VALUES($1,$2,'oidc','candidate','https://i','c',$3)`,
		"http issuer":                 `INSERT INTO sso_connections(id,org_id,protocol,state,oidc_issuer,oidc_client_id,oidc_secret_ciphertext,oidc_secret_nonce,created_by_member_id) VALUES($1,$2,'oidc','candidate','http://i','c',decode(repeat('00',16),'hex'),decode(repeat('00',12),'hex'),$3)`,
		"saml without certificates":   `INSERT INTO sso_connections(id,org_id,protocol,state,saml_idp_entity_id,saml_sso_url,saml_certificates,created_by_member_id) VALUES($1,$2,'saml','candidate','e','https://s','{}',$3)`,
		"active without activated_at": `INSERT INTO sso_connections(id,org_id,protocol,state,saml_idp_entity_id,saml_sso_url,saml_certificates,created_by_member_id) VALUES($1,$2,'saml','active','e','https://s','{x}',$3)`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.db.Pool.Exec(t.Context(), stmt, uuid.NewV7(), f.org, f.member); err == nil {
				t.Fatal("malformed connection accepted")
			}
		})
	}
	// The creator must be a member of the same organization.
	otherOrg, otherMember, otherUser := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.db.Pool, `INSERT INTO users(id,display_name) VALUES($1,'Other')`, otherUser)
	dbtest.MustExec(t, t.Context(), f.db.Pool, `INSERT INTO organizations(id,name,slug) VALUES($1,'Other','other')`, otherOrg)
	dbtest.MustExec(t, t.Context(), f.db.Pool, `INSERT INTO org_members(id,org_id,user_id) VALUES($1,$2,$3)`, otherMember, otherOrg, otherUser)
	_, err := f.db.Pool.Exec(t.Context(), `INSERT INTO sso_connections(id,org_id,protocol,state,saml_idp_entity_id,saml_sso_url,saml_certificates,created_by_member_id) VALUES($1,$2,'saml','candidate','e','https://s','{x}',$3)`, uuid.NewV7(), f.org, otherMember)
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23503" {
		t.Fatalf("cross-organization creator err = %v", err)
	}
}

func TestConsumeAssertionRefusesReplay(t *testing.T) {
	f := newConnectionFixture(t)
	var connection uuid.UUID
	if err := f.tx(t, func(tx pgx.Tx) error {
		c, err := f.store.CreateSAML(t.Context(), tx, f.org, f.member, samlIdP(ssotest.NewIdP(t)))
		connection = c.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a := SAMLAssertion{ID: "_assertion-1", NotOnOrAfter: time.Now().Add(-time.Hour)}
	if err := ConsumeAssertion(t.Context(), f.db.Pool, connection, a); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeAssertion(t.Context(), f.db.Pool, connection, a); !errors.Is(err, ErrAssertionReplayed) {
		t.Fatalf("replay err = %v", err)
	}
	if n, err := purgeConsumedAssertions(t.Context(), f.db.Pool); err != nil || n != 1 {
		t.Fatalf("purged %d err %v", n, err)
	}
	// A candidate tested moments ago can still be discarded and replaced.
	if err := ConsumeAssertion(t.Context(), f.db.Pool, connection, SAMLAssertion{ID: "_test-sign-in", NotOnOrAfter: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := f.tx(t, func(tx pgx.Tx) error {
		if err := f.store.DiscardCandidate(t.Context(), tx, f.org, connection); err != nil {
			return err
		}
		_, err := f.store.CreateSAML(t.Context(), tx, f.org, f.member, samlIdP(ssotest.NewIdP(t)))
		return err
	}); err != nil {
		t.Fatalf("discard after test sign-in: %v", err)
	}
}
