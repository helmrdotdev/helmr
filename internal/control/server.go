package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/asyncbus"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	readinessTimeout          = 2 * time.Second
	apiRequestBodyLimit       = int64(128 << 20)
	workerLogRequestBodyLimit = int64(256 << 10)
)

type secretManager interface {
	Put(ctx context.Context, orgID uuid.UUID, name string, value []byte) (db.Secret, error)
	PutScoped(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error)
	Check(ctx context.Context, orgID uuid.UUID, bindings api.SecretBindings) error
	CheckScoped(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, bindings api.SecretBindings) error
	Resolve(ctx context.Context, orgID uuid.UUID, bindings api.SecretBindings) (api.ResolvedSecrets, error)
	ResolveScoped(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, bindings api.SecretBindings) (api.ResolvedSecrets, error)
}

type Server struct {
	log                 *slog.Logger
	deploymentMode      string
	db                  db.Querier
	tx                  txBeginner
	readinessDB         db.DBTX
	auth                auth.Authenticator
	github              githubCommitResolver
	githubConnector     githubInstallationConnector
	cas                 cas.Store
	secrets             secretManager
	runEnqueuer         runEnqueuer
	dispatchQueue       dispatch.Queue
	asyncPublisher      asyncbus.Publisher
	runEvents           runEventSubscriptionNotifier
	workerLeaseScanSeed atomic.Uint64
	githubWebhookSecret []byte
	workerTokenSecret   []byte
	workerTokenTTL      time.Duration
	workerRegisterToken string
	setupToken          string
	authSecret          []byte
	publicURL           *url.URL
	authProvider        authProvider
	mailer              emailSender
	magicLinkDebugURLs  bool
	githubOAuthClientID string
	githubOAuthSecret   string
	sessionTTL          time.Duration
	magicLinkTTL        time.Duration
	deviceCodeTTL       time.Duration
	devicePollEvery     time.Duration
}

type Option func(*Server)

const (
	deploymentModeSelfHosted   = "self-hosted"
	deploymentModeManagedCloud = "managed-cloud"
)

func WithDeploymentMode(mode string) Option {
	return func(server *Server) {
		server.deploymentMode = strings.TrimSpace(mode)
	}
}

type runEnqueuer interface {
	EnqueueRun(context.Context, pgtype.UUID, pgtype.UUID) (dispatch.EnqueueResult, error)
}

type txBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type dbTXBeginner interface {
	db.DBTX
	txBeginner
}

func WithDB(queries db.Querier) Option {
	return func(server *Server) {
		server.db = queries
		if queue, ok := queries.(dispatch.Queue); ok {
			server.dispatchQueue = queue
		}
		if queries != nil && server.auth == nil {
			server.auth = auth.NewDBAuthenticator(queries)
		}
	}
}

func WithDBTX(database dbTXBeginner) Option {
	return func(server *Server) {
		queries := db.New(database)
		server.db = queries
		server.tx = database
		server.readinessDB = database
		if server.auth == nil {
			server.auth = auth.NewDBAuthenticator(queries)
		}
	}
}

func WithAuthenticator(authenticator auth.Authenticator) Option {
	return func(server *Server) {
		server.auth = authenticator
	}
}

func WithGitHubResolver(resolver githubCommitResolver) Option {
	return func(server *Server) {
		server.github = resolver
		if connector, ok := resolver.(githubInstallationConnector); ok {
			server.githubConnector = connector
		}
	}
}

func WithGitHubConnector(connector githubInstallationConnector) Option {
	return func(server *Server) {
		server.githubConnector = connector
	}
}

func WithCAS(store cas.Store) Option {
	return func(server *Server) {
		server.cas = store
	}
}

func WithSecrets(secrets secretManager) Option {
	return func(server *Server) {
		server.secrets = secrets
	}
}

func WithRunEnqueuer(enqueuer runEnqueuer) Option {
	return func(server *Server) {
		server.runEnqueuer = enqueuer
	}
}

func WithDispatchQueue(queue dispatch.Queue) Option {
	return func(server *Server) {
		server.dispatchQueue = queue
	}
}

func WithAsyncBus(queue asyncbus.Publisher) Option {
	return func(server *Server) {
		server.asyncPublisher = queue
	}
}

func WithRunEventNotifier(notifier runEventSubscriptionNotifier) Option {
	return func(server *Server) {
		server.runEvents = notifier
	}
}

