package control

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/email"
)

type testServerConfig struct {
	Log                 *slog.Logger
	DeploymentMode      string
	DB                  db.Querier
	DBTX                dbTXBeginner
	TX                  TxBeginner
	Auth                auth.Authenticator
	CAS                 cas.Store
	Secrets             SecretManager
	RunEnqueuer         RunEnqueuer
	DispatchQueue       dispatch.Queue
	ScheduleEngine      ScheduleRegistrar
	EventStream         *EventStream
	WorkerTokenSecret   []byte
	WorkerTokenTTL      time.Duration
	WorkerRegisterToken string
	SetupToken          string
	AuthSecret          []byte
	PublicURL           *url.URL
	AuthProvider        AuthProvider
	Mailer              email.Sender
	MagicLinkDebugURLs  bool
	SessionTTL          time.Duration
}

func newTestServer(testCfg testServerConfig) http.Handler {
	log := testCfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg := ServerConfig{
		Log:  log,
		DB:   &fakeStore{},
		Auth: fakeAuth{},
	}
	if testCfg.DBTX != nil {
		queries := db.New(testCfg.DBTX)
		cfg.DB = queries
		cfg.TX = testCfg.DBTX
		cfg.ReadinessDB = testCfg.DBTX
		cfg.Auth = auth.NewDBAuthenticator(queries)
	}
	if testCfg.DeploymentMode != "" {
		cfg.DeploymentMode = testCfg.DeploymentMode
	}
	if testCfg.DB != nil {
		cfg.DB = testCfg.DB
	}
	if testCfg.TX != nil {
		cfg.TX = testCfg.TX
	}
	if testCfg.Auth != nil {
		cfg.Auth = testCfg.Auth
	}
	if testCfg.CAS != nil {
		cfg.CAS = testCfg.CAS
	}
	if testCfg.Secrets != nil {
		cfg.Secrets = testCfg.Secrets
	}
	if testCfg.RunEnqueuer != nil {
		cfg.RunEnqueuer = testCfg.RunEnqueuer
	}
	if testCfg.DispatchQueue != nil {
		cfg.DispatchQueue = testCfg.DispatchQueue
	}
	if testCfg.ScheduleEngine != nil {
		cfg.ScheduleEngine = testCfg.ScheduleEngine
	}
	if testCfg.EventStream != nil {
		cfg.EventStream = testCfg.EventStream
	}
	if len(testCfg.WorkerTokenSecret) > 0 {
		cfg.WorkerTokenSecret = testCfg.WorkerTokenSecret
	}
	if testCfg.WorkerTokenTTL != 0 {
		cfg.WorkerTokenTTL = testCfg.WorkerTokenTTL
	}
	if testCfg.WorkerRegisterToken != "" {
		cfg.WorkerRegisterToken = testCfg.WorkerRegisterToken
	}
	if testCfg.SetupToken != "" {
		cfg.SetupToken = testCfg.SetupToken
	}
	if len(testCfg.AuthSecret) > 0 {
		cfg.AuthSecret = testCfg.AuthSecret
	}
	if testCfg.PublicURL != nil {
		cfg.PublicURL = testCfg.PublicURL
	}
	if testCfg.AuthProvider != nil {
		cfg.AuthProvider = testCfg.AuthProvider
	}
	if testCfg.Mailer != nil {
		cfg.Mailer = testCfg.Mailer
	}
	if testCfg.MagicLinkDebugURLs {
		cfg.MagicLinkDebugURLs = testCfg.MagicLinkDebugURLs
	}
	if testCfg.SessionTTL != 0 {
		cfg.SessionTTL = testCfg.SessionTTL
	}
	handler, err := NewServer(cfg)
	if err != nil {
		panic(err)
	}
	return handler
}

func mustParseTestURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestAPIRejectsOversizedRequestBody(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}, Secrets: fakeSecrets{}})
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(strings.Repeat("x", int(apiRequestBodyLimit)+1)))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIRejectsUnsupportedAPIVersion(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"task_id":"deploy"}`))
	req.Header.Set("authorization", "Bearer test-key")
	req.Header.Set(api.APIVersionHeader, "2099-01-01")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(api.APIVersionHeader); got != api.CurrentAPIVersion {
		t.Fatalf("response %s = %q", api.APIVersionHeader, got)
	}
	if !strings.Contains(rec.Body.String(), "unsupported "+api.APIVersionHeader) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWorkerLogsRejectOversizedRequestBody(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, WorkerTokenSecret: []byte("01234567890123456789012345678901"), WorkerTokenTTL: time.Hour})
	workerToken := mintTestWorkerToken(t, handler, "00000000-0000-0000-0000-000000000401")
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/logs", strings.NewReader(strings.Repeat("x", int(workerLogRequestBodyLimit)+1)))
	req.Header.Set("authorization", "Bearer "+workerToken)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestRecoverPanicsWritesJSONError(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := server.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("content-type"); contentType != "application/json" {
		t.Fatalf("content-type = %q", contentType)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestRecoverPanicsRepanicsAfterResponseCommitted(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := server.recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		handler.ServeHTTP(rec, req)
	}()

	if recovered == nil {
		t.Fatal("expected panic after committed response")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "partial" {
		t.Fatalf("body = %s", body)
	}
}
