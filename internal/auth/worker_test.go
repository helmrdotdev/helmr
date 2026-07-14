package auth

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

func TestWorkerTokenRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	payload := validWorkerClaims(now)

	token, err := IssueWorkerToken(workerSecret(), payload)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyWorkerToken(workerSecret(), token, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, payload) {
		t.Fatalf("claims = %+v, want %+v", got, payload)
	}
}

func TestWorkerTokenUsesCanonicalClaims(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	token, err := IssueWorkerToken(workerSecret(), validWorkerClaims(now))
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", token)
	}
	header := decodeJWTPart(t, parts[0])
	if header["alg"] != "HS256" || header["typ"] != "JWT" {
		t.Fatalf("header = %+v", header)
	}
	claims := decodeJWTPart(t, parts[1])
	wants := map[string]any{
		"iss": WorkerTokenIssuer, "sub": "worker-1", "aud": []any{WorkerTokenAudience},
		"worker_group_id": "group-1", "worker_instance_id": "worker-1",
		"credential_id": "credential-1", "worker_epoch": float64(7),
		"claim_version": float64(2), "group_claim_version": float64(4),
		"roles": []any{"build", "run"}, "protocol_version": WorkerProtocolVersion,
	}
	for key, want := range wants {
		if !reflect.DeepEqual(claims[key], want) {
			t.Errorf("claim %s = %#v, want %#v", key, claims[key], want)
		}
	}
}