func WithGitHubWebhookSecret(secret string) Option {
	return func(server *Server) {
		server.githubWebhookSecret = []byte(secret)
	}
}

func WithWorkerAuth(tokenSigningKey string, ttl time.Duration) Option {
	return func(server *Server) {
		server.workerTokenSecret = []byte(tokenSigningKey)
		if ttl <= 0 {
			ttl = defaultWorkerTokenTTL
		}
		server.workerTokenTTL = ttl
	}
}

func WithDefaultWorkerBootstrapToken(token string) Option {
	return func(server *Server) {
		server.workerRegisterToken = strings.TrimSpace(token)
	}
}

func WithInitialSetupToken(token string) Option {
	return func(server *Server) {
		server.setupToken = strings.TrimSpace(token)
	}
}

func WithUserAuth(authSecret string, publicURL string) Option {
	return func(server *Server) {
		server.authSecret = []byte(authSecret)
		if parsed, err := url.Parse(publicURL); err == nil && parsed.Scheme != "" && parsed.Host != "" {
			server.publicURL = parsed
		}
	}
}

func WithGitHubOAuth(clientID string, clientSecret string) Option {
	return func(server *Server) {
		server.githubOAuthClientID = clientID
		server.githubOAuthSecret = clientSecret
	}
}

func WithAuthProvider(provider authProvider) Option {
	return func(server *Server) {
		server.authProvider = provider
	}
}

func WithMagicLinkMailer(mailer magicLinkMailer) Option {
	return func(server *Server) {
		server.mailer = legacyMagicLinkEmailSender{mailer: mailer}
	}
}

func WithEmailSender(sender emailSender) Option {
	return func(server *Server) {
		server.mailer = sender
	}
}

func WithDisabledEmailSender() Option {
	return func(server *Server) {
		server.mailer = unconfiguredEmailSender{}
	}
}

func WithLogEmailSender() Option {
	return func(server *Server) {
		server.mailer = logEmailSender{log: server.log}
	}
}

func WithMagicLinkDebugURLs(enabled bool) Option {
	return func(server *Server) {
		server.magicLinkDebugURLs = enabled
	}
}

func WithSMTPMagicLinkMailer(addr string, username string, password string, from string) Option {
	return WithSMTPEmailSender(addr, username, password, from)
}

func WithSMTPEmailSender(addr string, username string, password string, from string) Option {
	return func(server *Server) {
		server.mailer = smtpEmailSender{
			addr:     addr,
			username: username,
			password: password,
			from:     from,
		}
	}
}

func WithResendEmailSender(apiKey string, from string) Option {
	return func(server *Server) {
		server.mailer = newResendEmailSender(apiKey, from)
	}
}

func WithSessionTTL(ttl time.Duration) Option {
	return func(server *Server) {
		server.sessionTTL = ttl
	}
}

func WithMagicLinkTTL(ttl time.Duration) Option {
	return func(server *Server) {
		server.magicLinkTTL = ttl
	}
}

func WithDeviceCode(ttl time.Duration, pollEvery time.Duration) Option {
	return func(server *Server) {
		server.deviceCodeTTL = ttl
		server.devicePollEvery = pollEvery
	}
}

