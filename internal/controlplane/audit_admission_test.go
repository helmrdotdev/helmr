package controlplane

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"golang.org/x/time/rate"
)

func TestAuditAdmissionBoundsAndContainmentReserve(t *testing.T) {
	guard := newAuditAdmission()
	s := &Server{auditAdmission: guard, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.auditRouter = chi.NewRouter()
	s.auditRouter.Post("/v1/sessions", func(http.ResponseWriter, *http.Request) {})
	s.auditRouter.Post("/v1/sessions/{sessionID}/cancel", func(http.ResponseWriter, *http.Request) {})
	s.auditRouter.Delete("/api/projects/{project}/environments/{environment}/api-keys/{id}", func(http.ResponseWriter, *http.Request) {})

	for i := 0; i < cap(guard.active); i++ {
		guard.active <- struct{}{}
	}
	entered := false
	handler := s.limitSecurityAdmission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered = true; w.WriteHeader(204) }))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/sessions", nil))
	if entered || rr.Code != 429 || guard.dropped.Load() != 1 {
		t.Fatal("ordinary overload was admitted", rr.Code)
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/sessions/123/cancel", nil))
	if !entered || rr.Code != 204 {
		t.Fatal("containment reserve unavailable", rr.Code)
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("DELETE", "/api/projects/project/environments/test/api-keys/key", nil))
	if rr.Code != 204 {
		t.Fatalf("key revocation reserve unavailable: %d", rr.Code)
	}
	for range auditPrincipalCapacity {
		guard.principals[uuid.NewV7().String()] = auditPrincipalBucket{limiter: rate.NewLimiter(1, 1), seen: time.Now()}
	}
	principal := auth.Principal{Kind: auth.PrincipalKindSession, UserID: uuid.NewV7(), OrgID: uuid.NewV7(), SessionID: uuid.NewV7()}
	if !s.admitPrincipal(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/sessions", nil), principal) || len(guard.principals) != auditPrincipalCapacity {
		t.Fatal("principal cache capacity blocked a new caller or grew")
	}
	if !s.admitPrincipal(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/sessions/123/cancel", nil), principal) {
		t.Fatal("principal capacity consumed containment reserve")
	}
}

func TestAuditAdmissionEstablishedStreamsReleaseOperationSlots(t *testing.T) {
	s := &Server{auditAdmission: newAuditAdmission(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	release := make(chan struct{})
	ready := make(chan struct{}, 32)
	var wg sync.WaitGroup
	handler := s.limitSecurityAdmission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.admitSecurityKey(w, r, "stream-principal", true) {
			return
		}
		if r.URL.Path == "/v1/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			ready <- struct{}{}
			<-release
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	for range 32 {
		wg.Go(func() {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/v1/events", nil))
		})
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatal("stream could not establish")
		}
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", "/v1/agents", nil))
	if rr.Code != 204 {
		t.Fatalf("streams starved operations: %d", rr.Code)
	}
	// The handler defer must not release an already released slot a second time.
	close(release)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("double release blocked stream completion")
	}
}

func TestAuditAdmissionPrincipalCannotOccupyAllSlots(t *testing.T) {
	for _, route := range []struct{ name, method, path string }{{"capture", "POST", "/v1/sessions"}, {"metadata", "GET", "/api/projects/project/environments/test/usage"}} {
		t.Run(route.name, func(t *testing.T) {
			s := &Server{auditAdmission: newAuditAdmission(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			s.auditRouter = chi.NewRouter()
			s.auditRouter.Post("/v1/sessions", func(http.ResponseWriter, *http.Request) {})
			s.auditRouter.Get("/api/projects/{project}/environments/{environment}/usage", func(http.ResponseWriter, *http.Request) {})
			principals := map[string]uuid.UUID{"first": uuid.NewV7(), "second": uuid.NewV7()}
			ready, release := make(chan struct{}, auditPrincipalConcurrency), make(chan struct{})
			var wg sync.WaitGroup
			handler := s.limitSecurityAdmission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !s.admitPrincipal(w, r, auth.Principal{Kind: auth.PrincipalKindSession, UserID: principals[r.Header.Get("Test-Principal")], SessionID: uuid.NewV7()}) {
					return
				}
				if r.Header.Get("Test-Hold") == "true" {
					ready <- struct{}{}
					<-release
				}
				w.WriteHeader(204)
			}))
			request := func(key string, hold bool) int {
				r := httptest.NewRequest(route.method, route.path, nil)
				r.Header.Set("Test-Principal", key)
				if hold {
					r.Header.Set("Test-Hold", "true")
				}
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, r)
				return rr.Code
			}
			for range auditPrincipalConcurrency {
				wg.Go(func() { request("first", true) })
			}
			defer func() { close(release); wg.Wait() }()
			for range auditPrincipalConcurrency {
				select {
				case <-ready:
				case <-time.After(3 * time.Second):
					t.Fatal("principal did not fill its slots")
				}
			}
			if code := request("first", false); code != 429 {
				t.Fatalf("principal exceeded concurrency: %d", code)
			}
			if code := request("second", false); code != 204 {
				t.Fatalf("other principal starved: %d", code)
			}
			if route.name == "metadata" {
				for range 250 {
					if code := request("second", false); code != 204 {
						t.Fatalf("metadata spent capture tokens: %d", code)
					}
				}
			}

		})
	}
}

func TestAuditControlContainmentPreservedAcrossRequestNames(t *testing.T) {
	for _, control := range []struct {
		path     string
		reserved bool
	}{
		{"/v1/sessions/{sessionID}/cancel", true},
		{"/v1/sessions/{sessionID}/interrupt", true},
		{"/v1/sessions/{sessionID}/turns/{turnID}/interrupt", true},
		{"/v1/commands/{commandID}/cancel", true},
		{"/v1/sessions/{sessionID}/resume", false},
		{"/v1/sessions/{sessionID}/close", false},
	} {
		for _, boundary := range []string{"global", "principal"} {
			t.Run(boundary+control.path, func(t *testing.T) {
				guard := newAuditAdmission()
				s := &Server{auditAdmission: guard, log: slog.New(slog.NewTextHandler(io.Discard, nil)), auditRouter: chi.NewRouter()}
				s.auditRouter.Post(control.path, func(http.ResponseWriter, *http.Request) {})
				principal := auth.Principal{Kind: auth.PrincipalKindSession, UserID: uuid.NewV7(), OrgID: uuid.NewV7(), SessionID: uuid.NewV7()}
				if boundary == "global" {
					for range cap(guard.active) {
						guard.active <- struct{}{}
					}
				} else {
					limiter := rate.NewLimiter(0, 1)
					limiter.Allow()
					key := string(principal.Kind) + ":" + principal.UserID.String() + ":" + principal.APIKeyID.String() + ":" + principal.OrgID.String()
					guard.principals[key] = auditPrincipalBucket{limiter: limiter, seen: time.Now()}
				}
				h := s.limitSecurityAdmission(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if boundary == "principal" && !s.admitPrincipal(w, r, principal) {
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, control.path, nil))
				want := http.StatusTooManyRequests
				if control.reserved {
					want = http.StatusNoContent
				}
				if rr.Code != want {
					t.Fatalf("status=%d want=%d", rr.Code, want)
				}
			})
		}
	}
}
