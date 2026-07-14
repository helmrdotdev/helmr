package auth

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestWorkerTokenAuthorityClaimsIntersectsRoles(t *testing.T) {
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	authority := validAuthority()
	authority.CredentialRoles = WorkerRoles{Run: true, Build: true}
	authority.GroupRoles = WorkerRoles{Run: true, Build: true}
	input := validExchangeInput()
	input.SupervisorRoles = WorkerRoles{Run: true}

	claims, err := authority.Claims(input, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(claims.Roles, []string{WorkerRoleRun}) {
		t.Fatalf("roles = %v", claims.Roles)
	}
	if claims.WorkerEpoch != authority.WorkerEpoch || claims.GroupClaimVersion != authority.GroupClaimVersion {
		t.Fatalf("claims = %+v", claims)
	}
	if _, err := IssueWorkerToken([]byte("01234567890123456789012345678901"), claims); err != nil {
		t.Fatalf("derived claims cannot be issued: %v", err)
	}
}

func TestEpochExchangeInputRejectsNonCanonicalProtocolAndMissingServiceID(t *testing.T) {
	input := validExchangeInput()
	input.ServiceID = uuid.Nil
	if err := input.Validate(); err == nil || !strings.Contains(err.Error(), "service_id") {
		t.Fatalf("error = %v", err)
	}
	input = validExchangeInput()
	input.ProtocolVersion = "helmr.worker.v1"
	if err := input.Validate(); err == nil || !strings.Contains(err.Error(), WorkerProtocolVersion) {
		t.Fatalf("error = %v", err)
	}
}

func TestWorkerTokenAuthorityRejectsEmptyRoleIntersection(t *testing.T) {
	authority := validAuthority()
	authority.CredentialRoles = WorkerRoles{Run: true}
	authority.GroupRoles = WorkerRoles{Build: true}
	_, err := authority.Claims(validExchangeInput(), time.Now(), time.Now().Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "intersection is empty") {
		t.Fatalf("error = %v", err)
	}
}

func validAuthority() WorkerTokenAuthority {
	return WorkerTokenAuthority{
		WorkerGroupID: "group-1", WorkerInstanceID: uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		CredentialID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), WorkerEpoch: 7,
		ClaimVersion: 2, GroupClaimVersion: 4,
		CredentialRoles: WorkerRoles{Run: true, Build: true}, GroupRoles: WorkerRoles{Run: true, Build: true},
	}
}

func validExchangeInput() EpochExchangeInput {
	return EpochExchangeInput{
		ServiceID:       uuid.MustParse("00000000-0000-0000-0000-000000000003"),
		SupervisorRoles: WorkerRoles{Run: true, Build: true}, ProtocolVersion: WorkerProtocolVersion,
	}
}
