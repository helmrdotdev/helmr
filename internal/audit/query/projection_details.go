package auditquery

import (
	"errors"
	"slices"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
)

func projectDetails(event *api.AuditEvent, scope readScope, details audit.Details) (api.AuditDetails, error) {
	switch d := details.(type) {
	case nil:
		return api.AuditEmptyDetails{}, nil
	case audit.OwnershipChangedDetails:
		return api.AuditOwnershipChangedDetails(d), nil
	case audit.MembershipDetails:
		return api.AuditMembershipDetails(d), nil
	case audit.InvitationCreatedDetails:
		return api.AuditInvitationCreatedDetails(d), nil
	case audit.InvitationStateDetails:
		return api.AuditInvitationStateDetails(d), nil
	case audit.APIKeyIssuedDetails:
		return api.AuditAPIKeyIssuedDetails(d), nil
	case audit.APIKeyChangedDetails:
		return api.AuditAPIKeyChangedDetails{Before: api.AuditAPIKeyPolicy(d.Before), After: api.AuditAPIKeyPolicy(d.After)}, nil
	case audit.PermissionsDetails:
		return api.AuditPermissionsDetails(d), nil
	case audit.RetentionChangedDetails:
		return api.AuditRetentionChangedDetails(d), nil
	case audit.AccessChangeDetails:
		return api.AuditAccessChangeDetails{CauseAgentID: d.CauseAgentID, Reason: d.Reason, Before: projectAccessSnapshot(event, scope, d.Before), After: projectAccessSnapshot(event, scope, d.After)}, nil
	case audit.AgentCleanupDetails:
		return api.AuditAgentCleanupDetails(d), nil
	case audit.AgentChangeDetails:
		return api.AuditAgentChangeDetails(d), nil
	case audit.SessionRevokedDetails:
		return api.AuditSessionRevokedDetails(d), nil
	case audit.MessageRejectedDetails:
		return api.AuditMessageRejectedDetails(d), nil
	case audit.AuthenticationDetails:
		public := api.AuditAuthenticationDetails(d)
		if event.Authorization.InstallationAuthority && public.AuthorizingCredentialID != uuid.Nil() {
			public.AuthorizingCredentialID = uuid.Nil()
			restrict(event, api.AuditRestrictedDetails)
		}
		return public, nil
	case audit.FailureDetails:
		public := api.AuditFailureDetails{Reason: d.Reason, Tool: d.Tool}
		if d.Operation != "" {
			// HTTP refusal writers store a matched route template, never the URL/body.
			// Other internal operations are restricted until a public contract names them.
			if strings.Contains(d.Operation, " /api/") || strings.Contains(d.Operation, " /v1/") {
				public.Operation = d.Operation
			} else {
				restrict(event, api.AuditRestrictedDetails)
			}
		}
		return public, nil
	case audit.MCPDetails:
		return api.AuditMCPDetails(d), nil
	case audit.SecretExposureDetails:
		return api.AuditSecretExposureDetails(d), nil
	case audit.ComputerRetentionDetails:
		return api.AuditComputerRetentionDetails(d), nil
	case audit.ComputerControlDetails:
		return api.AuditComputerControlDetails(d), nil
	case audit.SessionControlDetails:
		return api.AuditSessionControlDetails(d), nil
	case audit.TemplateRefreshDetails:
		return api.AuditTemplateRefreshDetails(d), nil
	case audit.SessionHoldDetails:
		if d.Generation != 0 {
			restrict(event, api.AuditRestrictedDetails)
		}
		return api.AuditSessionHoldDetails{HoldID: d.HoldID, Reason: d.Reason}, nil
	case audit.CheckpointDetails:
		if d.Generation != 0 {
			restrict(event, api.AuditRestrictedDetails)
		}
		return api.AuditCheckpointDetails{CheckpointID: d.CheckpointID}, nil
	case audit.LifecycleDetails:
		public := api.AuditLifecycleDetails{Sequence: d.Sequence, RequestID: d.RequestID}
		if event.Action == "session.delivery_check_requested" || event.Action == "session.delivery_abandoned" {
			public.PublicationRevision = d.Generation
		} else if d.Generation != 0 {
			restrict(event, api.AuditRestrictedDetails)
		}
		return public, nil
	case audit.SearchDetails:
		return projectSearchDetails(event, scope, d), nil
	default:
		return nil, errors.New("audit detail type has no organization projection")
	}
}

func projectAccessSnapshot(event *api.AuditEvent, scope readScope, d *audit.AccessSnapshot) *api.AuditAccessSnapshot {
	if d == nil {
		return nil
	}
	if !scope.permitsStrings(scope.org, d.EnvironmentID) || (!scope.organizationWide && d.ProjectID != "" && d.ProjectID != pointerValue(event.ProjectID).String()) {
		restrict(event, api.AuditRestrictedDetails)
		return nil
	}
	public := api.AuditAccessSnapshot(*d)
	return &public
}

func projectSearchDetails(event *api.AuditEvent, scope readScope, d audit.SearchDetails) api.AuditSearchDetails {
	public := api.AuditSearchDetails{From: d.From, To: d.To}
	for _, env := range d.EnvironmentIDs {
		if scope.permits(scope.org, env) {
			public.EnvironmentIDs = append(public.EnvironmentIDs, env)
		} else {
			restrict(event, api.AuditRestrictedDetails)
		}
	}
	if scope.permits(scope.org, d.RequestedEnvironmentID) {
		public.RequestedEnvironmentID = d.RequestedEnvironmentID
	} else {
		restrict(event, api.AuditRestrictedDetails)
	}
	// Admin selection may contain platform events/operands. Its recorded count is
	// not the size of the tenant-visible selection, even for the same organization.
	if d.ReadModel != "organization" || event.Authorization.InstallationAuthority || slices.Contains(event.Restricted, api.AuditRestrictedAuthorization) {
		restrict(event, api.AuditRestrictedDetails)
		return public
	}
	// Older operator/invalid filters cannot reintroduce a hidden identifier via
	// their receipt. Only the current public filter vocabulary is projected.
	if d.ActorKind == "worker_host" || d.ActorKind == "component" || (d.ActorID != uuid.Nil() && d.ActorKind != "" && d.ActorKind != "user" && d.ActorKind != "api_key" && d.ActorKind != "session") {
		restrict(event, api.AuditRestrictedDetails)
		return public
	}
	for _, name := range d.Actions {
		action, ok := audit.Lookup(name)
		if !ok || action.Audience != audit.OrganizationAudience {
			restrict(event, api.AuditRestrictedDetails)
			return public
		}
	}
	if d.TargetKind != "" && !publicTargetKind(d.TargetKind) {
		restrict(event, api.AuditRestrictedDetails)
		return public
	}
	if len(public.EnvironmentIDs) != len(d.EnvironmentIDs) || public.RequestedEnvironmentID != d.RequestedEnvironmentID {
		restrict(event, api.AuditRestrictedDetails)
		return public
	}
	public.ResultCount = &d.ResultCount
	public.Actions, public.ActorKind, public.ActorID = d.Actions, d.ActorKind, d.ActorID
	public.OperationID, public.EventID = d.OperationID, d.EventID
	public.TargetKind, public.TargetID, public.TargetUserID = d.TargetKind, d.TargetID, d.TargetUserID
	public.SlackTeamID, public.SlackUserID, public.Outcome = d.SlackTeamID, d.SlackUserID, d.Outcome
	return public
}
func publicTargetKind(kind string) bool {
	for _, d := range audit.Catalog() {
		if d.Audience == audit.OrganizationAudience && d.TargetKind == kind {
			return true
		}
	}
	return false
}
