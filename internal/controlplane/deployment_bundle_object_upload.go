package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
)

// Preflight uses server dependencies, not the client's transfer lifetime.
const deploymentUploadPreflightTimeout = 30 * time.Second

type DeploymentUploadLimits struct {
	ProcessConcurrency      int
	OrganizationConcurrency int
	IdleTimeout             time.Duration
	MaxDuration             time.Duration
}

func (l DeploymentUploadLimits) Validate() error {
	if l.ProcessConcurrency <= 0 || l.OrganizationConcurrency <= 0 || l.OrganizationConcurrency > l.ProcessConcurrency {
		return fmt.Errorf("deployment upload concurrency must be positive with organization capacity no greater than process capacity")
	}
	if l.IdleTimeout < time.Millisecond || l.MaxDuration < l.IdleTimeout {
		return fmt.Errorf("deployment upload durations must be positive with idle timeout no greater than maximum duration")
	}
	return nil
}

func isDeploymentObjectUpload(r *http.Request) bool {
	if r.Method != http.MethodPut {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) == 5 && parts[0] == "v1" {
		return parts[1] == "deployment-bundles" && parts[3] == "objects"
	}
	return len(parts) == 9 && parts[0] == "api" && parts[1] == "projects" &&
		parts[3] == "environments" && parts[5] == "deployment-bundles" && parts[7] == "objects"
}

// An HTTP/1 upload may be rejected before any body arrives, including by outer
// authentication/admission middleware. Closing that connection lets Go send the
// response without draining the untrusted body. HTTP/2 closes only its stream.
func deploymentUploadTransport(idle time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isDeploymentObjectUpload(r) {
				if r.ProtoMajor == 1 {
					w.Header().Set("Connection", "close")
				}
				// Extend the ordinary body deadline before authorization/preflight:
				// an expired HTTP/2 read timer irreversibly closes its stream.
				// Preflight still has its own context bound, and the admitted stream
				// installs progress deadlines and requires controller support.
				_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(deploymentUploadPreflightTimeout).Add(idle))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Both bounds are per process. Organization sharing does not reserve capacity
// or provide a distributed quota. Only active organizations occupy the map.
type deploymentUploadAdmission struct {
	mu     sync.Mutex
	active map[uuid.UUID]int
	total  int
	limits DeploymentUploadLimits
}

func (a *deploymentUploadAdmission) acquire(org uuid.UUID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.total >= a.limits.ProcessConcurrency || a.active[org] >= a.limits.OrganizationConcurrency {
		return false
	}
	if a.active == nil {
		a.active = make(map[uuid.UUID]int)
	}
	a.active[org]++
	a.total++
	return true
}

func (a *deploymentUploadAdmission) release(org uuid.UUID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total--
	if a.active[org] == 1 {
		delete(a.active, org)
	} else {
		a.active[org]--
	}
}

func (s *Server) uploadDeploymentBundleObject(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	// Reject malformed descriptors before manifest or storage access. The admitted
	// manifest below remains the authority for the exact descriptor.
	supplied := bundle.Object{Digest: chi.URLParam(r, "objectDigest"), SizeBytes: r.ContentLength, MediaType: r.Header.Get("Content-Type")}
	if bundle.ValidateObject(supplied, "upload") != nil || r.Header.Get("Content-Encoding") != "" {
		writeError(w, badRequest(errors.New("deployment object length, media type, digest or encoding is invalid")))
		return
	}
	// Bound manifest reads and storage preflight as well as body transfer. Do not
	// queue requests while they occupy ordinary API admission slots.
	if !s.deploymentUploads.acquire(principal.OrgID) {
		w.Header().Set("Retry-After", "1")
		writeError(w, apiError{kind: errTooManyRequests, err: errors.New("deployment upload capacity is busy; retry the upload")})
		return
	}
	defer s.deploymentUploads.release(principal.OrgID)
	preflightCtx, finishPreflight := context.WithTimeout(r.Context(), deploymentUploadPreflightTimeout)
	defer finishPreflight()
	preflightRequest := r.WithContext(preflightCtx)
	scope, _, _, err := s.requestEnvironmentScopeFromRequest(preflightRequest, principal)
	if err != nil {
		if preflightCtx.Err() != nil {
			writeError(w, unavailable(errors.New("deployment upload preflight stopped")))
		} else {
			writeError(w, badRequest(err))
		}
		return
	}
	descriptor, err := s.deploymentFinalizer.ResolveUploadDescriptor(preflightCtx, principal, scope,
		chi.URLParam(r, "bundleDigest"), chi.URLParam(r, "objectDigest"))
	if err != nil {
		if preflightCtx.Err() != nil {
			writeError(w, unavailable(errors.New("deployment upload preflight stopped")))
		} else {
			s.writeDeploymentError(w, r, err)
		}
		return
	}
	if r.ContentLength != descriptor.SizeBytes || r.Header.Get("Content-Type") != descriptor.MediaType {
		writeError(w, badRequest(errors.New("deployment object length, media type or encoding differs from the bundle")))
		return
	}
	owner := strings.ToLower(principal.OrgID.String())
	exists, err := s.cas.HasExactQuarantine(preflightCtx, owner, descriptor)
	if preflightCtx.Err() != nil {
		writeError(w, unavailable(errors.New("deployment upload preflight stopped")))
		return
	}
	finishPreflight()
	if err != nil {
		s.writeDeploymentError(w, r, err)
		return
	}
	if exists {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	stream, err := newDeploymentUploadStream(w, r, s.deploymentUploads.limits.IdleTimeout, s.deploymentUploads.limits.MaxDuration)
	if err != nil {
		writeError(w, unavailable(errors.New("deployment upload transport is unavailable")))
		return
	}
	defer stream.close()
	releaseSecurityAdmission(r)
	err = s.cas.StreamQuarantine(stream.ctx, owner, descriptor, stream)
	if err != nil {
		switch {
		case errors.Is(err, cas.ErrDigestMismatch):
			writeError(w, badRequest(errors.New("deployment object bytes do not match the bundle")))
		case errors.Is(err, cas.ErrUploadConflict):
			writeError(w, conflict(errors.New("deployment object upload conflicted; reconcile and retry the bundle")))
		case stream.ctx.Err() != nil:
			writeError(w, unavailable(errors.New("deployment object upload stopped; reconcile and retry the bundle")))
		default:
			s.writeDeploymentError(w, r, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
