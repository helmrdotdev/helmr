package workergroup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// testHostAuthConfig derives the host challenge key from a fixed root key and
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
	if cfg.challengeKey[0] != 1 || cfg.signingKey[0] != 2 {
		t.Fatal("host auth config aliases its keys")
	}
	if _, err := NewHostAuthConfig(hostKey[:8], signingKey, time.Minute); err == nil {
		t.Fatal("short challenge key was accepted")
	}
	if _, err := NewHostAuthConfig(hostKey, signingKey[:8], time.Minute); !errors.Is(err, errInvalidHostCredentialSigningKey) {
		t.Fatalf("short signing key error = %v", err)
	}
}

// newMachineKey generates a worker host machine key.
func newMachineKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// signedHostCredentialRequest issues a challenge for the host and service at
// now and answers it with the machine key, as a worker host does.
func signedHostCredentialRequest(t *testing.T, cfg HostAuthConfig, hostID, serviceID string, key ed25519.PrivateKey, now time.Time) HostCredentialRequest {
	t.Helper()
	challenge, err := IssueHostChallenge(cfg, hostID, serviceID, now)
	if err != nil {
		t.Fatal(err)
	}
	return HostCredentialRequest{
		HostID: hostID, ServiceID: serviceID, Challenge: challenge.Value,
		Signature: ed25519.Sign(key, auth.HostChallengeMessage(challenge.Value)),
	}
}

func TestIssueHostChallengeValidatesIdentifiers(t *testing.T) {
	cfg := testHostAuthConfig(t)
	valid := uuid.NewV7().String()
	for _, test := range []struct {
		name, host, service, input string
	}{
		{name: "host malformed", host: "host", service: valid, input: "worker_host_id must be a canonical UUIDv7"},
		{name: "service malformed", host: valid, service: "service", input: "service_id must be a canonical UUIDv7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var input InputError
			if _, err := IssueHostChallenge(cfg, test.host, test.service, time.Now()); !errors.As(err, &input) || err.Error() != test.input {
				t.Fatalf("error = %v, want input error %q", err, test.input)
			}
		})
	}
}

func TestHostChallengeVerifiesIssuerHostServiceAndExpiry(t *testing.T) {
	cfg := testHostAuthConfig(t)
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	host, service := uuid.NewV7(), uuid.NewV7()
	challenge, err := IssueHostChallenge(cfg, host.String(), service.String(), now)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.ExpiresIn != HostChallengeTTL {
		t.Fatalf("expires in %s, want %s", challenge.ExpiresIn, HostChallengeTTL)
	}
	if !cfg.verifyHostChallenge(challenge.Value, host, service, now) {
		t.Fatal("issued challenge did not verify")
	}
	again, err := IssueHostChallenge(cfg, host.String(), service.String(), now)
	if err != nil {
		t.Fatal(err)
	}
	if again.Value == challenge.Value {
		t.Fatal("challenges for the same host and service repeat")
	}
	foreign, err := NewHostAuthConfig(bytes.Repeat([]byte{3}, auth.MACKeySize), bytes.Repeat([]byte{2}, HostCredentialSigningKeySize), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	payload, mac, _ := strings.Cut(challenge.Value, ".")
	tamperedMAC := []byte(mac)
	tamperedMAC[0] ^= 1
	if tamperedMAC[0] == mac[0] {
		t.Fatal("tampered MAC is unchanged")
	}
	for _, test := range []struct {
		name      string
		cfg       HostAuthConfig
		challenge string
		host      uuid.UUID
		service   uuid.UUID
		at        time.Time
	}{
		{name: "wrong host", cfg: cfg, challenge: challenge.Value, host: uuid.NewV7(), service: service, at: now},
		{name: "wrong service", cfg: cfg, challenge: challenge.Value, host: host, service: uuid.NewV7(), at: now},
		{name: "expired", cfg: cfg, challenge: challenge.Value, host: host, service: service, at: now.Add(HostChallengeTTL)},
		{name: "tampered MAC", cfg: cfg, challenge: payload + "." + string(tamperedMAC), host: host, service: service, at: now},
		{name: "tampered payload", cfg: cfg, challenge: "e30." + mac, host: host, service: service, at: now},
		{name: "foreign issuer", cfg: foreign, challenge: challenge.Value, host: host, service: service, at: now},
		{name: "malformed", cfg: cfg, challenge: payload, host: host, service: service, at: now},
		{name: "empty", cfg: cfg, host: host, service: service, at: now},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.cfg.verifyHostChallenge(test.challenge, test.host, test.service, test.at) {
				t.Fatal("challenge verified")
			}
		})
	}
}

