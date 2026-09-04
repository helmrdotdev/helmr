package auth

import (
	"strings"
	"testing"
	"time"

	"uuid"
)

func TestWorkerTokenAuthorityClaims(t *testing.T) {
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
	if _, err := IssueWorkerToken([]byte("01234567890123456789012345678901"), claims); err != nil {
		t.Fatalf("derived claims cannot be issued: %v", err)
	}
}

func TestEpochExchangeInputRejectsMissingServiceID(t *testing.T) {
	input := validExchangeInput()
	input.ServiceID = uuid.Nil()
	if err := input.Validate(); err == nil || !strings.Contains(err.Error(), "service_id") {
		t.Fatalf("error = %v", err)
	}
}

func validAuthority() WorkerTokenAuthority {
	return WorkerTokenAuthority{
		WorkerGroupID: uuid.MustParse("01900000-0000-7000-8000-000000000401"), WorkerInstanceID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		CredentialID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), WorkerEpoch: 7,
		ClaimVersion: 2, GroupClaimVersion: 4,
	}
}

func validExchangeInput() EpochExchangeInput {
	return EpochExchangeInput{ServiceID: uuid.MustParse("00000000-0000-0000-0000-000000000003")}
}
