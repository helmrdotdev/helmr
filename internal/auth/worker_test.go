package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestWorkerTokenRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	payload := WorkerClaims{
		WorkerPoolID: "00000000-0000-0000-0000-000000000101",
		WorkerHostID: "worker-1",
		CredentialID: "00000000-0000-0000-0000-000000000002",
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	}

	token, err := IssueWorkerToken(workerSecret(), payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token = %q", token)
	}

	got, err := VerifyWorkerToken(workerSecret(), token, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkerPoolID != payload.WorkerPoolID || got.WorkerHostID != payload.WorkerHostID || got.CredentialID != payload.CredentialID || !got.IssuedAt.Equal(payload.IssuedAt) || !got.ExpiresAt.Equal(payload.ExpiresAt) {
		t.Fatalf("payload = %+v", got)
	}
}

func TestWorkerTokenUsesJWTClaims(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	token, err := IssueWorkerToken(workerSecret(), WorkerClaims{
		WorkerPoolID: "pool-1",
		WorkerHostID: "worker-1",
		CredentialID: "credential-1",
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", token)
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatal(err)
	}
	if header["alg"] != "HS256" || header["typ"] != "JWT" {
		t.Fatalf("header = %s", headerJSON)
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims["iss"] != WorkerTokenIssuer || claims["sub"] != "worker-1" || claims["worker_pool_id"] != "pool-1" || claims["worker_host_id"] != "worker-1" || claims["credential_id"] != "credential-1" {
		t.Fatalf("claims = %s", claimsJSON)
	}
	audience, ok := claims["aud"].([]any)
	if !ok || len(audience) != 1 || audience[0] != WorkerTokenAudience {
		t.Fatalf("claims = %s", claimsJSON)
	}
}

func TestVerifyWorkerTokenRejectsBadSignature(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	token, err := IssueWorkerToken(workerSecret(), WorkerClaims{
		WorkerPoolID: "pool-1",
		WorkerHostID: "worker-1",
		CredentialID: "credential-1",
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = VerifyWorkerToken(otherWorkerSecret(), token, now.Add(time.Minute))
	if !errors.Is(err, ErrInvalidWorkerToken) {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyWorkerTokenRejectsTamperedPayload(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	token, err := IssueWorkerToken(workerSecret(), WorkerClaims{
		WorkerPoolID: "pool-1",
		WorkerHostID: "worker-1",
		CredentialID: "credential-1",
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token = %q", token)
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	payloadJSON = []byte(strings.Replace(string(payloadJSON), "worker-1", "worker-2", 1))
	tamperedToken := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payloadJSON) + "." + parts[2]

	_, err = VerifyWorkerToken(workerSecret(), tamperedToken, now.Add(time.Minute))
	if !errors.Is(err, ErrInvalidWorkerToken) {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyWorkerTokenRejectsExpiredToken(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	token, err := IssueWorkerToken(workerSecret(), WorkerClaims{
		WorkerPoolID: "pool-1",
		WorkerHostID: "worker-1",
		CredentialID: "credential-1",
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = VerifyWorkerToken(workerSecret(), token, now.Add(time.Hour))
	if !errors.Is(err, ErrExpiredWorkerToken) {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkerTokenValidation(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	_, err := IssueWorkerToken(workerSecret(), WorkerClaims{
		WorkerPoolID: "pool-1",
		WorkerHostID: " ",
		CredentialID: "credential-1",
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	})
	if err == nil || !strings.Contains(err.Error(), "worker_host_id is empty") {
		t.Fatalf("error = %v", err)
	}

	_, err = IssueWorkerToken([]byte("short"), WorkerClaims{
		WorkerPoolID: "pool-1",
		WorkerHostID: "worker-1",
		CredentialID: "credential-1",
		IssuedAt:     now,
		ExpiresAt:    now.Add(time.Hour),
	})
	if !errors.Is(err, ErrWeakWorkerTokenSecret) {
		t.Fatalf("error = %v", err)
	}

	_, err = VerifyWorkerToken(workerSecret(), "not-a-token", now)
	if !errors.Is(err, ErrInvalidWorkerToken) {
		t.Fatalf("error = %v", err)
	}
}

func workerSecret() []byte {
	return []byte("01234567890123456789012345678901")
}

func otherWorkerSecret() []byte {
	return []byte("abcdefabcdefabcdefabcdefabcdef12")
}
