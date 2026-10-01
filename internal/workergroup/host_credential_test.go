package workergroup

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestHostCredentialRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	payload := validHostCredentialClaims(now)

	credential, err := signHostCredential(testSigningKey(), payload)
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifyHostCredential(testSigningKey(), credential, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, payload) {
		t.Fatalf("claims = %+v, want %+v", got, payload)
	}
}

func TestHostCredentialUsesCanonicalClaims(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	credential, err := signHostCredential(testSigningKey(), validHostCredentialClaims(now))
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(credential, ".")
	if len(parts) != 3 {
		t.Fatalf("credential = %q", credential)
	}
	header := decodeJWTPart(t, parts[0])
	if header["alg"] != "HS256" || header["typ"] != "JWT" {
		t.Fatalf("header = %+v", header)
	}
	claims := decodeJWTPart(t, parts[1])
	wants := map[string]any{
		"iss": "helmr-controlplane", "sub": "worker-1", "aud": []any{"helmr-worker"},
		"worker_group_id": "01900000-0000-7000-8000-000000000501", "worker_host_id": "worker-1",
		"credential_id": "credential-1", "worker_epoch": float64(7),
		"claim_version": float64(2), "group_claim_version": float64(4),
	}
	for key, want := range wants {
		if !reflect.DeepEqual(claims[key], want) {
			t.Errorf("claim %s = %#v, want %#v", key, claims[key], want)
		}
	}
}

