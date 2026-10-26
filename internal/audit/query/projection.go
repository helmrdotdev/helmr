package auditquery

import (
	"errors"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
)

// readScope is current reader authority, not reconstructed historical authority.
// Organization-wide checks on an allowed event remain evidence; explicit foreign
// organization/environment references do not become visible through that event.
type readScope struct {
	org              uuid.UUID
	organizationWide bool
	environments     []uuid.UUID
}

func (s readScope) permits(org, environment uuid.UUID) bool {
	return (org == uuid.Nil() || org == s.org) && (environment == uuid.Nil() || s.organizationWide || slices.Contains(s.environments, environment))
}
func (s readScope) permitsStrings(org uuid.UUID, environment string) bool {
	if environment == "" {
		return s.permits(org, uuid.Nil())
	}
	id, err := uuid.Parse(environment)
	return err == nil && s.permits(org, id)
}
func pointerValue[T any](v *T) T {
	if v != nil {
		return *v
	}
	var zero T
	return zero
}
func restrict(event *api.AuditEvent, section api.AuditRestrictedSection) {
	if !slices.Contains(event.Restricted, section) {
		event.Restricted = append(event.Restricted, section)
	}
}

func projectRecord(r Record, scope readScope) (api.AuditEvent, error) {
	d, ok := audit.Lookup(r.Action)
	if !ok || d.Audience != audit.OrganizationAudience || r.SchemaVersion != 1 || r.OrgID == nil || *r.OrgID != scope.org || !scope.permits(*r.OrgID, pointerValue(r.EnvironmentID)) || d.TargetKind != r.TargetKind || !slices.Contains(d.Outcomes, r.Outcome) {
		return api.AuditEvent{}, errors.New("audit event has no organization projection")
	}
	z, err := audit.DecodeAuthorization(r.Authorization)
	if err != nil {
		return api.AuditEvent{}, err
	}
	captured := audit.Evidence{
		Actor:             audit.Actor{UserID: pointerValue(r.ActorUserID), APIKeyID: pointerValue(r.ActorAPIKeyID), SessionID: pointerValue(r.ActorSessionID), WorkerHostID: pointerValue(r.ActorWorkerHostID), Component: pointerValue(r.ActorComponent)},
		Authentication:    audit.Authentication{Result: r.AuthenticationResult, Origin: r.AuthenticationOrigin, Provider: pointerValue(r.IdentityProvider), CredentialKind: pointerValue(r.CredentialKind), CredentialID: pointerValue(r.CredentialID), CredentialVersion: pointerValue(r.CredentialVersion)},
		RuntimeGeneration: pointerValue(r.RuntimeGeneration), ProcessEpoch: pointerValue(r.ProcessEpoch), Authorization: z,
	}
	if err := captured.Validate(); err != nil {
		return api.AuditEvent{}, err
	}
	event := api.AuditEvent{AuditEventMetadata: api.AuditEventMetadata{
		ID: r.ID, OrgID: r.OrgID, ProjectID: r.ProjectID, EnvironmentID: r.EnvironmentID, SchemaVersion: r.SchemaVersion,
		OccurredAt: r.OccurredAt, RecordedAt: r.RecordedAt, RetentionSeconds: r.RetentionSeconds, ExpiresAt: r.ExpiresAt,
		ActorUserID: r.ActorUserID, ActorAPIKeyID: r.ActorAPIKeyID, ActorSessionID: r.ActorSessionID,
		Authentication: api.AuditAuthentication{Result: r.AuthenticationResult, Origin: r.AuthenticationOrigin, Provider: r.IdentityProvider, CredentialKind: r.CredentialKind, CredentialID: r.CredentialID, CredentialVersion: r.CredentialVersion},
		Action:         r.Action, Outcome: r.Outcome, TargetKind: r.TargetKind, TargetID: r.TargetID, TargetUserID: r.TargetUserID, SlackTeamID: r.SlackTeamID, SlackUserID: r.SlackUserID, TargetResolution: r.TargetResolution,
		OperationID: r.OperationID, SessionID: r.SessionID, TurnID: r.TurnID, ComputerID: r.ComputerID, DeploymentID: r.DeploymentID, Channel: r.Channel,
		Restricted: []api.AuditRestrictedSection{},
	}}
	switch {
	case r.ActorUserID != nil:
		event.ActorKind = api.AuditActorUser
	case r.ActorAPIKeyID != nil:
		event.ActorKind = api.AuditActorAPIKey
	case r.ActorSessionID != nil:
		event.ActorKind = api.AuditActorSession
	case r.ActorWorkerHostID != nil || r.ActorComponent != nil:
		event.ActorKind = api.AuditActorPlatformService
		restrict(&event, api.AuditRestrictedActor)
	default:
		event.ActorKind = api.AuditActorUnidentified
	}
	if r.ActorIdentity != nil {
		if r.ActorUserID == nil || r.ActorIdentity.Source != "account" {
			return api.AuditEvent{}, errors.New("invalid audit human identity")
		}
		event.ActorIdentity = &api.AuditIdentity{Source: r.ActorIdentity.Source, DisplayName: r.ActorIdentity.DisplayName, Email: r.ActorIdentity.Email}
	}
	if err := projectAuthorization(&event, z, scope); err != nil {
		return api.AuditEvent{}, err
	}
	switch pointerValue(r.CredentialKind) {
	case "worker_host", "capacity_token":
		event.Authentication.CredentialKind = nil
		event.Authentication.CredentialID = nil
		event.Authentication.CredentialVersion = nil
		restrict(&event, api.AuditRestrictedAuthentication)
	}
	if event.Authorization.InstallationAuthority && (event.Authentication.CredentialID != nil || event.Authentication.CredentialVersion != nil) {
		event.Authentication.CredentialID = nil
		event.Authentication.CredentialVersion = nil
		restrict(&event, api.AuditRestrictedAuthentication)
	}
	if r.RuntimeGeneration != nil || r.ProcessEpoch != nil {
		restrict(&event, api.AuditRestrictedRuntime)
	}
	if r.SourceIP != nil {
		restrict(&event, api.AuditRestrictedTransport)
	}
	if r.CauseEventID != nil {
		restrict(&event, api.AuditRestrictedCorrelation)
	}
	if err := projectCausality(&event, r); err != nil {
		return api.AuditEvent{}, err
	}
	details, err := audit.DecodeDetails(r.Action, r.Outcome, r.Details)
	if err != nil {
		return api.AuditEvent{}, err
	}
	event.Details, err = projectDetails(&event, scope, details)
	if err != nil {
		return api.AuditEvent{}, err
	}
	return event, nil
}

