package auditquery

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func projectionRecord(t *testing.T, evidence audit.Evidence, action string, details any) (Record, readScope) {
	t.Helper()
	org, project, env := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	definition, ok := audit.Lookup(action)
	if !ok {
		t.Fatal(action)
	}
	z, err := json.Marshal(evidence.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	r := Record{ID: uuid.NewV7(), OrgID: &org, ProjectID: &project, EnvironmentID: &env, SchemaVersion: 1, OccurredAt: time.Now(), RecordedAt: time.Now(), RetentionSeconds: 3600, ExpiresAt: time.Now().Add(time.Hour), Action: action, Outcome: definition.Outcomes[0], TargetKind: definition.TargetKind, TargetResolution: "verified", OperationID: uuid.NewV7(), Channel: "http", Authorization: z, Causality: json.RawMessage(`{}`), Details: body, AuthenticationResult: evidence.Authentication.Result, AuthenticationOrigin: evidence.Authentication.Origin}
	nonzero := func(id uuid.UUID) *uuid.UUID {
		if id == uuid.Nil() {
			return nil
		}
		return &id
	}
	r.ActorUserID = nonzero(evidence.Actor.UserID)
	r.ActorAPIKeyID = nonzero(evidence.Actor.APIKeyID)
	r.ActorSessionID = nonzero(evidence.Actor.SessionID)
	r.ActorWorkerHostID = nonzero(evidence.Actor.WorkerHostID)
	if evidence.Actor.Component != "" {
		r.ActorComponent = &evidence.Actor.Component
	}
	if evidence.Authentication.CredentialKind != "" {
		r.CredentialKind = &evidence.Authentication.CredentialKind
	}
	if evidence.Authentication.Provider != "" {
		r.IdentityProvider = &evidence.Authentication.Provider
	}
	r.CredentialID = nonzero(evidence.Authentication.CredentialID)
	if evidence.Authentication.CredentialVersion != 0 {
		r.CredentialVersion = &evidence.Authentication.CredentialVersion
	}
	if evidence.RuntimeGeneration != 0 {
		r.RuntimeGeneration = &evidence.RuntimeGeneration
	}
	if evidence.ProcessEpoch != 0 {
		r.ProcessEpoch = &evidence.ProcessEpoch
	}
	return r, readScope{org: org, organizationWide: true}
}

func TestAuditTypedProjectionWithholdsWorkerEvidence(t *testing.T) {
	host, sessionID, checkpoint := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	for _, actor := range []audit.Evidence{audit.RuntimeActor(sessionID, 42, 17, host, 3), audit.AssignedWorker(host, 3, "computer", uuid.NewV7(), 17), audit.Internal(audit.ControlPlane)} {
		r, scope := projectionRecord(t, actor, "computer.checkpoint_published", audit.CheckpointDetails{CheckpointID: checkpoint, Generation: 17})
		peer := "10.20.30.40"
		r.SourceIP = &peer
		before, _ := json.Marshal(r)
		public, err := projectRecord(r, scope)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(public)
		for _, secret := range []string{host.String(), peer, "worker_host", "control_plane", "runtime_generation", "process_epoch", "host_assignment", "runtime_binding", "\"generation\""} {
			if bytes.Contains(encoded, []byte(secret)) {
				t.Fatalf("platform evidence escaped: %s in %s", secret, encoded)
			}
		}
		if public.Authentication.Result != r.AuthenticationResult || public.Authorization.Decision != actor.Authorization.Decision {
			t.Fatal("projection changed observed result")
		}
		want := api.AuditActorPlatformService
		if actor.Actor.SessionID != uuid.Nil() {
			want = api.AuditActorSession
		}
		if public.ActorKind != want || !bytes.Contains(encoded, []byte(checkpoint.String())) {
			t.Fatalf("lost customer fact: %s", encoded)
		}
		after, _ := json.Marshal(r)
		if !bytes.Equal(before, after) {
			t.Fatal("projection rewrote capture")
		}
	}
}

func TestAuditTypedProjectionRetainsHumanOperatorAndRestrictsSearch(t *testing.T) {
	user, credential, host := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	actor, err := audit.PlatformOperator(auth.Principal{Kind: auth.PrincipalKindSession, UserID: user, SessionID: credential, Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	details := audit.SearchDetails{ReadModel: "platform", From: time.Now().Add(-time.Hour), To: time.Now(), ResultCount: 31, ActorID: host, ActorKind: "worker_host", TargetID: host, TargetKind: "worker_host"}
	r, scope := projectionRecord(t, actor, "audit.searched", details)
	r.ActorIdentity = &IdentitySnapshot{Source: "account", DisplayName: "Historical operator", Email: "operator@example.test"}
	public, err := projectRecord(r, scope)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(public)
	if public.ActorKind != api.AuditActorUser || public.ActorUserID == nil || *public.ActorUserID != user || public.ActorIdentity.Email != "operator@example.test" || !public.Authorization.PlatformAuthority {
		t.Fatalf("lost operator accountability: %s", encoded)
	}
	for _, hidden := range []string{credential.String(), host.String(), "result_count", "worker_host"} {
		if bytes.Contains(encoded, []byte(hidden)) {
			t.Fatalf("operator evidence leak: %s", encoded)
		}
	}
	stale := audit.UserAttributeCheck{Type: "user_attribute", UserID: user, Attribute: "admin", Expected: true, Observed: false, ObservationSource: "transaction", Scope: audit.CheckScope{Kind: "platform"}, Result: "failed"}
	actor.Authorization.Decision = "denied"
	actor.Authorization.Checks = append(actor.Authorization.Checks, stale)
	r, scope = projectionRecord(t, actor, "audit.searched", audit.FailureDetails{Reason: "platform_authority_required", Operation: "GET /admin/api/v1/audit-events"})
	r.Outcome = "denied"
	public, err = projectRecord(r, scope)
	if err != nil {
		t.Fatal(err)
	}
	if public.Authorization.PlatformAuthority || public.Authorization.Decision != "denied" {
		t.Fatal("stale admin observation labeled an operator")
	}
}

func TestAuditTypedProjectionRejectsUnknownOrMalformedEvidence(t *testing.T) {
	for _, bad := range []string{`null`, `[]`, `{"worker_host_id":"hidden"}`, `{} {}`} {
		r, scope := projectionRecord(t, audit.Internal(audit.ControlPlane), "secret.created", struct{}{})
		r.Details = json.RawMessage(bad)
		if _, err := projectRecord(r, scope); err == nil {
			t.Fatalf("accepted malformed details %s", bad)
		}
	}
	for _, bad := range []string{`null`, `{"decision":"not_evaluated"}`, `{"decision":"not_evaluated","checks":null}`, `{"decision":"allowed","checks":[{"type":"new_internal_check","host_id":"hidden"}]}`} {
		r, scope := projectionRecord(t, audit.Internal(audit.ControlPlane), "secret.created", struct{}{})
		r.Authorization = json.RawMessage(bad)
		if _, err := projectRecord(r, scope); err == nil {
			t.Fatalf("accepted malformed authorization %s", bad)
		}
	}
	r, scope := projectionRecord(t, audit.Internal(audit.ControlPlane), "member.ownership_changed", struct{}{})
	if _, err := projectRecord(r, scope); err == nil {
		t.Fatal("missing required booleans accepted")
	}
	r.Details = json.RawMessage(`{"was_owner":null,"is_owner":true}`)
	if _, err := projectRecord(r, scope); err == nil {
		t.Fatal("null ownership observation accepted")
	}
	for _, tc := range []struct{ action, details string }{
		{"session.content_read", `{"sequence":null}`},
		{"role.created", `{"before":null,"after":{"revision":null}}`},
		{"role.created", `{"before":null,"after":{"permissions":[null]}}`},
	} {
		record, read := projectionRecord(t, audit.Internal(audit.ControlPlane), tc.action, struct{}{})
		record.Details = json.RawMessage(tc.details)
		if _, err := projectRecord(record, read); err == nil {
			t.Fatal("null optional observation accepted", tc.action)
		}
	}
	validRecord, validScope := projectionRecord(t, audit.Internal(audit.ControlPlane), "secret.created", struct{}{})
	validRecord.Causality = json.RawMessage(`{"initiator_kind":null}`)
	if _, err := projectRecord(validRecord, validScope); err == nil {
		t.Fatal("null causality accepted")
	}
	r.Action = "unknown.action"
	if _, err := projectRecord(r, scope); err == nil {
		t.Fatal("unclassified action accepted")
	}
}

func TestAuditProjectionScopesAndDetailSemantics(t *testing.T) {
	user, credential, foreign := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	actor := audit.FromPrincipal(auth.Principal{Kind: auth.PrincipalKindSession, UserID: user, SessionID: credential})
	actor.Authorization = audit.Authorization{Decision: "denied", Checks: []audit.Check{audit.PermissionCheck{Type: "permission", SubjectUserID: user, Permission: auth.PermissionSessionsRead, Scope: auth.Scope{OrgID: foreign}, Result: "failed", Reason: "organization_mismatch"}}}
	r, scope := projectionRecord(t, actor, "session.content_read", audit.FailureDetails{Reason: "permission_denied"})
	r.Outcome = "denied"
	public, err := projectRecord(r, scope)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(public)
	if bytes.Contains(data, []byte(foreign.String())) || !bytes.Contains(data, []byte("organization_mismatch")) {
		t.Fatalf("foreign scope/result: %s", data)
	}
	r, scope = projectionRecord(t, audit.Internal(audit.ControlPlane), "session.delivery_check_requested", audit.LifecycleDetails{Generation: 7, RequestID: uuid.NewV7()})
	public, err = projectRecord(r, scope)
	if err != nil {
		t.Fatal(err)
	}
	if public.Details.(api.AuditLifecycleDetails).PublicationRevision != 7 {
		t.Fatal("delivery publication observation lost")
	}
	r, scope = projectionRecord(t, audit.Internal(audit.ControlPlane), "secret.exposed", audit.SecretExposureDetails{ExposureID: uuid.NewV7(), VersionID: uuid.NewV7(), RevocationGeneration: 9})
	public, err = projectRecord(r, scope)
	if err != nil {
		t.Fatal(err)
	}
	if public.Details.(api.AuditSecretExposureDetails).RevocationGeneration != 9 {
		t.Fatal("tenant secret authority lost")
	}
	r, scope = projectionRecord(t, audit.Internal(audit.ControlPlane), "audit.searched", audit.SearchDetails{From: time.Now().Add(-time.Hour), To: time.Now(), ResultCount: 9, ActorID: foreign})
	public, err = projectRecord(r, scope)
	if err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(public)
	if bytes.Contains(data, []byte(foreign.String())) || bytes.Contains(data, []byte("result_count")) {
		t.Fatalf("unclassified historical selection escaped: %s", data)
	}
}

// Every captured customer action/outcome must have a closed readable projection.
// The samples cover the actual payload families, including nullable public fields.
func TestAuditCatalogProjection(t *testing.T) {
	now, id := time.Now(), uuid.NewV7()
	samples := []any{
		struct{}{}, audit.FailureDetails{Reason: "permission_denied"},
		audit.AuthenticationDetails{Method: "login"}, audit.SessionRevokedDetails{Reason: "logout"}, audit.MessageRejectedDetails{Reason: "turn_closed"},
		audit.SearchDetails{ReadModel: "organization", From: now.Add(-time.Hour), To: now},
		audit.SecretExposureDetails{ExposureID: id, VersionID: id}, audit.MCPDetails{Tool: "list_agents"},
		audit.ComputerRetentionDetails{}, audit.ComputerControlDetails{RequestID: id},
		audit.ComputerControlDetails{RequestID: id, Reason: "operation_failed"},
		audit.SessionControlDetails{RequestID: id}, audit.TemplateRefreshDetails{ImageBuildID: id},
		audit.SessionHoldDetails{HoldID: id, Reason: "storage_pressure"}, audit.CheckpointDetails{CheckpointID: id}, audit.LifecycleDetails{},
		audit.AgentCleanupDetails{CauseAgentID: id.String(), Reason: "agent_deleted"}, audit.AgentChangeDetails{Revision: 1},
		audit.OwnershipChangedDetails{}, audit.AccessChangeDetails{After: &audit.AccessSnapshot{Name: "Audit test"}},
		audit.MembershipDetails{MembershipID: id.String()}, audit.InvitationStateDetails{}, audit.InvitationCreatedDetails{ExpiresAt: &now},
		audit.APIKeyChangedDetails{Before: audit.APIKeyPolicy{Name: "Before"}, After: audit.APIKeyPolicy{Name: "After"}},
		audit.APIKeyIssuedDetails{Name: "Key"}, audit.PermissionsDetails{Permissions: []string{"chat:write"}}, audit.RetentionChangedDetails{},
	}
	for _, definition := range audit.Catalog() {
		if definition.Audience != audit.OrganizationAudience {
			continue
		}
		for _, outcome := range definition.Outcomes {
			t.Run(definition.Action+"/"+outcome, func(t *testing.T) {
				var payload []byte
				for _, sample := range samples {
					candidate, err := json.Marshal(sample)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = audit.DecodeDetails(definition.Action, outcome, candidate); err == nil {
						payload = candidate
						break
					}
				}
				if payload == nil {
					t.Fatal("no valid retained payload fixture")
				}
				r, scope := projectionRecord(t, audit.Internal(audit.ControlPlane), definition.Action, struct{}{})
				r.Outcome, r.Details = outcome, payload
				public, err := projectRecord(r, scope)
				if err != nil {
					t.Fatal(err)
				}
				if data, err := json.Marshal(public); err != nil || !json.Valid(data) {
					t.Fatal("invalid public encoding", err)
				}
				var unknown map[string]json.RawMessage
				if err := json.Unmarshal(payload, &unknown); err != nil {
					t.Fatal(err)
				}
				unknown["private_unknown"] = json.RawMessage(`"private-marker"`)
				r.Details, _ = json.Marshal(unknown)
				if _, err := projectRecord(r, scope); err == nil {
					t.Fatal("unknown retained field escaped closed projection")
				}
			})
		}
	}
}

func TestAuditProjectionPreservesOperationContext(t *testing.T) {
	key := "auditor"
	for _, tc := range []struct {
		action, outcome     string
		details             audit.Details
		expected, forbidden string
	}{
		{"role.created", "succeeded", audit.AccessChangeDetails{After: &audit.AccessSnapshot{Name: "Auditor", BuiltinKey: &key}}, `"builtin_key":"auditor"`, ""},
		{"session.hold_released", "succeeded", audit.SessionHoldDetails{HoldID: uuid.NewV7(), Reason: "storage_pressure", Generation: 4}, `"reason":"storage_pressure"`, `"generation"`},
		{"identity.session_revoked", "succeeded", audit.SessionRevokedDetails{Reason: "logout"}, `"reason":"logout"`, `"method"`},
		{"message.rejected", "succeeded", audit.MessageRejectedDetails{Reason: "turn_terminated"}, `"reason":"turn_terminated"`, ""},
		{"mcp.refused", "denied", audit.FailureDetails{Reason: "stale_authority", Tool: "list_agents"}, `"tool":"list_agents"`, ""},
	} {
		t.Run(tc.action, func(t *testing.T) {
			r, scope := projectionRecord(t, audit.Internal(audit.ControlPlane), tc.action, tc.details)
			r.Outcome = tc.outcome
			projected, err := projectRecord(r, scope)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(projected.Details)
			if err != nil || !bytes.Contains(data, []byte(tc.expected)) || (tc.forbidden != "" && bytes.Contains(data, []byte(tc.forbidden))) {
				t.Fatalf("projection lost context or disclosed internal fields: %s %v", data, err)
			}
		})
	}
}
