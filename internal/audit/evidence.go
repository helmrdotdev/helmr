package audit

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auth"
	"slices"
	"uuid"
)

// Actor references the entity that acted, independently of its credential or authority.
// Exactly one reference is set for an established actor. Historical references have no live FKs.
type Actor struct {
	UserID       uuid.UUID
	APIKeyID     uuid.UUID
	SessionID    uuid.UUID
	WorkerHostID uuid.UUID
	Component    string
}

// Evidence transports observations made by the operation owner. It does not grant authority.
type Evidence struct {
	RuntimeGeneration int64
	ProcessEpoch      int64
	Actor             Actor
	Authentication    Authentication
	Authorization     Authorization
}

type Authentication struct {
	Origin            string    `json:"origin"`
	Result            string    `json:"result"`
	Provider          string    `json:"provider,omitempty"`
	CredentialKind    string    `json:"credential_kind,omitempty"`
	CredentialID      uuid.UUID `json:"credential_id,omitzero"`
	CredentialVersion int64     `json:"credential_version,omitempty"`
}

type Authorization struct {
	Decision string  `json:"decision"`
	Checks   []Check `json:"checks"`
}

// Check is closed evidence, not an executable policy expression.
type Check interface {
	auditCheck()
	result() string
	validate() error
}

type PermissionCheck struct {
	Type string `json:"type"`
	auth.PermissionEvaluation
}

func (PermissionCheck) auditCheck()      {}
func (c PermissionCheck) result() string { return c.Result }
func (c PermissionCheck) validate() error {
	if (c.SubjectUserID == uuid.Nil()) == (c.SubjectAPIKeyID == uuid.Nil()) {
		return errors.New("permission subject required")
	}
	if c.Type != "permission" || c.Permission == "" || !slices.Contains([]string{"passed", "failed"}, c.Result) {
		return errors.New("invalid permission check")
	}
	if c.Result == "passed" {
		d := c.Evidence
		if d == nil || !auth.IsPermission(c.Permission) || d.Permission != c.Permission || d.Scope != c.Scope || d.AssignmentID == "" || d.AssignmentRevision <= 0 || d.RoleID == "" || d.RoleRevision <= 0 || c.Reason != "" {
			return errors.New("incomplete permission evidence")
		}
	} else if c.Evidence != nil || !slices.Contains([]string{"unsupported_permission", "organization_mismatch", "key_permission_not_supported", "key_scope_mismatch", "no_matching_grant"}, c.Reason) {
		return errors.New("invalid permission refusal")
	}
	return nil
}

type CheckScope struct {
	Kind          string    `json:"kind"`
	OrgID         uuid.UUID `json:"org_id,omitzero"`
	ProjectID     uuid.UUID `json:"project_id,omitzero"`
	EnvironmentID uuid.UUID `json:"environment_id,omitzero"`
	SessionID     uuid.UUID `json:"session_id,omitzero"`
}

type UserAttributeCheck struct {
	Type              string     `json:"type"`
	UserID            uuid.UUID  `json:"user_id"`
	Attribute         string     `json:"attribute"`
	Expected          bool       `json:"expected"`
	Observed          bool       `json:"observed"`
	ObservationSource string     `json:"observation_source"`
	Scope             CheckScope `json:"scope"`
	Result            string     `json:"result"`
}

func (UserAttributeCheck) auditCheck()      {}
func (c UserAttributeCheck) result() string { return c.Result }
func (c UserAttributeCheck) validate() error {
	if c.Type != "user_attribute" || c.UserID == uuid.Nil() || c.Attribute != "admin" || !c.Expected || c.Scope != (CheckScope{Kind: "platform"}) || !slices.Contains([]string{"request_authentication", "transaction"}, c.ObservationSource) || c.Result != checkResult(c.Observed) {
		return errors.New("invalid user attribute check")
	}
	return nil
}

type MembershipAttributeCheck struct {
	Type      string     `json:"type"`
	MemberID  uuid.UUID  `json:"member_id"`
	Attribute string     `json:"attribute"`
	Expected  bool       `json:"expected"`
	Observed  bool       `json:"observed"`
	Scope     CheckScope `json:"scope"`
	Result    string     `json:"result"`
}