func projectAuthorization(event *api.AuditEvent, z audit.Authorization, scope readScope) error {
	event.Authorization = api.AuditAuthorization{Decision: z.Decision, Checks: []api.AuditCheck{}}
	passedAdmin, failedAdmin := false, false
	for _, check := range z.Checks {
		switch c := check.(type) {
		case *audit.UserAttributeCheck:
			if event.ActorUserID != nil && c.UserID == *event.ActorUserID {
				passedAdmin = passedAdmin || c.Result == "passed"
				failedAdmin = failedAdmin || c.Result == "failed"
			}
			restrict(event, api.AuditRestrictedAuthorization)
		case *audit.RuntimeBindingCheck, *audit.HostAssignmentCheck:
			restrict(event, api.AuditRestrictedAuthorization)
		case *audit.PermissionCheck:
			public := api.AuditPermissionCheck{Type: c.Type, SubjectUserID: c.SubjectUserID, SubjectAPIKeyID: c.SubjectAPIKeyID, Permission: string(c.Permission), Result: c.Result, Reason: c.Reason}
			if scope.permitsStrings(c.Scope.OrgID, c.Scope.EnvironmentID) && (scope.organizationWide || c.Scope.ProjectID == "" || c.Scope.ProjectID == pointerValue(event.ProjectID).String()) {
				value := api.AuditPermissionScope(c.Scope)
				public.Scope = &value
				if w := c.Evidence; w != nil && scope.permitsStrings(w.Scope.OrgID, w.Scope.EnvironmentID) && (scope.organizationWide || w.Scope.ProjectID == "" || w.Scope.ProjectID == pointerValue(event.ProjectID).String()) {
					public.Evidence = &api.AuditPermissionWitness{Permission: string(w.Permission), Scope: api.AuditPermissionScope(w.Scope), AssignmentID: w.AssignmentID, AssignmentRevision: w.AssignmentRevision, RoleID: w.RoleID, RoleRevision: w.RoleRevision, MemberID: w.MemberID, GroupID: w.GroupID, Subject: w.Subject}
				} else if w != nil {
					restrict(event, api.AuditRestrictedAuthorization)
				}
			} else {
				restrict(event, api.AuditRestrictedAuthorization)
			}
			event.Authorization.Checks = append(event.Authorization.Checks, public)
		case *audit.MembershipAttributeCheck:
			if !scope.permits(c.Scope.OrgID, c.Scope.EnvironmentID) {
				restrict(event, api.AuditRestrictedAuthorization)
				continue
			}
			event.Authorization.Checks = append(event.Authorization.Checks, api.AuditMembershipAttributeCheck{Type: c.Type, MemberID: c.MemberID, Attribute: c.Attribute, Expected: c.Expected, Observed: c.Observed, Scope: api.AuditCheckScope(c.Scope), Result: c.Result})
		case *audit.PrincipalCurrencyCheck:
			if !scope.permits(c.Scope.OrgID, c.Scope.EnvironmentID) {
				restrict(event, api.AuditRestrictedAuthorization)
				continue
			}
			event.Authorization.Checks = append(event.Authorization.Checks, api.AuditPrincipalCurrencyCheck{Type: c.Type, SubjectUserID: c.SubjectUserID, SubjectAPIKeyID: c.SubjectAPIKeyID, Scope: api.AuditCheckScope(c.Scope), ObservationSource: c.ObservationSource, Result: c.Result, Reason: c.Reason, PresentedVersion: c.PresentedVersion, ObservedVersion: c.ObservedVersion})
		case *audit.OriginCheck:
			event.Authorization.Checks = append(event.Authorization.Checks, api.AuditOriginCheck(*c))
		case *audit.SlackThreadCheck:
			event.Authorization.Checks = append(event.Authorization.Checks, api.AuditSlackThreadCheck(*c))
		default:
			return errors.New("audit check has no organization projection")
		}
	}
	event.Authorization.InstallationAuthority = z.Decision == "allowed" && passedAdmin && !failedAdmin
	return nil
}

func projectCausality(event *api.AuditEvent, r Record) error {
	cause, err := audit.DecodeCausality(r.Causality)
	if err != nil {
		return err
	}
	if cause == (audit.Causality{}) {
		return nil
	}
	// Existing initiation writers bind these references to the event's Environment.
	// No current producer writes a cause-event reference or a global linkage.
	if r.EnvironmentID == nil || cause.InitiatorID == uuid.Nil() || !slices.Contains([]string{"user", "api_key", "session", "schedule"}, cause.InitiatorKind) {
		restrict(event, api.AuditRestrictedCausality)
		return nil
	}
	event.Causality = api.AuditCausality(cause)
	if cause.InitiatorKind == "schedule" && cause.SourceID != uuid.Nil() {
		event.Causality.SourceID = uuid.Nil()
		restrict(event, api.AuditRestrictedCausality)
	}
	return nil
}
