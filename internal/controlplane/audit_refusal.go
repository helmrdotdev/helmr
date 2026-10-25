package controlplane

import (
	"errors"
	"net/http"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/audit"
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
func (s *Server) refuseOperation(w http.ResponseWriter, r *http.Request, reason string) bool {
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
	actor := audit.Anonymous("presented_credential")
	org := uuid.Nil()
	if p.UserID != uuid.Nil() || p.APIKeyID != uuid.Nil() {
		actor = audit.FromPrincipal(p)
		actor.Authorization = audit.Authorization{Basis: "credential", Decision: "denied", Reason: reason}
		org = p.OrgID
	}
	if host := workerFromContext(r.Context()); host.HostID != uuid.Nil() {
		// The authenticated Host does not prove the tenant/runtime IDs in the
		// rejected body. Keep custody at installation scope.
		actor = audit.System("worker-host")
		actor.CredentialKind, actor.CredentialID, actor.CredentialVersion = "worker_host", host.HostID, host.HostClaimVersion
		actor.Authorization = audit.Authorization{Basis: "credential", Decision: "denied", Reason: reason}
		org = uuid.Nil()
		if !s.admitSecurityKey(w, r, "worker_host:"+host.HostID.String()) {
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
	event := audit.Event{Actor: actor, OrgID: org, Action: route.action, TargetKind: definition.TargetKind, Unresolved: true, Details: audit.FailureDetails{Operation: operation}}
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
	var apiErr apiError
	if errors.As(err, &apiErr) {
		reason := ""
		switch apiErr.kind {
		case errForbidden:
			reason = "permission_denied"
		case errUnauthorized:
			reason = "authentication_invalid"
		}
		if reason != "" && !s.refuseOperation(w, r, reason) {
			return
		}
	}
	writeError(w, err)
}
func presentedCredential(r *http.Request) bool {
	if r.Header.Get("Authorization") != "" {
		return true
	}
	_, err := r.Cookie(sessionCookieName(r))
	return err == nil
}