func (MembershipAttributeCheck) auditCheck()      {}
func (c MembershipAttributeCheck) result() string { return c.Result }
func (c MembershipAttributeCheck) validate() error {
	if c.Type != "membership_attribute" || c.MemberID == uuid.Nil() || c.Attribute != "is_owner" || !c.Expected || c.Scope.Kind != "organization" || c.Scope.OrgID == uuid.Nil() || c.Result != checkResult(c.Observed) {
		return errors.New("invalid membership attribute check")
	}
	return nil
}

// RuntimeBindingCheck records the matched runtime authority, not the human who started it.
type RuntimeBindingCheck struct {
	Reason             string    `json:"reason,omitempty"`
	ObservedGeneration int64     `json:"observed_generation,omitempty"`
	ProcessEpoch       int64     `json:"process_epoch,omitempty"`
	Type               string    `json:"type"`
	SessionID          uuid.UUID `json:"session_id"`
	Generation         int64     `json:"generation"`
	Result             string    `json:"result"`
}

func (RuntimeBindingCheck) auditCheck()      {}
func (c RuntimeBindingCheck) result() string { return c.Result }
func (c RuntimeBindingCheck) validate() error {
	if c.Type != "runtime_binding" || c.SessionID == uuid.Nil() || c.Generation <= 0 || c.ProcessEpoch < 0 || !slices.Contains([]string{"passed", "failed"}, c.Result) {
		return errors.New("invalid runtime binding check")
	}
	if c.Result == "passed" && (c.Reason != "" || c.ObservedGeneration != 0) || c.Result == "failed" && (c.Reason != "generation_changed" || c.ObservedGeneration <= 0 || c.ObservedGeneration == c.Generation) {
		return errors.New("invalid runtime binding observation")
	}
	return nil
}

// OriginCheck records a verified initiating-principal relationship used by an owner.
type OriginCheck struct {
	Type          string    `json:"type"`
	SessionID     uuid.UUID `json:"session_id"`
	InitiatorKind string    `json:"initiator_kind"`
	InitiatorID   uuid.UUID `json:"initiator_id"`
	Result        string    `json:"result"`
}

func (OriginCheck) auditCheck()      {}
func (c OriginCheck) result() string { return c.Result }
func (c OriginCheck) validate() error {
	if c.Type != "session_origin" || c.SessionID == uuid.Nil() || !slices.Contains([]string{"user", "api_key", "session"}, c.InitiatorKind) || c.InitiatorID == uuid.Nil() || c.Result != "passed" {
		return errors.New("invalid session origin check")
	}
	return nil
}

func checkResult(ok bool) string {
	if ok {
		return "passed"
	}
	return "failed"
}
func NotEvaluated() Authorization { return Authorization{Decision: "not_evaluated", Checks: []Check{}} }
func PermissionAuthorization(checks []auth.PermissionEvaluation, allowed bool) Authorization {
	a := Authorization{Decision: "denied", Checks: make([]Check, 0, len(checks))}
	if allowed {
		a.Decision = "allowed"
	}
	for _, c := range checks {
		a.Checks = append(a.Checks, PermissionCheck{Type: "permission", PermissionEvaluation: c})
	}
	return a
}
func PermissionEvidence(p auth.Principal, decisions []auth.PermissionDecision) []Check {
	checks := make([]Check, 0, len(decisions))
	for _, d := range decisions {
		checks = append(checks, PermissionCheck{Type: "permission", PermissionEvaluation: auth.PermissionEvaluation{SubjectUserID: p.UserID, SubjectAPIKeyID: p.APIKeyID, Permission: d.Permission, Scope: d.Scope, Result: "passed", Evidence: &d}})
	}
	return checks
}
func FromPrincipal(p auth.Principal) Evidence {
	e := Evidence{Authorization: NotEvaluated(), Authentication: Authentication{Origin: "request", Result: "verified"}}
	switch p.Kind {
	case auth.PrincipalKindAPIKey:
		e.Actor.APIKeyID = p.APIKeyID
		e.Authentication.CredentialKind = "api_key"
		e.Authentication.CredentialID = p.APIKeyID
		e.Authentication.CredentialVersion = p.CredentialVersion
	case auth.PrincipalKindSession:
		e.Actor.UserID = p.UserID
		e.Authentication.CredentialKind = "login_session"
		e.Authentication.CredentialID = p.SessionID
	}
	return e
}
func Authorized(p auth.Principal, permission auth.Permission, scope auth.Scope) (Evidence, error) {
	checks, ok := p.Evaluate(permission, scope)
	e := FromPrincipal(p)
	e.Authorization = PermissionAuthorization(checks, ok)
	if !ok {
		return e, &auth.PermissionRefusal{Principal: p, Checks: checks}
	}
	return e, nil
}
func VerifiedLogin(user uuid.UUID, provider string) Evidence {
	return Evidence{Actor: Actor{UserID: user}, Authentication: Authentication{Origin: "request", Result: "verified", CredentialKind: "provider_assertion", Provider: provider}, Authorization: NotEvaluated()}
}