func New(log *slog.Logger, opts ...Option) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	server := &Server{log: log, deploymentMode: deploymentModeSelfHosted}
	for _, opt := range opts {
		opt(server)
	}
	if server.mailer == nil {
		if server.magicLinkDebugURLs {
			server.mailer = logEmailSender{log: log}
		} else {
			server.mailer = unconfiguredEmailSender{}
		}
	}
	if server.authProvider == nil && server.publicURL != nil && server.githubOAuthClientID != "" && server.githubOAuthSecret != "" {
		server.authProvider = newGitHubOAuthProvider(server.githubOAuthClientID, server.githubOAuthSecret, server.publicURL)
	}
	router := chi.NewRouter()
	router.Use(server.recoverPanics)
	router.Use(otelhttp.NewMiddleware("helmr-control"))
	router.Get("/healthz", server.healthz)
	router.Get("/readyz", server.readyz)
	router.Post("/webhooks/github", server.githubWebhook)
	router.Get("/waitpoints/respond", server.waitpointConfirmationPage)
	router.Route("/api", server.mountAPIRoutes)
	router.NotFound(server.notFound)
	return router
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("control handler panic", "panic", recovered, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, errors.New("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) mountAPIRoutes(r chi.Router) {
	r.Use(limitRequestBody(apiRequestBodyLimit))
	s.mountAuthRoutes(r)
	s.mountWaitpointTokenRoutes(r)
	s.mountOwnerRoutes(r)
	s.mountRunRoutes(r)
	s.mountWorkerRoutes(r)
}

func (s *Server) mountAuthRoutes(r chi.Router) {
	r.Post("/auth/github/start", s.githubStart)
	r.Post("/auth/github/invite/start", s.githubInviteStart)
	r.Post("/auth/github/finish", s.githubFinish)
	r.Post("/auth/magic-link/start", s.magicLinkStart)
	r.Post("/auth/magic-link/invite/start", s.magicLinkInviteStartRoute)
	r.Post("/auth/magic-link/finish", s.magicLinkFinish)
	r.Post("/auth/device/start", s.startDeviceCode)
	r.Post("/auth/device/token", s.deviceToken)
	r.Post("/auth/logout", s.logout)
	r.Group(func(r chi.Router) {
		r.Use(s.requireSession)
		r.Get("/me", s.me)
		r.Post("/organizations", s.createOrganization)
		r.Get("/auth/device/status", s.deviceStatus)
		r.Post("/auth/device/approve", s.approveDeviceCode)
		r.Post("/auth/device/deny", s.denyDeviceCode)
	})
}

func (s *Server) mountOwnerRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionPermission(auth.PermissionAPIKeysManage, next)
		})
		r.Get("/api-keys", s.listAPIKeys)
		r.Post("/api-keys", s.issueAPIKey)
		r.Delete("/api-keys/{id}", s.revokeAPIKey)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionPermission(auth.PermissionMembersManage, next)
		})
		r.Get("/members", s.listMembers)
		r.Patch("/members/{userID}", s.updateMemberRole)
		r.Delete("/members/{userID}", s.removeMember)
		r.Get("/invitations", s.listInvitations)
		r.Post("/invitations", s.createInvitation)
		r.Delete("/invitations/{id}", s.revokeInvitation)
	})
	r.Group(func(r chi.Router) {
		r.Use(s.requireSession)
		r.Get("/projects", s.listProjects)
		r.Get("/projects/{projectID}", s.getProject)
		r.Get("/projects/{projectID}/environments/{environmentID}", s.getEnvironment)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionPermission(auth.PermissionProjectsManage, next)
		})
		r.Post("/projects", s.createProject)
		r.Patch("/projects/{projectID}", s.updateProject)
		r.Delete("/projects/{projectID}", s.archiveProject)
		r.Post("/projects/{projectID}/environments", s.createEnvironment)
		r.Patch("/projects/{projectID}/environments/{environmentID}", s.updateEnvironment)
		r.Delete("/projects/{projectID}/environments/{environmentID}", s.archiveEnvironment)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requirePermission(auth.PermissionWaitpointPolicies, next)
		})
		r.Get("/waitpoint-policies", s.listWaitpointPolicies)
		r.Post("/waitpoint-policies", s.createWaitpointPolicy)
		r.Get("/waitpoint-policies/{name}", s.getWaitpointPolicy)
		r.Patch("/waitpoint-policies/{name}", s.updateWaitpointPolicy)
		r.Post("/waitpoint-policies/{name}/disable", s.disableWaitpointPolicy)
	})
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		r.Get("/deployments/current", s.getCurrentDeployment)
		r.Get("/deployments/{deploymentID}", s.getDeployment)
		r.Post("/deployments/{deployment}/promote", s.promoteDeployment)
		r.Post("/deployments", s.createDeployment)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionPermission(auth.PermissionGitHubManage, next)
		})
		r.Get("/github/installations", s.listGitHubInstallations)
		r.Get("/github/installations/{installationID}/repositories", s.listGitHubInstallationRepositories)
		r.Post("/github/setup/start", s.githubSetupStart)
	})
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		r.Put("/projects/{projectID}/github/repositories/{githubRepositoryID}", s.connectProjectGitHubRepository)
		r.Delete("/projects/{projectID}/github/repositories/{githubRepositoryID}", s.disconnectProjectGitHubRepository)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requirePermission(auth.PermissionSecretsWrite, next)
		})
		r.Get("/secrets", s.listSecrets)
		r.Put("/secrets/{name}", s.setSecret)
	})
}

