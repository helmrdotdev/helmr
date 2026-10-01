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

// testHostAuthConfig derives the host secret key from a fixed root key and
// signs with a fixed signing key.
func testHostAuthConfig(t *testing.T) HostAuthConfig {
	t.Helper()
	keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewHostAuthConfig(keys.WorkerHost, bytes.Repeat([]byte{2}, HostCredentialSigningKeySize), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNewHostAuthConfigValidatesKeysAndDefaultsTTL(t *testing.T) {
	hostKey := bytes.Repeat([]byte{1}, auth.MACKeySize)
	signingKey := bytes.Repeat([]byte{2}, HostCredentialSigningKeySize)
	cfg, err := NewHostAuthConfig(hostKey, signingKey, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ttl != DefaultHostCredentialTTL {
		t.Fatalf("ttl = %s, want %s", cfg.ttl, DefaultHostCredentialTTL)
	}
	hostKey[0], signingKey[0] = 9, 9
	if cfg.hostSecretKey[0] != 1 || cfg.signingKey[0] != 2 {
		t.Fatal("host auth config aliases its keys")
	}
	if _, err := NewHostAuthConfig(hostKey[:8], signingKey, time.Minute); err == nil {
		t.Fatal("short host secret key was accepted")
	}
	if _, err := NewHostAuthConfig(hostKey, signingKey[:8], time.Minute); !errors.Is(err, errInvalidHostCredentialSigningKey) {
		t.Fatalf("short signing key error = %v", err)
	}
}

func TestIssueHostCredentialValidatesInOrder(t *testing.T) {
	cfg := testHostAuthConfig(t)
	valid := uuid.NewV7().String()
	for _, test := range []struct {
		name     string
		exchange HostCredentialRequest
		input    string
	}{
		{name: "host missing", exchange: HostCredentialRequest{Secret: "secret", ServiceID: valid}, input: "worker_host_id is required"},
		{name: "host malformed", exchange: HostCredentialRequest{HostID: "host", Secret: "secret", ServiceID: valid}, input: "worker_host_id must be a canonical UUIDv7"},
		// A blank secret is refused before the service ID is parsed.
		{name: "secret blank", exchange: HostCredentialRequest{HostID: valid, Secret: " ", ServiceID: "service"}},
		{name: "service malformed", exchange: HostCredentialRequest{HostID: valid, Secret: "secret", ServiceID: "service"}, input: "service_id must be a canonical UUIDv7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A call past validation has no database executor.
			_, err := IssueHostCredential(t.Context(), db.New(nil), cfg, test.exchange, time.Now)
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

func TestHostCredentialAuthorityClaims(t *testing.T) {
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
	if _, err := signHostCredential([]byte("01234567890123456789012345678901"), claims); err != nil {
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

func validAuthority() hostCredentialAuthority {
	return hostCredentialAuthority{
		WorkerGroupID: uuid.MustParse("01900000-0000-7000-8000-000000000401"), WorkerHostID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		HostSecretID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), WorkerEpoch: 7,
		ClaimVersion: 2, GroupClaimVersion: 4,
	}
}

func validExchangeInput() exchangeInput {
	return exchangeInput{ServiceID: uuid.MustParse("00000000-0000-0000-0000-000000000003")}
}

func TestGenerateHostSecret(t *testing.T) {
	hashSecret := make([]byte, auth.MACKeySize)
	for index := range hashSecret {
		hashSecret[index] = byte(index + 1)
	}
	generated, err := generateHostSecret(hashSecret)
	if err != nil {
		t.Fatal(err)
	}
	if hostSecretPrefix != "hlmr_wi_" {
		t.Fatalf("hostSecretPrefix = %q, want hlmr_wi_", hostSecretPrefix)
	}
	if !strings.HasPrefix(generated.Raw, hostSecretPrefix) {
		t.Fatalf("raw = %q, want prefix %q", generated.Raw, hostSecretPrefix)
	}
	randomPart := generated.Raw[len(hostSecretPrefix):]
	wantKeyPrefix := hostSecretPrefix + randomPart[:8]
	if generated.KeyPrefix != wantKeyPrefix {
		t.Fatalf("KeyPrefix = %q, want %q", generated.KeyPrefix, wantKeyPrefix)
	}
	hash, err := auth.HashToken(hashSecret, generated.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(generated.SecretHash) != string(hash) {
		t.Fatal("SecretHash is not the keyed hash of the raw secret")
	}
}