type Component string

const (
	ControlPlane       Component = "control_plane"
	Dispatcher         Component = "dispatcher"
	ControlPlaneCLI    Component = "control_plane_cli"
	AuditMaintenance   Component = "audit_maintenance"
	Development        Component = "development"
	CapacityController Component = "capacity_controller"
)

func Internal(component Component) Evidence {
	return Evidence{Actor: Actor{Component: string(component)}, Authentication: Authentication{Origin: "internal", Result: "not_attempted"}, Authorization: NotEvaluated()}
}
func CapacityAuthority() Evidence {
	e := Internal(CapacityController)
	e.Authentication = Authentication{Origin: "request", Result: "verified", CredentialKind: "capacity_token"}
	return e
}
func PlatformOperator(p auth.Principal) (Evidence, error) {
	e := FromPrincipal(p)
	if p.Kind != auth.PrincipalKindSession {
		return e, errors.New("platform authority requires a login session")
	}
	e.Authorization = Authorization{Decision: "denied", Checks: []Check{UserAttributeCheck{Type: "user_attribute", UserID: p.UserID, Attribute: "admin", Expected: true, Observed: p.Admin, ObservationSource: "request_authentication", Scope: CheckScope{Kind: "platform"}, Result: checkResult(p.Admin)}}}
	if !p.Admin {
		return e, errors.New("platform authority is required")
	}
	e.Authorization.Decision = "allowed"
	return e, nil
}
func Ownership(p auth.Principal) Authorization {
	decision := "denied"
	if p.IsOwner {
		decision = "allowed"
	}
	return Authorization{Decision: decision, Checks: []Check{MembershipAttributeCheck{Type: "membership_attribute", MemberID: p.MemberID, Attribute: "is_owner", Expected: true, Observed: p.IsOwner, Scope: CheckScope{Kind: "organization", OrgID: p.OrgID}, Result: checkResult(p.IsOwner)}}}
}
func RuntimeActor(session uuid.UUID, generation, epoch int64, host uuid.UUID, hostVersion int64) Evidence {
	return Evidence{Actor: Actor{SessionID: session}, RuntimeGeneration: generation, ProcessEpoch: epoch,
		Authentication: Authentication{Origin: "request", Result: "verified", CredentialKind: "worker_host", CredentialID: host, CredentialVersion: hostVersion},
		Authorization:  Authorization{Decision: "allowed", Checks: []Check{RuntimeBindingCheck{Type: "runtime_binding", SessionID: session, Generation: generation, ProcessEpoch: epoch, Result: "passed"}}}}
}

func Worker(host uuid.UUID, version int64) Evidence {
	return Evidence{Actor: Actor{WorkerHostID: host}, Authentication: Authentication{Origin: "request", Result: "verified", CredentialKind: "worker_host", CredentialID: host, CredentialVersion: version}, Authorization: NotEvaluated()}
}
func Unauthenticated(result, kind string) Evidence {
	return Evidence{Authentication: Authentication{Origin: "request", Result: result, CredentialKind: kind}, Authorization: NotEvaluated()}
}

