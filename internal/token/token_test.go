package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func testCredentialKey(t *testing.T) auth.CredentialKey {
	t.Helper()
	key, err := auth.NewCredentialKey(make([]byte, auth.CredentialKeySize))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

type unusedBeginner struct{}

func (unusedBeginner) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("unused") }

var testTime = time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)

func TestNewRequiresTransactionsAndKey(t *testing.T) {
	if _, err := New(nil, testCredentialKey(t), nil); err == nil {
		t.Fatal("Token owner without transactions was accepted")
	}
	if _, err := New(unusedBeginner{}, auth.CredentialKey{}, nil); err == nil {
		t.Fatal("Token owner without a credential key was accepted")
	}
	if _, err := NewRegistrar(nil); err == nil {
		t.Fatal("Token wait registrar without transactions was accepted")
	}
}

func TestCreatedCarriesCallbackURLFromAPIOrigin(t *testing.T) {
	tokens, err := New(unusedBeginner{}, testCredentialKey(t), &url.URL{Scheme: "https", Host: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	row := db.Token{ID: pgvalue.UUID(uuid.NewV7()), ExpiresAt: pgvalue.Timestamptz(testTime)}
	created, err := tokens.created(row, "hlmr_pub_secret", "callback-secret")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := created.CallbackURL(), "https://api.example.test/api/token-callbacks/"+pgvalue.UUIDString(row.ID)+"/callback-secret"; got != want {
		t.Fatalf("callback URL = %q, want %q", got, want)
	}
	if created.PublicAccessToken() != "hlmr_pub_secret" || created.Token().ID != row.ID {
		t.Fatalf("created = %+v", created)
	}
	if _, err := tokens.created(db.Token{ID: row.ID}, "hlmr_pub_secret", "callback-secret"); err == nil {
		t.Fatal("Token without expiry was projected as created")
	}
	unconfigured, err := New(unusedBeginner{}, testCredentialKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unconfigured.created(row, "hlmr_pub_secret", "callback-secret"); err == nil {
		t.Fatal("Token created without a callback origin")
	}
}

func TestCreatedTokenReturnsIsolatedCopy(t *testing.T) {
	var created Created
	dbtest.FillSlices(t, &created.token)
	want := fmt.Sprintf("%#v", created.Token())
	returned := created.Token()
	dbtest.MutateSlices(t, &returned)
	if fmt.Sprintf("%#v", returned) == want {
		t.Fatal("mutation did not change the returned value")
	}
	if got := fmt.Sprintf("%#v", created.Token()); got != want {
		t.Fatalf("mutating the returned Token changed the created Token: %s", got)
	}
	if !reflect.DeepEqual(cloneToken(created.token), created.token) {
		t.Fatal("clone differs from the created Token")
	}
}

func TestCompleteRejectsAmbiguousResultBeforeTransaction(t *testing.T) {
	tokens, err := New(unusedBeginner{}, testCredentialKey(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, complete := range map[string]func(json.RawMessage) error{
		"management": func(result json.RawMessage) error {
			_, err := tokens.Complete(t.Context(), Target{}, result, "key")
			return err
		},
		"callback": func(result json.RawMessage) error {
			_, err := tokens.CompleteWithCallback(t.Context(), uuid.NewV7(), "secret", result)
			return err
		},
		"bearer": func(result json.RawMessage) error {
			_, err := tokens.CompleteWithBearer(t.Context(), uuid.NewV7(), "bearer", result)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			var input InputError
			if err := complete(json.RawMessage(`{"a":1,"a":2}`)); !errors.As(err, &input) || input.Error() != "result must be unambiguous JSON" {
				t.Fatalf("ambiguous result error = %v", err)
			}
		})
	}
}

func TestOperationReceiptValidation(t *testing.T) {
	tokenID := uuid.NewV7().String()
	for _, outcome := range []string{"completed", "cancelled", "expired"} {
		raw := []byte(`{"token_id":"` + tokenID + `","outcome":"` + outcome + `"}`)
		if receipt, err := operationReceiptFromJSON(raw); err != nil || receipt.TokenID != tokenID || receipt.Outcome != outcome {
			t.Fatalf("receipt %s = %+v, %v", raw, receipt, err)
		}
	}
	for _, raw := range []string{
		``,
		`{"token_id":"` + tokenID + `","outcome":"pending"}`,
		`{"token_id":"not-a-token","outcome":"completed"}`,
		`{"token_id":"` + tokenID + `","outcome":"completed","extra":true}`,
		`{"token_id":"` + tokenID + `","outcome":"completed"} {}`,
	} {
		if _, err := operationReceiptFromJSON([]byte(raw)); !errors.Is(err, ErrReceiptInvalid) {
			t.Fatalf("receipt %q error = %v", raw, err)
		}
	}
}

func TestReplayOperationResolvesStoredReceipts(t *testing.T) {
	target := Target{ID: pgvalue.UUID(uuid.NewV7())}
	receipt := func(outcome string) []byte {
		return []byte(`{"token_id":"` + pgvalue.UUIDString(target.ID) + `","outcome":"` + outcome + `"}`)
	}
	if _, done, err := replayOperation(t.Context(), nil, db.IdempotencyClaim{Status: "pending"}, target, "completed"); done || err != nil {
		t.Fatalf("pending claim = %v, %v", done, err)
	}
	var expired *ExpiredError
	if _, done, err := replayOperation(t.Context(), nil, db.IdempotencyClaim{Status: "failed", Receipt: receipt("expired")}, target, "cancelled"); !done || !errors.As(err, &expired) {
		t.Fatalf("expired receipt = %v, %v", done, err)
	}
	for name, claim := range map[string]db.IdempotencyClaim{
		"completed other outcome": {Status: "completed", Receipt: receipt("cancelled")},
		"completed other Token":   {Status: "completed", Receipt: []byte(`{"token_id":"` + uuid.NewV7().String() + `","outcome":"completed"}`)},
		"failed not expired":      {Status: "failed", Receipt: receipt("completed")},
		"unknown status":          {Status: "abandoned"},
	} {
		if _, done, err := replayOperation(t.Context(), nil, claim, target, "completed"); !done || !errors.Is(err, ErrReceiptInvalid) {
			t.Fatalf("%s = %v, %v", name, done, err)
		}
	}
}
