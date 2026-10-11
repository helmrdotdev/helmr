package slack

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestPublicationPendingSlotAndCredentialCustody(t *testing.T) {
	f, store, org := credentialFixture(t)
	if err := store.DisconnectPublication(t.Context(), org, f.User, f.Environment, f.publication); err != nil {
		t.Fatal(err)
	}
	var erased bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.disconnected_at IS NOT NULL AND i.credential_ciphertext IS NULL AND i.credential_nonce IS NULL AND i.credential_expires_at IS NULL AND r.retired_at IS NOT NULL AND r.credential_ciphertext IS NULL AND r.credential_nonce IS NULL FROM slack_installations i JOIN slack_app_registrations r ON r.id=i.app_registration_id WHERE i.id=$1`, f.installation).Scan(&erased); err != nil || !erased {
		t.Fatal("disconnect retained credential material", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, f.Deployment)
	var wg sync.WaitGroup
	results := make(chan PublicationSetup, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			setup, err := BeginPublication(t.Context(), f.Pool, org, f.User, f.Environment, f.Agent)
			if err != nil {
				failures <- err
			} else {
				results <- setup
			}
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	var setup PublicationSetup
	count := 0
	for value := range results {
		setup = value
		count++
	}
	if count != 1 {
		t.Fatalf("created %d pending connections", count)
	}
	for err := range failures {
		if !errors.Is(err, ErrPublicationUnavailable) {
			t.Fatal(err)
		}
	}
	secret := AppCredentials{ClientID: "client", ClientSecret: "private-client-secret", SigningSecret: "private-signing-secret"}
	if err := store.StoreAppCredentials(t.Context(), org, f.User, setup.RegistrationID, secret); err != nil {
		t.Fatal(err)
	}
	got, err := store.appCredentials(t.Context(), setup.RegistrationID)
	if err != nil || got != secret {
		t.Fatal("credential round trip failed")
	}
	var ciphertext, nonce []byte
	if err = f.Pool.QueryRow(t.Context(), `SELECT credential_ciphertext,credential_nonce FROM slack_app_registrations WHERE id=$1`, setup.RegistrationID).Scan(&ciphertext, &nonce); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(secret.ClientSecret)) || bytes.Contains(ciphertext, []byte(secret.SigningSecret)) {
		t.Fatal("plaintext credentials persisted")
	}
	for _, scope := range []struct {
		org, id  uuid.UUID
		revision int64
	}{{uuid.NewV7(), setup.RegistrationID, 1}, {org, uuid.NewV7(), 1}, {org, setup.RegistrationID, 2}} {
		if _, err = store.openApp(scope.org, scope.id, scope.revision, ciphertext, nonce); err == nil {
			t.Fatal("credential owner substitution accepted")
		}
	}
	if _, err = store.open(org, setup.RegistrationID, 1, ciphertext, nonce); err == nil {
		t.Fatal("app credentials accepted as installation token")
	}
	changed := secret
	changed.ClientID = "different-client"
	if err = store.StoreAppCredentials(t.Context(), org, f.User, setup.RegistrationID, changed); !errors.Is(err, errInstallationAuthority) {
		t.Fatal("client identity replaced", err)
	}
	changed = secret
	changed.SigningSecret = "rotated-signing-secret"
	if err = store.StoreAppCredentials(t.Context(), org, f.User, setup.RegistrationID, changed); err != nil {
		t.Fatal(err)
	}
	if err = store.StoreAppCredentials(t.Context(), org, uuid.NewV7(), setup.RegistrationID, secret); !errors.Is(err, errInstallationAuthority) {
		t.Fatal("nonmember wrote secrets", err)
	}
	if err = store.DisconnectPublication(t.Context(), org, f.User, f.Environment, setup.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.appCredentials(t.Context(), setup.RegistrationID); !errors.Is(err, errCredentialUnavailable) {
		t.Fatal("retired registration exposed credentials", err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT retired_at IS NOT NULL AND credential_ciphertext IS NULL AND credential_nonce IS NULL AND credential_revision>0 AND client_id='client' FROM slack_app_registrations WHERE id=$1`, setup.RegistrationID).Scan(&erased); err != nil || !erased {
		t.Fatal("pending disconnect retained secrets or erased identity", err)
	}
	next, err := BeginPublication(t.Context(), f.Pool, org, f.User, f.Environment, f.Agent)
	if err != nil || next.ID == setup.ID || next.RegistrationID == setup.RegistrationID {
		t.Fatal("discard revived generation", err)
	}
	var registrations int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_app_registrations`).Scan(&registrations); err != nil || registrations != 3 {
		t.Fatal("failed setup leaked registrations", registrations, err)
	}
}

func TestPublicationGrantPinsIdentityAndRejectsStaleSetup(t *testing.T) {
	f, store, org := credentialFixture(t)
	if err := store.DisconnectPublication(t.Context(), org, f.User, f.Environment, f.publication); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET current_deployment_id=$2 WHERE id=$1`, f.Environment, f.Deployment)
	setup, err := BeginPublication(t.Context(), f.Pool, org, f.User, f.Environment, f.Agent)
	if err != nil {
		t.Fatal(err)
	}
	credentials := AppCredentials{ClientID: "dedicated-client", ClientSecret: "client-secret", SigningSecret: "signing-secret"}
	if err = store.StoreAppCredentials(t.Context(), org, f.User, setup.RegistrationID, credentials); err != nil {
		t.Fatal(err)
	}
	observed, err := store.publicationAuthorization(t.Context(), org, f.User, setup.ID)
	if err != nil {
		t.Fatal(err)
	}
	grant := refreshResult().grant
	grant.app = "dedicated-app"
	installation, err := store.attachPublicationGrant(t.Context(), org, f.User, setup.ID, observed, grant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.attachPublicationGrant(t.Context(), org, f.User, setup.ID, observed, grant); !errors.Is(err, ErrPublicationUnavailable) {
		t.Fatal("repeated callback created another installation", err)
	}
	var pin bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT p.slack_installation_id=$2 AND i.app_registration_id=p.slack_app_registration_id AND r.app_id=i.app_id AND r.organization_id=i.organization_id FROM agent_publications p JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id JOIN slack_installations i ON i.id=p.slack_installation_id WHERE p.id=$1`, setup.ID, installation).Scan(&pin); err != nil || !pin {
		t.Fatal("grant identity not pinned", err)
	}

	observed, err = store.publicationAuthorization(t.Context(), org, f.User, setup.ID)
	if err != nil {
		t.Fatal(err)
	}
	other := grant
	other.bot = "another-bot"
	if _, err = store.attachPublicationGrant(t.Context(), org, f.User, setup.ID, observed, other); !errors.Is(err, ErrPublicationUnavailable) {
		t.Fatal("sender identity replaced", err)
	}
	repaired, err := store.attachPublicationGrant(t.Context(), org, f.User, setup.ID, observed, grant)
	if err != nil || repaired != installation {
		t.Fatal("same identity repair changed generation", err)
	}
	credentials.SigningSecret = "rotated-secret"
	if err = store.StoreAppCredentials(t.Context(), org, f.User, setup.RegistrationID, credentials); err != nil {
		t.Fatal(err)
	}
	if _, err = store.attachPublicationGrant(t.Context(), org, f.User, setup.ID, observed, grant); !errors.Is(err, ErrPublicationUnavailable) {
		t.Fatal("stale registration exchange committed", err)
	}
}