// Validate checks captured evidence independently of current authority.
func (e Evidence) Validate() error {
	a, n := e.Actor, 0
	for _, id := range []uuid.UUID{a.UserID, a.APIKeyID, a.SessionID, a.WorkerHostID} {
		if id != uuid.Nil() {
			n++
		}
	}
	if a.Component != "" {
		n++
		if !slices.Contains([]string{string(ControlPlane), string(Dispatcher), string(ControlPlaneCLI), string(AuditMaintenance), string(Development), string(CapacityController)}, a.Component) {
			return errors.New("unknown audit component")
		}
	}
	if n > 1 {
		return errors.New("multiple audit actors")
	}
	c := e.Authentication
	if !slices.Contains([]string{"verified", "failed", "absent", "not_attempted"}, c.Result) {
		return errors.New("invalid authentication result")
	}
	if n == 0 && c.Result != "failed" && c.Result != "absent" || n != 0 && (c.Result == "failed" || c.Result == "absent") {
		return errors.New("actor authentication mismatch")
	}
	if !slices.Contains([]string{"internal", "request"}, c.Origin) {
		return errors.New("invalid authentication origin")
	}
	if (c.Origin == "internal") != (c.Result == "not_attempted") {
		return errors.New("authentication origin/result mismatch")
	}
	if c.Origin == "internal" && (a.Component == "" || c.CredentialKind != "") {
		return errors.New("invalid internal provenance")
	}
	if c.Result != "verified" && (c.CredentialID != uuid.Nil() || c.CredentialVersion != 0 || e.RuntimeGeneration != 0 || e.ProcessEpoch != 0) {
		return errors.New("unverified credential reference")
	}
	if c.CredentialKind != "" && !slices.Contains([]string{"login_session", "api_key", "worker_host", "capacity_token", "provider_assertion", "magic_link", "device_code"}, c.CredentialKind) {
		return errors.New("invalid credential kind")
	}
	if c.Result == "verified" && c.CredentialKind == "" {
		return errors.New("verified credential kind required")
	}
	if c.Provider != "" && c.CredentialKind != "provider_assertion" && c.CredentialKind != "magic_link" {
		return errors.New("provider credential mismatch")
	}
	if len(c.Provider) > 64 || c.CredentialVersion < 0 || e.RuntimeGeneration < 0 || e.ProcessEpoch < 0 {
		return errors.New("invalid authentication evidence")
	}
	if a.APIKeyID != uuid.Nil() && (c.CredentialKind != "api_key" || c.CredentialID != a.APIKeyID || c.CredentialVersion <= 0) {
		return errors.New("invalid key identity")
	}
	if a.WorkerHostID != uuid.Nil() && (c.CredentialKind != "worker_host" || c.CredentialID != a.WorkerHostID || c.CredentialVersion <= 0) {
		return errors.New("invalid worker identity")
	}
	if a.SessionID != uuid.Nil() && (c.CredentialKind != "worker_host" || c.CredentialID == uuid.Nil() || c.CredentialVersion <= 0 || e.RuntimeGeneration <= 0) {
		return errors.New("invalid runtime identity")
	}
	if a.UserID != uuid.Nil() {
		if !slices.Contains([]string{"login_session", "provider_assertion", "magic_link", "device_code"}, c.CredentialKind) {
			return errors.New("invalid user credential")
		}
		if (c.CredentialKind == "login_session" || c.CredentialKind == "device_code" || c.CredentialKind == "magic_link") && c.CredentialID == uuid.Nil() {
			return errors.New("user credential reference required")
		}
		if c.CredentialKind == "provider_assertion" && c.Provider == "" {
			return errors.New("identity provider required")
		}
	}
	if a.Component != "" && c.Result == "verified" && (a.Component != string(CapacityController) || c.CredentialKind != "capacity_token") {
		return errors.New("invalid authenticated component")
	}
	if (e.RuntimeGeneration != 0 || e.ProcessEpoch != 0) && a.SessionID == uuid.Nil() {
		return errors.New("runtime evidence requires Session actor")
	}

	z := e.Authorization
	if !slices.Contains([]string{"allowed", "denied", "not_evaluated"}, z.Decision) {
		return errors.New("invalid authorization decision")
	}
	if z.Decision == "not_evaluated" && len(z.Checks) != 0 || z.Decision != "not_evaluated" && len(z.Checks) == 0 {
		return errors.New("authorization checks mismatch")
	}
	if n == 0 && z.Decision != "not_evaluated" {
		return errors.New("unidentified authorization")
	}
	passed, failed := false, false
	for _, check := range z.Checks {
		if check == nil {
			return errors.New("nil authorization check")
		}
		if err := check.validate(); err != nil {
			return fmt.Errorf("authorization evidence: %w", err)
		}
		passed = passed || check.result() == "passed"
		failed = failed || check.result() == "failed"
	}
	if z.Decision == "allowed" && !passed || z.Decision == "denied" && !failed {
		return errors.New("authorization decision lacks supporting check")
	}
	return nil
}

// Refusal preserves a non-Permission check at the boundary that evaluated it.
type Refusal struct {
	Authorization Authorization
	Cause         error
}