func (s *Server) mountRunRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		r.Post("/runs", s.createRun)
		r.Get("/runs", s.listRuns)
		r.Get("/runs/counts", s.countRuns)
		r.Get("/runs/{id}", s.getRun)
		r.Get("/runs/{id}/events", s.getRunEvents)
		r.Get("/runs/{id}/logs", s.getRunLogs)
	})
}

func (s *Server) mountWaitpointTokenRoutes(r chi.Router) {
	r.Post("/waitpoints/tokens/{tokenID}/respond", s.respondWaitpointToken)
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		r.Post("/waitpoints", s.createWaitpoint)
		r.Post("/waitpoints/{waitpointID}/respond", s.respondWaitpoint)
		r.Post("/waitpoints/tokens", s.createWaitpointToken)
	})
}

func (s *Server) mountWorkerRoutes(r chi.Router) {
	r.Route("/worker", func(r chi.Router) {
		r.Post("/register", s.workerRegister)
		r.Post("/auth/token", s.workerAuthToken)
		r.Group(func(r chi.Router) {
			r.Use(s.requireWorker)
			r.Post("/activate", s.workerActivate)
			r.Post("/drain", s.workerDrain)
			r.Get("/status", s.workerStatus)
			r.Post("/deployments/lease", s.workerLeaseDeploymentBuild)
			r.Post("/deployments/complete", s.workerCompleteDeploymentBuild)
			r.Post("/executions/lease", s.workerLease)
			r.Post("/executions/start", s.workerStart)
			r.Post("/executions/restores/ack", s.workerAcknowledgeRestore)
			r.Post("/executions/renew", s.workerRenew)
			r.Post("/executions/release", s.workerRelease)
			r.With(limitRequestBody(workerLogRequestBodyLimit)).Post("/executions/logs", s.workerAppendLogs)
			r.Post("/executions/log-entries", s.workerRecordLogEntry)
			r.Post("/executions/events", s.workerEmitEvent)
			r.Post("/executions/waitpoints", s.workerCreateWaitpoint)
			r.Post("/executions/checkpoints/ready", s.workerCheckpointReady)
			r.Post("/executions/checkpoints/failed", s.workerCheckpointFailed)
		})
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.readinessDB == nil {
		s.writeReadinessUnavailable(w, errors.New("database readiness is not configured"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	var version int
	var dirty bool
	if err := s.readinessDB.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		s.writeReadinessUnavailable(w, fmt.Errorf("database schema is not ready: %w", err))
		return
	}
	if dirty {
		s.writeReadinessUnavailable(w, errors.New("database schema migration is dirty"))
		return
	}
	currentVersion, err := schema.CurrentVersion()
	if err != nil {
		s.writeReadinessUnavailable(w, fmt.Errorf("read embedded migration version: %w", err))
		return
	}
	if version < int(currentVersion) {
		s.writeReadinessUnavailable(w, fmt.Errorf("database schema version is %d, required %d", version, currentVersion))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) writeReadinessUnavailable(w http.ResponseWriter, err error) {
	s.log.Warn("control readiness check failed", "error", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func decodeJSON(r *http.Request, out any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain a single JSON value")
	}
	return nil
}

func limitRequestBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeError(w, http.StatusRequestEntityTooLarge, errors.New("request body is too large"))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) userAuthConfigured() error {
	if s.db == nil {
		return errors.New("run storage is not configured")
	}
	if len(s.authSecret) == 0 {
		return errors.New("user authentication is not configured")
	}
	if s.publicURL == nil {
		return errors.New("public URL is not configured")
	}
	return auth.ValidateTokenSecret(s.authSecret)
}

func (s *Server) effectiveSessionTTL() time.Duration {
	if s.sessionTTL > 0 {
		return s.sessionTTL
	}
	return 30 * 24 * time.Hour
}

func (s *Server) effectiveMagicLinkTTL() time.Duration {
	if s.magicLinkTTL > 0 {
		return s.magicLinkTTL
	}
	return 15 * time.Minute
}

func (s *Server) effectiveDeviceCodeTTL() time.Duration {
	if s.deviceCodeTTL > 0 {
		return s.deviceCodeTTL
	}
	return 10 * time.Minute
}

func (s *Server) effectiveDevicePollEvery() time.Duration {
	if s.devicePollEvery > 0 {
		return s.devicePollEvery
	}
	return 5 * time.Second
}
