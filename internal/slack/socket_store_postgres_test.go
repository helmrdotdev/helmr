package slack

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func socketFixture(t *testing.T) (statusFixture, *CredentialStore, uuid.UUID) {
	t.Helper()
	f, _, org := credentialFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_app_registrations SET transport='socket' WHERE id=$1`, f.registration)
	store, err := NewCredentialStore(f.Pool, bytes.Repeat([]byte{1}, 32), TransportSocket)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreAppCredentials(t.Context(), audit.System("test-fixture"), org, f.User, f.registration, AppCredentials{ClientID: "client", ClientSecret: "secret", AppToken: "xapp-test"}); err != nil {
		t.Fatal(err)
	}
	return f, store, org
}

func TestSocketOwnershipReplacementFencesReceipts(t *testing.T) {
	f, store, _ := socketFixture(t)
	first, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || first == nil {
		t.Fatalf("first claim: %v %v", first, err)
	}
	if second, err := store.acquireSocket(t.Context(), uuid.NewV7()); err != nil || second != nil {
		t.Fatalf("duplicate claim: %v %v", second, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_socket_connections SET expires_at=clock_timestamp()-interval '1 second' WHERE app_registration_id=$1`, f.registration)
	second, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || second == nil || second.ID == first.ID {
		t.Fatalf("replacement: %v %v", second, err)
	}
	if err := store.renewSocket(t.Context(), *first); !errors.Is(err, errSocketClaimLost) {
		t.Fatalf("stale renewal: %v", err)
	}
	if _, err := (socketReceiptDB{pool: f.Pool, claim: *first}).Begin(t.Context()); !errors.Is(err, errSocketClaimLost) {
		t.Fatalf("stale receipt: %v", err)
	}
	if err := store.releaseSocket(t.Context(), *first, "disconnected", "", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.observeSocket(t.Context(), *second, true); err != nil {
		t.Fatalf("stale release altered replacement: %v", err)
	}
}

func TestSocketAuthorityChangesFenceReceipts(t *testing.T) {
	for _, change := range []string{"rotation", "disconnect", "revocation"} {
		t.Run(change, func(t *testing.T) {
			f, store, org := socketFixture(t)
			claim, err := store.acquireSocket(t.Context(), uuid.NewV7())
			if err != nil || claim == nil {
				t.Fatal(err)
			}
			switch change {
			case "rotation":
				err = store.StoreAppCredentials(t.Context(), audit.System("test-fixture"), org, f.User, f.registration, AppCredentials{ClientID: "client", ClientSecret: "replacement", AppToken: "xapp-replacement"})
			case "disconnect":
				err = store.DisconnectConnection(t.Context(), audit.System("test-fixture"), org, f.User, f.Environment, f.publication)
			case "revocation":
				_, err = f.Pool.Exec(t.Context(), `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := store.observeSocket(t.Context(), *claim, true); !errors.Is(err, errSocketClaimLost) {
				t.Fatalf("obsolete owner observed: %v", err)
			}
			if _, err := (socketReceiptDB{pool: f.Pool, claim: *claim}).Begin(t.Context()); !errors.Is(err, errSocketClaimLost) {
				t.Fatalf("obsolete receipt: %v", err)
			}
		})
	}
}

func TestSocketReceiptCannotCommitAfterLeaseExpiry(t *testing.T) {
	f, store, _ := socketFixture(t)
	claim, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	tx, err := (socketReceiptDB{pool: f.Pool, claim: *claim}).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `UPDATE slack_socket_connections SET expires_at=clock_timestamp()-interval '1 second' WHERE app_registration_id=$1`, f.registration); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); !errors.Is(err, errSocketClaimLost) {
		t.Fatalf("expired receipt committed: %v", err)
	}
}

func TestSocketCredentialRotationBypassesActionRequiredBackoff(t *testing.T) {
	f, store, org := socketFixture(t)
	claim, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	if err := store.releaseSocket(t.Context(), *claim, "action_required", "api_rejected", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if claim, err := store.acquireSocket(t.Context(), uuid.NewV7()); err != nil || claim != nil {
		t.Fatal("backoff ignored", err)
	}
	if err := store.StoreAppCredentials(t.Context(), audit.System("test-fixture"), org, f.User, f.registration, AppCredentials{ClientID: "client", ClientSecret: "replacement", AppToken: "xapp-replacement"}); err != nil {
		t.Fatal(err)
	}
	if claim, err := store.acquireSocket(t.Context(), uuid.NewV7()); err != nil || claim == nil || claim.Token != "xapp-replacement" {
		t.Fatal("replacement did not become eligible", err)
	}
}

func TestSocketHealthDoesNotClaimExpiredConnection(t *testing.T) {
	f, store, org := socketFixture(t)
	claim, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	if err := store.observeSocket(t.Context(), *claim, false); err != nil {
		t.Fatal(err)
	}
	view, err := GetConnection(t.Context(), f.Pool, org, f.User, f.Environment, f.Agent)
	if err != nil || view == nil || view.Socket == nil || view.Socket.Status != "connected" || view.Socket.LastConnectedAt == nil {
		t.Fatalf("health unavailable: %+v %v", view, err)
	}
	payload, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Socket map[string]json.RawMessage `json:"socket"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	if string(response.Socket["status"]) != `"connected"` || response.Socket["state"] != nil {
		t.Fatalf("unexpected socket JSON contract: %s", payload)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_socket_connections SET expires_at=clock_timestamp()-interval '1 second' WHERE app_registration_id=$1`, f.registration)
	view, err = GetConnection(t.Context(), f.Pool, org, f.User, f.Environment, f.Agent)
	if err != nil || view.Socket.Status != "reconnecting" || view.Status != "connected" {
		t.Fatalf("lease health confused with authorization: %+v %v", view, err)
	}
}