func TestHostCredentialValidation(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		edit func(*hostCredentialClaims)
		want string
	}{
		{"group", func(c *hostCredentialClaims) { c.WorkerGroupID = " " }, "worker_group_id must be nonempty and canonical"},
		{"worker", func(c *hostCredentialClaims) { c.WorkerHostID = " " }, "worker_host_id must be nonempty and canonical"},
		{"credential", func(c *hostCredentialClaims) { c.CredentialID = " " }, "credential_id must be nonempty and canonical"},
		{"epoch", func(c *hostCredentialClaims) { c.WorkerEpoch = 0 }, "worker_epoch must be positive"},
		{"claim version", func(c *hostCredentialClaims) { c.ClaimVersion = 0 }, "claim_version must be positive"},
		{"group version", func(c *hostCredentialClaims) { c.GroupClaimVersion = 0 }, "group_claim_version must be positive"},
		{"issued at", func(c *hostCredentialClaims) { c.IssuedAt = time.Time{} }, "issued_at is zero"},
		{"expiry", func(c *hostCredentialClaims) { c.ExpiresAt = c.IssuedAt }, "expires_at must be after issued_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := validHostCredentialClaims(now)
			tt.edit(&claims)
			_, err := signHostCredential(testSigningKey(), claims)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

	if _, err := signHostCredential([]byte("short"), validHostCredentialClaims(now)); !errors.Is(err, errInvalidHostCredentialSigningKey) {
		t.Fatalf("weak secret error = %v", err)
	}
	if _, err := verifyHostCredential(testSigningKey(), " ", now); !errors.Is(err, errInvalidHostCredential) {
		t.Fatalf("empty credential error = %v", err)
	}
	if _, err := verifyHostCredential(testSigningKey(), "not-a-credential", now); !errors.Is(err, errInvalidHostCredential) {
		t.Fatalf("malformed credential error = %v", err)
	}
	if _, err := verifyHostCredential(testSigningKey(), "not-a-credential", time.Time{}); !errors.Is(err, errInvalidHostCredential) {
		t.Fatalf("zero time error = %v", err)
	}
}

func TestVerifyHostCredentialRejectsInvalidCredentials(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	valid, err := signHostCredential(testSigningKey(), validHostCredentialClaims(now))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		credential func() string
		at         time.Time
		want       error
	}{
		{"bad signature", func() string { return valid }, now.Add(time.Minute), errInvalidHostCredential},
		{"expired", func() string { return valid }, now.Add(time.Hour), errExpiredHostCredential},
		{"tampered", func() string { return mutateJWTClaim(t, valid, "worker_epoch", float64(8)) }, now.Add(time.Minute), errInvalidHostCredential},
		{"wrong subject", func() string {
			return signHostCredentialClaims(t, testSigningKey(), now, func(c *jwtClaims) { c.Subject = "worker-2" })
		}, now.Add(time.Minute), errInvalidHostCredential},
		{"deleted issuer", func() string {
			return signHostCredentialClaims(t, testSigningKey(), now, func(c *jwtClaims) { c.Issuer = "helmr-" + "control-plane" })
		}, now.Add(time.Minute), errInvalidHostCredential},
		{"extra audience", func() string {
			return signHostCredentialClaims(t, testSigningKey(), now, func(c *jwtClaims) { c.Audience = append(c.Audience, "other") })
		}, now.Add(time.Minute), errInvalidHostCredential},
		{"wrong type", func() string { return signHostCredentialClaimsWithType(t, testSigningKey(), now, "at+jwt") }, now.Add(time.Minute), errInvalidHostCredential},
		{"future issued at", func() string {
			return signHostCredentialClaims(t, testSigningKey(), now, func(c *jwtClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Minute)) })
		}, now, errInvalidHostCredential},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := testSigningKey()
			if tt.name == "bad signature" {
				secret = otherTestSigningKey()
			}
			_, err := verifyHostCredential(secret, tt.credential(), tt.at)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func validHostCredentialClaims(now time.Time) hostCredentialClaims {
	return hostCredentialClaims{
		WorkerGroupID: "01900000-0000-7000-8000-000000000501", WorkerHostID: "worker-1", CredentialID: "credential-1", WorkerEpoch: 7,
		ClaimVersion: 2, GroupClaimVersion: 4,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
}

func signHostCredentialClaims(t *testing.T, secret []byte, now time.Time, edit func(*jwtClaims)) string {
	t.Helper()
	c := validHostCredentialClaims(now)
	claims := jwtClaims{
		WorkerGroupID: c.WorkerGroupID, WorkerHostID: c.WorkerHostID, CredentialID: c.CredentialID,
		WorkerEpoch: c.WorkerEpoch, ClaimVersion: c.ClaimVersion,
		GroupClaimVersion: c.GroupClaimVersion,
		RegisteredClaims:  jwt.RegisteredClaims{Issuer: HostCredentialIssuer, Subject: c.WorkerHostID, Audience: jwt.ClaimStrings{HostCredentialAudience}, IssuedAt: jwt.NewNumericDate(c.IssuedAt), ExpiresAt: jwt.NewNumericDate(c.ExpiresAt)},
	}
	edit(&claims)
	credential := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	credential.Header["typ"] = "JWT"
	signed, err := credential.SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func signHostCredentialClaimsWithType(t *testing.T, secret []byte, now time.Time, typ string) string {
	t.Helper()
	credential := signHostCredentialClaims(t, secret, now, func(*jwtClaims) {})
	parts := strings.Split(credential, ".")
	claims := decodeJWTPart(t, parts[1])
	header := decodeJWTPart(t, parts[0])
	header["typ"] = typ
	encodedHeader, _ := json.Marshal(header)
	encodedClaims, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(encodedHeader) + "." + base64.RawURLEncoding.EncodeToString(encodedClaims)
	parsed := jwt.New(jwt.SigningMethodHS256)
	_ = parsed
	sig, err := jwt.SigningMethodHS256.Sign(unsigned, secret)
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func mutateJWTClaim(t *testing.T, credential, key string, value any) string {
	t.Helper()
	parts := strings.Split(credential, ".")
	claims := decodeJWTPart(t, parts[1])
	claims[key] = value
	encoded, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(encoded) + "." + parts[2]
}

func decodeJWTPart(t *testing.T, raw string) map[string]any {
	t.Helper()
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func testSigningKey() []byte      { return []byte("01234567890123456789012345678901") }
func otherTestSigningKey() []byte { return []byte("abcdefabcdefabcdefabcdefabcdef12") }