func (e *Refusal) Error() string { return e.Cause.Error() }
func (e *Refusal) Unwrap() error { return e.Cause }
func VerifiedDevice(user, device uuid.UUID) Evidence {
	return Evidence{Actor: Actor{UserID: user}, Authentication: Authentication{Origin: "request", Result: "verified", CredentialKind: "device_code", CredentialID: device}, Authorization: NotEvaluated()}
}
func VerifiedMagicLink(user, link uuid.UUID, provider string) Evidence {
	return Evidence{Actor: Actor{UserID: user}, Authentication: Authentication{Origin: "request", Result: "verified", CredentialKind: "magic_link", CredentialID: link, Provider: provider}, Authorization: NotEvaluated()}
}

type SlackThreadCheck struct {
	Type      string    `json:"type"`
	SessionID uuid.UUID `json:"session_id"`
	ThreadID  uuid.UUID `json:"thread_id"`
	UserID    uuid.UUID `json:"user_id"`
	Result    string    `json:"result"`
}

func (SlackThreadCheck) auditCheck()      {}
func (c SlackThreadCheck) result() string { return c.Result }
func (c SlackThreadCheck) validate() error {
	if c.Type != "slack_thread" || c.SessionID == uuid.Nil() || c.ThreadID == uuid.Nil() || c.UserID == uuid.Nil() || c.Result != "passed" {
		return errors.New("invalid Slack thread check")
	}
	return nil
}

// PrincipalCurrencyCheck records a failed transaction-time principal refresh.
// It does not imply that admission authentication failed.
type PrincipalCurrencyCheck struct {
	Type              string     `json:"type"`
	SubjectUserID     uuid.UUID  `json:"subject_user_id,omitzero"`
	SubjectAPIKeyID   uuid.UUID  `json:"subject_api_key_id,omitzero"`
	Scope             CheckScope `json:"scope"`
	ObservationSource string     `json:"observation_source"`
	Result            string     `json:"result"`
	Reason            string     `json:"reason"`
	PresentedVersion  int64      `json:"presented_version,omitempty"`
	ObservedVersion   int64      `json:"observed_version,omitempty"`
}

func (PrincipalCurrencyCheck) auditCheck()      {}
func (c PrincipalCurrencyCheck) result() string { return c.Result }
func (c PrincipalCurrencyCheck) validate() error {
	if c.Type != "principal_currency" || c.Result != "failed" || c.ObservationSource != "transaction" || (c.SubjectUserID == uuid.Nil()) == (c.SubjectAPIKeyID == uuid.Nil()) || c.Scope.Kind != "organization" || c.Scope.OrgID == uuid.Nil() {
		return errors.New("invalid principal currency check")
	}
	switch c.Reason {
	case "active_membership_not_found":
		if c.SubjectUserID == uuid.Nil() {
			return errors.New("membership check requires user")
		}
	case "current_key_not_found":
		if c.SubjectAPIKeyID == uuid.Nil() {
			return errors.New("key check requires key")
		}
	case "credential_version_changed":
		if c.SubjectAPIKeyID == uuid.Nil() || c.PresentedVersion <= 0 || c.ObservedVersion <= 0 || c.PresentedVersion == c.ObservedVersion {
			return errors.New("invalid credential version comparison")
		}
		return nil
	default:
		return errors.New("invalid principal currency reason")
	}
	if c.PresentedVersion != 0 || c.ObservedVersion != 0 {
		return errors.New("unexpected credential versions")
	}
	return nil
}

// HostAssignmentCheck records the exact assignment checked by the operation owner.
type HostAssignmentCheck struct {
	Type       string    `json:"type"`
	HostID     uuid.UUID `json:"host_id"`
	TargetKind string    `json:"target_kind"`
	TargetID   uuid.UUID `json:"target_id"`
	Epoch      int64     `json:"epoch,omitempty"`
	Result     string    `json:"result"`
}

func (HostAssignmentCheck) auditCheck()      {}
func (c HostAssignmentCheck) result() string { return c.Result }
func (c HostAssignmentCheck) validate() error {
	if c.Type != "host_assignment" || c.HostID == uuid.Nil() || c.TargetID == uuid.Nil() || !slices.Contains([]string{"computer", "session", "image_build"}, c.TargetKind) || c.Epoch < 0 || c.Result != "passed" {
		return errors.New("invalid host assignment check")
	}
	return nil
}
func AssignedWorker(host uuid.UUID, version int64, kind string, target uuid.UUID, epoch int64) Evidence {
	e := Worker(host, version)
	e.Authorization = Authorization{Decision: "allowed", Checks: []Check{HostAssignmentCheck{Type: "host_assignment", HostID: host, TargetKind: kind, TargetID: target, Epoch: epoch, Result: "passed"}}}
	return e
}