func TestIssueHostCredentialValidatesInOrder(t *testing.T) {
	cfg := testHostAuthConfig(t)
	valid := uuid.NewV7().String()
	foreign := signedHostCredentialRequest(t, cfg, uuid.NewV7().String(), valid, newMachineKey(t), time.Now())
	foreign.HostID = valid
	for _, test := range []struct {
		name     string
		exchange HostCredentialRequest
		input    string
	}{
		{name: "host missing", exchange: HostCredentialRequest{ServiceID: valid}, input: "worker_host_id is required"},
		{name: "host malformed", exchange: HostCredentialRequest{HostID: "host", ServiceID: valid}, input: "worker_host_id must be a canonical UUIDv7"},
		{name: "service malformed", exchange: HostCredentialRequest{HostID: valid, ServiceID: "service"}, input: "service_id must be a canonical UUIDv7"},
		// An unverifiable challenge is refused before the host key is read.
		{name: "challenge missing", exchange: HostCredentialRequest{HostID: valid, ServiceID: valid}},
		{name: "challenge for another host", exchange: foreign},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A call past validation has no database executor.
			_, err := issueHostCredential(t.Context(), db.New(nil), cfg, test.exchange, time.Now)
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

// machineKeyQuerier serves one host's registered public key and refuses to
// authenticate, so a signature that verifies is visible as the call reaching
// AuthenticateWorkerHostKey.
type machineKeyQuerier struct {
	db.Querier
	hostID    uuid.UUID
	publicKey ed25519.PublicKey
}

var errReachedHostKeyAuthentication = errors.New("reached host key authentication")

func (q machineKeyQuerier) GetActiveWorkerHostKey(_ context.Context, hostID pgtype.UUID) (db.GetActiveWorkerHostKeyRow, error) {
	if pgvalue.MustUUIDValue(hostID) != q.hostID {
		return db.GetActiveWorkerHostKeyRow{}, pgx.ErrNoRows
	}
	return db.GetActiveWorkerHostKeyRow{ID: pgvalue.UUID(uuid.NewV7()), PublicKey: q.publicKey}, nil
}

func (machineKeyQuerier) ReturnLostWorkerHost(context.Context, db.ReturnLostWorkerHostParams) (db.ReturnLostWorkerHostRow, error) {
	return db.ReturnLostWorkerHostRow{}, pgx.ErrNoRows
}

func (machineKeyQuerier) AuthenticateWorkerHostKey(context.Context, db.AuthenticateWorkerHostKeyParams) (db.AuthenticateWorkerHostKeyRow, error) {
	return db.AuthenticateWorkerHostKeyRow{}, errReachedHostKeyAuthentication
}

func TestIssueHostCredentialVerifiesTheMachineKeySignature(t *testing.T) {
	cfg := testHostAuthConfig(t)
	key := newMachineKey(t)
	host, service := uuid.NewV7(), uuid.NewV7().String()
	q := machineKeyQuerier{hostID: host, publicKey: key.Public().(ed25519.PublicKey)}
	signed := signedHostCredentialRequest(t, cfg, host.String(), service, key, time.Now())
	if _, err := issueHostCredential(t.Context(), q, cfg, signed, time.Now); !errors.Is(err, errReachedHostKeyAuthentication) {
		t.Fatalf("signed challenge error = %v, want host key authentication", err)
	}
	wrongKey := signedHostCredentialRequest(t, cfg, host.String(), service, newMachineKey(t), time.Now())
	tampered := signed
	tampered.Signature = append([]byte(nil), signed.Signature...)
	tampered.Signature[0] ^= 1
	unbound := signed
	unbound.Signature = ed25519.Sign(key, []byte(signed.Challenge))
	unregistered := signedHostCredentialRequest(t, cfg, uuid.NewV7().String(), service, key, time.Now())
	for name, exchange := range map[string]HostCredentialRequest{
		"another key":           wrongKey,
		"tampered signature":    tampered,
		"missing signature":     {HostID: signed.HostID, ServiceID: signed.ServiceID, Challenge: signed.Challenge},
		"unbound to its domain": unbound,
		"host without a key":    unregistered,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := issueHostCredential(t.Context(), q, cfg, exchange, time.Now); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("error = %v, want ErrUnauthenticated", err)
			}
		})
	}
	expired := func() time.Time { return time.Now().Add(HostChallengeTTL + time.Second) }
	if _, err := issueHostCredential(t.Context(), q, cfg, signed, expired); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired challenge error = %v, want ErrUnauthenticated", err)
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
		HostKeyID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), WorkerEpoch: 7,
		ClaimVersion: 2, GroupClaimVersion: 4,
	}
}

func validExchangeInput() exchangeInput {
	return exchangeInput{ServiceID: uuid.MustParse("00000000-0000-0000-0000-000000000003")}
}
