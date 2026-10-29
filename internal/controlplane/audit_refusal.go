package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
)

func (s *Server) requestAuditRoute(r *http.Request) (auditRoute, string, bool) {
	if s.auditRouter == nil {
		return auditRoute{}, "", false
	}
	matched := chi.NewRouteContext()
	if !s.auditRouter.Match(matched, r.Method, r.URL.Path) {
		return auditRoute{}, "", false
	}
	route, ok := auditRoutes[r.Method+" "+matched.RoutePattern()]
	if ok && route.action == "session.delivery_check_requested" && matched.URLParam("recovery") == "abandon" {
		route.action = "session.delivery_abandoned"
	}
	return route, r.Method + " " + matched.RoutePattern(), ok
}

// refuseOperation is called at explicit authentication/permission/owner error
// boundaries before a response is written, not by observing HTTP status codes.
func (s *Server) refuseOperation(w http.ResponseWriter, r *http.Request, reason string, causes ...error) bool {
	route, operation, ok := s.requestAuditRoute(r)
	if !ok || route.action == "" {
		return true
	}
	if route.action == "runtime.operation_refused" && workerFromContext(r.Context()).HostID == uuid.Nil() {
		// Malformed or unauthenticated machine transport is admission telemetry.
		return true
	}
	definition, ok := audit.Lookup(route.action)
	if !ok {
		s.auditCaptureFailed(r, audit.ErrCapture)
		writeError(w, unavailable(audit.ErrCapture))
		return false
	}
	p := principalFromContext(r.Context())
	result := "absent"
	if presentedCredential(r) {
		result = "failed"
	}
	actor := audit.Unauthenticated(result, "")
	org := uuid.Nil()
	if p.UserID != uuid.Nil() || p.APIKeyID != uuid.Nil() || p.OperatorID != uuid.Nil() {
		actor = audit.FromPrincipal(p)
		org = p.OrgID
	}
	if definition.Scope == audit.PlatformScope {
		org = uuid.Nil()
	}
	if host := workerFromContext(r.Context()); host.HostID != uuid.Nil() {
		// The authenticated Host does not prove the tenant/runtime IDs in the
		// rejected body. Keep custody at platform scope.
		actor = audit.Worker(host.HostID, host.HostClaimVersion)
		org = uuid.Nil()
		if !s.admitSecurityKey(w, r, "worker_host:"+host.HostID.String(), true) {
			return false
		}
		if guard := s.auditAdmission; guard != nil {
			if !guard.worker.Allow() {
				s.securityAdmissionRejected(w)
				return false
			}
			select {
			case guard.workerActive <- struct{}{}:
				defer func() { <-guard.workerActive }()
			default:
				s.securityAdmissionRejected(w)
				return false
			}
		}
	}
	for _, cause := range causes {
		if evaluated, ok := errors.AsType[*audit.Refusal](cause); ok {
			actor.Authorization = evaluated.Authorization
		}
		var unavailable *access.PrincipalUnavailable
		if errors.As(cause, &unavailable) && org != uuid.Nil() && (unavailable.UserID != uuid.Nil() && unavailable.UserID == p.UserID || unavailable.APIKeyID != uuid.Nil() && unavailable.APIKeyID == p.APIKeyID) {
			actor.Authorization = audit.Authorization{Decision: "denied", Checks: []audit.Check{audit.PrincipalCurrencyCheck{Type: "principal_currency", SubjectUserID: unavailable.UserID, SubjectAPIKeyID: unavailable.APIKeyID, Scope: audit.CheckScope{Kind: "organization", OrgID: org}, ObservationSource: "transaction", Result: "failed", Reason: unavailable.Reason, PresentedVersion: unavailable.PresentedVersion, ObservedVersion: unavailable.ObservedVersion}}}
		}
		if refusal, ok := errors.AsType[*auth.PermissionRefusal](cause); ok {
			// A failed check can concern a Session's origin principal. That principal
			// is the subject of the check, not necessarily the authenticated executor.
			actor.Authorization = audit.PermissionAuthorization(refusal.Checks, false)
		}

	}
	event := audit.Event{Evidence: actor, OrgID: org, Action: route.action, TargetKind: definition.TargetKind, Unresolved: true, Details: audit.FailureDetails{Operation: operation}}
	// Path/body tenant claims and guessed resource IDs never establish custody.
	if err := audit.RecordFailure(r.Context(), s.tx, s.auditPolicy, event, reason); err != nil {
		s.auditCaptureFailed(r, err)
		writeError(w, unavailable(audit.ErrCapture))
		return false
	}
	return true
}
func (s *Server) auditCaptureFailed(r *http.Request, err error) {
	if !errors.Is(err, audit.ErrCapture) {
		return
	}
	var count uint64
	if s.auditAdmission != nil {
		count = s.auditAdmission.captureFailures.Add(1)
	}
	s.log.ErrorContext(r.Context(), "mandatory audit capture failed", "capture_failures", count)
}
func (s *Server) writeOperationError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, audit.ErrCapture) {
		s.auditCaptureFailed(r, err)
		writeError(w, unavailable(audit.ErrCapture))
		return
	}
	if apiErr, ok := errors.AsType[apiError](err); ok {
		reason := ""
		switch apiErr.kind {
		case errForbidden:
			reason = "permission_denied"
		case errUnauthorized:
			reason = "authentication_invalid"
		}
		if reason != "" && !s.refuseOperation(w, r, reason, err) {
			return
		}
	}
	writeError(w, err)
}
func presentedCredential(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" || operatorToken(r) != "" {
		return true
	}
	_, err := r.Cookie(sessionCookieName(r))
	return err == nil
}