func TestWorkerTokenValidation(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		edit func(*WorkerClaims)
		want string
	}{
		{"group", func(c *WorkerClaims) { c.WorkerGroupID = " " }, "worker_group_id must be nonempty and canonical"},
		{"worker", func(c *WorkerClaims) { c.WorkerInstanceID = " " }, "worker_instance_id must be nonempty and canonical"},
		{"credential", func(c *WorkerClaims) { c.CredentialID = " " }, "credential_id must be nonempty and canonical"},
		{"epoch", func(c *WorkerClaims) { c.WorkerEpoch = 0 }, "worker_epoch must be positive"},
		{"claim version", func(c *WorkerClaims) { c.ClaimVersion = 0 }, "claim_version must be positive"},
		{"group version", func(c *WorkerClaims) { c.GroupClaimVersion = 0 }, "group_claim_version must be positive"},
		{"roles", func(c *WorkerClaims) { c.Roles = nil }, "roles is empty"},
		{"duplicate roles", func(c *WorkerClaims) { c.Roles = []string{WorkerRoleRun, WorkerRoleRun} }, "roles must be sorted and unique"},
		{"unsorted roles", func(c *WorkerClaims) { c.Roles = []string{WorkerRoleRun, WorkerRoleBuild} }, "roles must be sorted and unique"},
		{"unknown role", func(c *WorkerClaims) { c.Roles = []string{"admin"} }, `unsupported worker role "admin"`},
		{"v1", func(c *WorkerClaims) { c.ProtocolVersion = "helmr.worker.v1" }, `protocol_version must be "helmr.worker.v0"`},
		{"issued at", func(c *WorkerClaims) { c.IssuedAt = time.Time{} }, "issued_at is zero"},
		{"expiry", func(c *WorkerClaims) { c.ExpiresAt = c.IssuedAt }, "expires_at must be after issued_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := validWorkerClaims(now)
			tt.edit(&claims)
			_, err := IssueWorkerToken(workerSecret(), claims)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

	if _, err := IssueWorkerToken([]byte("short"), validWorkerClaims(now)); !errors.Is(err, ErrWeakWorkerTokenSecret) {
		t.Fatalf("weak secret error = %v", err)
	}
	if _, err := VerifyWorkerToken(workerSecret(), " ", now); !errors.Is(err, ErrInvalidWorkerToken) {
		t.Fatalf("empty token error = %v", err)
	}
	if _, err := VerifyWorkerToken(workerSecret(), "not-a-token", now); !errors.Is(err, ErrInvalidWorkerToken) {
		t.Fatalf("malformed token error = %v", err)
	}
	if _, err := VerifyWorkerToken(workerSecret(), "not-a-token", time.Time{}); !errors.Is(err, ErrInvalidWorkerToken) {
		t.Fatalf("zero time error = %v", err)
	}
}

func TestVerifyWorkerTokenRejectsInvalidTokens(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	valid, err := IssueWorkerToken(workerSecret(), validWorkerClaims(now))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		token func() string
		at    time.Time
		want  error
	}{
		{"bad signature", func() string { return valid }, now.Add(time.Minute), ErrInvalidWorkerToken},
		{"expired", func() string { return valid }, now.Add(time.Hour), ErrExpiredWorkerToken},
		{"tampered", func() string { return mutateJWTClaim(t, valid, "worker_epoch", float64(8)) }, now.Add(time.Minute), ErrInvalidWorkerToken},
		{"wrong protocol", func() string {
			return signWorkerClaims(t, workerSecret(), now, func(c *workerJWTClaims) { c.ProtocolVersion = "helmr.worker.v1" })
		}, now.Add(time.Minute), ErrInvalidWorkerToken},
		{"unknown role", func() string {
			return signWorkerClaims(t, workerSecret(), now, func(c *workerJWTClaims) { c.Roles = []string{"admin"} })
		}, now.Add(time.Minute), ErrInvalidWorkerToken},
		{"wrong subject", func() string {
			return signWorkerClaims(t, workerSecret(), now, func(c *workerJWTClaims) { c.Subject = "worker-2" })
		}, now.Add(time.Minute), ErrInvalidWorkerToken},
		{"extra audience", func() string {
			return signWorkerClaims(t, workerSecret(), now, func(c *workerJWTClaims) { c.Audience = append(c.Audience, "other") })
		}, now.Add(time.Minute), ErrInvalidWorkerToken},
		{"wrong type", func() string { return signWorkerClaimsWithType(t, workerSecret(), now, "at+jwt") }, now.Add(time.Minute), ErrInvalidWorkerToken},
		{"future issued at", func() string {
			return signWorkerClaims(t, workerSecret(), now, func(c *workerJWTClaims) { c.IssuedAt = jwt.NewNumericDate(now.Add(time.Minute)) })
		}, now, ErrInvalidWorkerToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := workerSecret()
			if tt.name == "bad signature" {
				secret = otherWorkerTokenSecret()
			}
			_, err := VerifyWorkerToken(secret, tt.token(), tt.at)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func validWorkerClaims(now time.Time) WorkerClaims {
	return WorkerClaims{
		WorkerGroupID: "group-1", WorkerInstanceID: "worker-1", CredentialID: "credential-1", WorkerEpoch: 7,
		ClaimVersion: 2, GroupClaimVersion: 4,
		Roles: []string{WorkerRoleBuild, WorkerRoleRun}, ProtocolVersion: WorkerProtocolVersion,
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
}

func signWorkerClaims(t *testing.T, secret []byte, now time.Time, edit func(*workerJWTClaims)) string {
	t.Helper()
	c := validWorkerClaims(now)
	claims := workerJWTClaims{
		WorkerGroupID: c.WorkerGroupID, WorkerInstanceID: c.WorkerInstanceID, CredentialID: c.CredentialID,
		WorkerEpoch: c.WorkerEpoch, ClaimVersion: c.ClaimVersion,
		GroupClaimVersion: c.GroupClaimVersion, Roles: c.Roles, ProtocolVersion: c.ProtocolVersion,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: WorkerTokenIssuer, Subject: c.WorkerInstanceID, Audience: jwt.ClaimStrings{WorkerTokenAudience}, IssuedAt: jwt.NewNumericDate(c.IssuedAt), ExpiresAt: jwt.NewNumericDate(c.ExpiresAt)},
	}
	edit(&claims)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["typ"] = "JWT"
	signed, err := token.SignedString(secret)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func signWorkerClaimsWithType(t *testing.T, secret []byte, now time.Time, typ string) string {
	t.Helper()
	token := signWorkerClaims(t, secret, now, func(*workerJWTClaims) {})
	parts := strings.Split(token, ".")
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

func mutateJWTClaim(t *testing.T, token, key string, value any) string {
	t.Helper()
	parts := strings.Split(token, ".")
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

func workerSecret() []byte           { return []byte("01234567890123456789012345678901") }
func otherWorkerTokenSecret() []byte { return []byte("abcdefabcdefabcdefabcdefabcdef12") }
