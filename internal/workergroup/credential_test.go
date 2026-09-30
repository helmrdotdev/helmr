package workergroup

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
)

// testCredentialConfig derives the host secret key from a fixed root key and
// signs with a fixed signing key.
func testCredentialConfig(t *testing.T) CredentialConfig {
	t.Helper()
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewCredentialConfig(keys.WorkerHost, bytes.Repeat([]byte{2}, TokenSigningKeySize), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewCredentialConfigValidatesKeysAndDefaultsTTL(t *testing.T) {
	hostKey := bytes.Repeat([]byte{1}, auth.MACKeySize)
	signingKey := bytes.Repeat([]byte{2}, TokenSigningKeySize)
	cfg, err := NewCredentialConfig(hostKey, signingKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ttl != DefaultTokenTTL {
		t.Fatalf("ttl = %s, want %s", cfg.ttl, DefaultTokenTTL)
	}
	hostKey[0], signingKey[0] = 9, 9
	if cfg.hostSecretKey[0] != 1 || cfg.signingKey[0] != 2 {
		t.Fatal("credential config aliases its keys")
	}
	if _, err := NewCredentialConfig(hostKey[:8], signingKey, time.Minute); err == nil {
		t.Fatal("short host secret key was accepted")
	}
	if _, err := NewCredentialConfig(hostKey, signingKey[:8], time.Minute); !errors.Is(err, errInvalidTokenSigningKey) {
		t.Fatalf("short signing key error = %v", err)
	}
}

func TestExchangeCredentialValidatesInOrder(t *testing.T) {
	cfg := testCredentialConfig(t)
	valid := uuid.NewV7().String()
	for _, test := range []struct {
		name     string
		exchange CredentialExchange
		input    string
	}{
		{name: "host missing", exchange: CredentialExchange{Secret: "secret", ServiceID: valid}, input: "worker_host_id is required"},
		{name: "host malformed", exchange: CredentialExchange{HostID: "host", Secret: "secret", ServiceID: valid}, input: "worker_host_id must be a canonical UUIDv7"},
		// A blank secret is refused before the service ID is parsed.
		{name: "secret blank", exchange: CredentialExchange{HostID: valid, Secret: " ", ServiceID: "service"}},
		{name: "service malformed", exchange: CredentialExchange{HostID: valid, Secret: "secret", ServiceID: "service"}, input: "service_id must be a canonical UUIDv7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A call past validation has no database executor.
			_, err := ExchangeCredential(t.Context(), db.New(nil), cfg, test.exchange, time.Now)
			var input InputError
			if test.input != "" && (!errors.As(err, &input) || err.Error() != test.input) {
				t.Fatalf("error = %v, want input error %q", err, test.input)
			}
			if test.input == "" && !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("error = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestTokenAuthorityClaims(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	authority := validAuthority()
	input := validExchangeInput()

	claims, err := authority.Claims(input, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if claims.WorkerEpoch != authority.WorkerEpoch || claims.GroupClaimVersion != authority.GroupClaimVersion {
		t.Fatalf("claims = %+v", claims)
	}
	if _, err := issueToken([]byte("01234567890123456789012345678901"), claims); err != nil {
		t.Fatalf("derived claims cannot be issued: %v", err)
	}
}

func TestExchangeInputRejectsMissingServiceID(t *testing.T) {
	input := validExchangeInput()
	input.ServiceID = uuid.Nil()
	if err := input.Validate(); err == nil || !strings.Contains(err.Error(), "service_id") {
		t.Fatalf("error = %v", err)
	}
}

func validAuthority() tokenAuthority {
	return tokenAuthority{
		WorkerGroupID: uuid.MustParse("01900000-0000-7000-8000-000000000401"), WorkerHostID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		CredentialID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), WorkerEpoch: 7,
		ClaimVersion: 2, GroupClaimVersion: 4,
	}
}

func validExchangeInput() exchangeInput {
	return exchangeInput{ServiceID: uuid.MustParse("00000000-0000-0000-0000-000000000003")}
}
