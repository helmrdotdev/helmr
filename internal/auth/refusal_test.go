package auth

import (
	"errors"
	"testing"
	"uuid"
)

func TestPermissionRefusalRetainsEvaluatedScopeAndSubject(t *testing.T) {
	org, key, env := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	p := Principal{Kind: PrincipalKindAPIKey, OrgID: org, APIKeyID: key, ProjectID: uuid.NewV7().String(), EnvironmentID: env.String(), CredentialVersion: 3}
	scope := Scope{OrgID: org, ProjectID: p.ProjectID, EnvironmentID: uuid.NewV7().String()}
	denied := errors.New("operation denied")
	err := Require(p, PermissionAuditEventsRead, scope, denied)
	var refusal *PermissionRefusal
	if !errors.Is(err, denied) || !errors.As(err, &refusal) {
		t.Fatal(err)
	}
	if len(refusal.Checks) != 1 {
		t.Fatal(refusal.Checks)
	}
	c := refusal.Checks[0]
	if c.Permission != PermissionAuditEventsRead || c.Scope != scope || c.SubjectAPIKeyID != key || c.SubjectUserID != uuid.Nil() || c.Result != "failed" || c.Reason != "key_scope_mismatch" || c.Evidence != nil {
		t.Fatalf("invented or lost evidence: %+v", c)
	}
}

func TestPermissionPrerequisiteRefusalDoesNotInventLaterChecks(t *testing.T) {
	p := Principal{Kind: PrincipalKindSession, UserID: uuid.NewV7(), OrgID: uuid.NewV7()}
	scope := Scope{OrgID: p.OrgID, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String(), AgentID: uuid.NewV7().String()}
	checks, ok := p.Evaluate(PermissionSessionsSend, scope)
	if ok || len(checks) != 1 || checks[0].Permission != PermissionAgentsUse || checks[0].Reason != "no_matching_grant" || checks[0].SubjectUserID != p.UserID {
		t.Fatal(checks, ok)
	}
}
