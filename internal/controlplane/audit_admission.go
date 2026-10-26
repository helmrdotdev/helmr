package controlplane

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
	"golang.org/x/time/rate"
)

// These are per-process ceilings, not a distributed quota. Containment keeps
// separate concurrency and rate capacity when ordinary requests are saturated.
const auditPrincipalCapacity = 4096
const auditPrincipalConcurrency = 12

type auditPrincipalBucket struct {
	limiter *rate.Limiter
	seen    time.Time
	active  int
}
type auditAdmissionContextKey struct{}
type auditAdmissionLease struct{ releases []func() }

type auditAdmission struct {
	global                *rate.Limiter
	containment           *rate.Limiter
	active                chan struct{}
	containmentActive     chan struct{}
	mu                    sync.Mutex
	principals            map[string]auditPrincipalBucket
	containmentPrincipals map[string]auditPrincipalBucket
	worker                *rate.Limiter
	workerActive          chan struct{}
	dropped               atomic.Uint64
	captureFailures       atomic.Uint64
}

func newAuditAdmission() *auditAdmission {
	return &auditAdmission{global: rate.NewLimiter(400, 800), containment: rate.NewLimiter(10, 20), active: make(chan struct{}, 16), containmentActive: make(chan struct{}, 4), principals: make(map[string]auditPrincipalBucket), containmentPrincipals: make(map[string]auditPrincipalBucket), worker: rate.NewLimiter(20, 40), workerActive: make(chan struct{}, 8)}
}
func (s *Server) containmentRequest(r *http.Request) bool {
	_, operation, ok := s.requestAuditRoute(r)
	if !ok {
		return false
	}
	switch operation {
	case "DELETE /api/projects/{project}/environments/{environment}/api-keys/{id}",
		"POST /admin/api/v1/worker-groups/{groupID}/disable",
		"POST /admin/api/v1/worker-groups/{groupID}/pause",
		"POST /admin/api/v1/worker-groups/{groupID}/pools/{poolID}/disable",
		"POST /api/auth/logout",
		"POST /api/projects/{project}/environments/{environment}/commands/{commandID}/cancel",
		"POST /api/projects/{project}/environments/{environment}/secrets/{secret}/revoke",
		"POST /api/projects/{project}/environments/{environment}/sessions/{sessionID}/cancel",
		"POST /api/projects/{project}/environments/{environment}/sessions/{sessionID}/interrupt",
		"POST /api/projects/{project}/environments/{environment}/sessions/{sessionID}/turns/{turnID}/interrupt",
		"POST /v1/commands/{commandID}/cancel",
		"POST /v1/secrets/{secret}/revoke",
		"POST /v1/sessions/{sessionID}/cancel",
		"POST /v1/sessions/{sessionID}/interrupt",
		"POST /v1/sessions/{sessionID}/turns/{turnID}/interrupt":
		return true
	}
	return false
}

func (s *Server) limitSecurityAdmission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auditAdmission == nil || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || (!strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/v1/") && !strings.HasPrefix(r.URL.Path, "/admin/")) {
			next.ServeHTTP(w, r)
			return
		}
		guard := s.auditAdmission

		active := guard.active
		admitted := false
		if guard.global.Allow() {
			select {
			case active <- struct{}{}:
				admitted = true
			default:
			}
		}
		// Containment uses ordinary capacity first, then its protected reserve.
		if !admitted && s.containmentRequest(r) && guard.containment.Allow() {
			active = guard.containmentActive
			select {
			case active <- struct{}{}:
				admitted = true
			default:
			}
		}
		if !admitted {
			s.securityAdmissionRejected(w)
			return
		}
		lease := &auditAdmissionLease{}
		r = r.WithContext(context.WithValue(r.Context(), auditAdmissionContextKey{}, lease))
		var once sync.Once
		release := func() {
			once.Do(func() {
				for _, done := range lease.releases {
					done()
				}
				<-active
			})
		}
		defer release()
		if _, ok := w.(http.Flusher); ok {
			w = &auditAdmissionWriter{ResponseWriter: w, release: release}
		}

		next.ServeHTTP(w, r)
	})
}
func (s *Server) admitPrincipal(w http.ResponseWriter, r *http.Request, p auth.Principal) bool {
	route, _, ok := s.requestAuditRoute(r)
	key := string(p.Kind) + ":" + p.UserID.String() + ":" + p.APIKeyID.String() + ":" + p.OrgID.String()
	return s.admitSecurityKey(w, r, key, !ok || route.action != "")
}
func (s *Server) admitSecurityKey(w http.ResponseWriter, r *http.Request, key string, capture bool) bool {
	guard := s.auditAdmission
	if guard == nil {
		return true
	}
	now := time.Now()
	guard.mu.Lock()
	principals := guard.principals
	if s.containmentRequest(r) {
		principals = guard.containmentPrincipals
	}
	bucket, ok := principals[key]
	if !ok {
		evictOldestAuditBucket(principals)

		bucket = auditPrincipalBucket{limiter: rate.NewLimiter(100, 200)}
	}
	bucket.seen = now
	lease, _ := r.Context().Value(auditAdmissionContextKey{}).(*auditAdmissionLease)
	allowed := bucket.active < auditPrincipalConcurrency && (!capture || bucket.limiter.AllowN(now, 1))
	if allowed && lease != nil {
		bucket.active++
		lease.releases = append(lease.releases, func() {
			guard.mu.Lock()
			current := principals[key]
			current.active--
			principals[key] = current
			guard.mu.Unlock()
		})
	}
	principals[key] = bucket
	guard.mu.Unlock()
	if !allowed {
		s.securityAdmissionRejected(w)
	}
	return allowed
}
func (s *Server) securityAdmissionRejected(w http.ResponseWriter) {
	count := s.auditAdmission.dropped.Add(1)
	// Bounded logarithmic reporting avoids turning rejected traffic into log load.
	if count&(count-1) == 0 {
		s.log.Warn("security admission capacity exceeded", "rejected_requests", count)
	}
	w.Header().Set("Retry-After", "1")
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{"code": "rate_limited", "message": "request admission capacity exceeded"}})
}

// An established SSE stream does not occupy a bounded operation slot forever.
// Only the server's response content type releases it, never request headers.
type auditAdmissionWriter struct {
	http.ResponseWriter
	release func()
}

func (w *auditAdmissionWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *auditAdmissionWriter) Flush() {
	if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		w.release()
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

// The global budget remains the backstop if churn evicts a key. Cardinality
// pressure must not lock every newly authenticated principal out for minutes.
func evictOldestAuditBucket(buckets map[string]auditPrincipalBucket) {
	if len(buckets) < auditPrincipalCapacity {
		return
	}
	oldestKey := ""
	var oldest time.Time
	for key, bucket := range buckets {
		if bucket.active == 0 && (oldestKey == "" || bucket.seen.Before(oldest)) {
			oldestKey, oldest = key, bucket.seen
		}
	}
	delete(buckets, oldestKey)
}
