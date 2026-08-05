package controlplane

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
	"strconv"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/enrollment"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/imagecache"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	readinessTimeout           = 2 * time.Second
	apiRequestBodyLimit        = int64(128 << 20)
	deploymentRequestBodyLimit = archive.MaxSourceArtifactBytes + 2<<20
	workerLogRequestBodyLimit  = int64(256 << 10)
	taskCompletionBodyLimit    = int64(17 << 20)
	workspaceExecResultLimit   = int64(10 << 20)
	secretRequestBodyLimit     = int64(1 << 20)
	maxPageSize                = int32(500)
	defaultRunLeaseTTL         = 5 * time.Minute
)

type SecretManager interface {
	Create(ctx context.Context, environmentID uuid.UUID, name string, value []byte, idempotencyKey string) (db.GetSecretSnapshotRow, error)
	Rotate(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID, value []byte, idempotencyKey string) (db.GetSecretSnapshotRow, error)
	PutScoped(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error)
	Revoke(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID, idempotencyKey string) (db.GetSecretSnapshotRow, error)
	CheckScopedNames(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, names []string) error
	ResolveScopedNames(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, names []string) (secret.Resolved, error)
}

type PlatformArtifactLocker interface {
	With(context.Context, []string, func() error) error
}

type RegistryCredentialOpener interface {
	OpenRegistryCredential(uuid.UUID, db.Secret, db.SecretVersion) ([]byte, error)
}

type SubjectEventReader interface {
	ReadSubject(context.Context, uuid.UUID, string, uuid.UUID, int64, func(api.RunEvent) error, func() error) error
}

type Server struct {
	log                   *slog.Logger
	deploymentMode        string
	db                    db.Querier
	tx                    TxBeginner
	readinessDB           db.DBTX
	auth                  auth.Authenticator
	cas                   cas.Store
	buildPolicy           *deployment.BuildPolicy
	platformStore         cas.Reader
	platformArtifactLocks PlatformArtifactLocker
	secrets               SecretManager
	secretDelivery        SecretDeliveryOpener
	registryCredentials   RegistryCredentialOpener
	cacheRepositories     imagecache.RepositoryProvisioner
	workspaceFencingKey   workspace.FencingKey
	tokenCredentialKey    auth.CredentialKey
	eventStream           SubjectEventReader
	telemetryReader       telemetry.Reader
	workerTokenSigningKey []byte
	workerTokenTTL        time.Duration
	runLeaseTTL           time.Duration
	runFinalizationTTL    time.Duration
	workerEnrollment      *enrollment.Verifier
	workerEnrollmentGuard *workerEnrollmentGuard
	capacityTokenHash     []byte
	setupToken            string
	authKeys              auth.Keys
	publicURL             *url.URL
	authProvider          AuthProvider
	mailer                email.Sender
	magicLinkDebugURLs    bool
	sessionTTL            time.Duration
	magicLinkTTL          time.Duration
	deviceCodeTTL         time.Duration
	devicePollEvery       time.Duration
}

const (
	deploymentModeSelfHosted   = "self-hosted"
	deploymentModeManagedCloud = "managed-cloud"
)

type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type ServerConfig struct {
	Log            *slog.Logger
	DeploymentMode string

	DB          db.Querier
	TX          TxBeginner
	ReadinessDB db.DBTX

	Auth                  auth.Authenticator
	CAS                   cas.Store
	BuildPolicy           *deployment.BuildPolicy
	PlatformStore         cas.Reader
	PlatformArtifactLocks PlatformArtifactLocker
	Secrets               SecretManager
	SecretDelivery        SecretDeliveryOpener
	RegistryCredentials   RegistryCredentialOpener
	CacheRepositories     imagecache.RepositoryProvisioner
	WorkspaceFencingKey   workspace.FencingKey
	TokenCredentialKey    auth.CredentialKey
	EventStream           SubjectEventReader
	TelemetryReader       telemetry.Reader
	Mailer                email.Sender
	AuthProvider          AuthProvider

	WorkerTokenSigningKey []byte
	WorkerTokenTTL        time.Duration
	RunLeaseTTL           time.Duration
	RunFinalizationTTL    time.Duration
	WorkerEnrollment      *enrollment.Verifier
	CapacityToken         string
	SetupToken            string
	AuthKey               []byte
	PublicURL             *url.URL

	MagicLinkDebugURLs bool
	SessionTTL         time.Duration
	MagicLinkTTL       time.Duration
	DeviceCodeTTL      time.Duration
	DevicePollEvery    time.Duration
}

func NewServer(cfg ServerConfig) (http.Handler, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.DB == nil {
		return nil, errors.New("control plane database is required")
	}
	if cfg.TX == nil {
		return nil, errors.New("control plane transaction database is required")
	}
	if cfg.Auth == nil {
		return nil, errors.New("control plane authenticator is required")
	}
	if cfg.PlatformArtifactLocks == nil {
		return nil, errors.New("platform artifact locks are required")
	}
	deploymentMode := strings.TrimSpace(cfg.DeploymentMode)
	if deploymentMode == "" {
		deploymentMode = deploymentModeSelfHosted
	}
	if deploymentMode != deploymentModeSelfHosted && deploymentMode != deploymentModeManagedCloud {
		return nil, errors.New("deployment mode must be self-hosted or managed-cloud")
	}
	if cfg.WorkerEnrollment == nil {
		return nil, errors.New("worker enrollment configuration is required")
	}
	capacityTokenHash, err := hashCapacityToken(cfg.CapacityToken)
	if err != nil {
		return nil, err
	}
	if cfg.SecretDelivery == nil {
		return nil, errors.New("secret delivery opener is required")
	}
	if cfg.RegistryCredentials == nil {
		return nil, errors.New("registry credential opener is required")
	}
	if !cfg.WorkspaceFencingKey.Valid() {
		return nil, errors.New("workspace fencing key is required")
	}
	if !cfg.TokenCredentialKey.Valid() {
		return nil, errors.New("token credential key is required")
	}
	authKeys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		return nil, err
	}
	if err := auth.ValidateWorkerTokenSigningKey(cfg.WorkerTokenSigningKey); err != nil {
		return nil, err
	}
	telemetryReader := cfg.TelemetryReader
	if telemetryReader == nil {
		return nil, errors.New("control plane telemetry reader is required")
	}
	mailer := cfg.Mailer
	if mailer == nil {
		if cfg.MagicLinkDebugURLs {
			mailer = email.LogSender{Log: log}
		} else {
			mailer = email.Unconfigured{}
		}
	}
	workerTokenTTL := cfg.WorkerTokenTTL
	if workerTokenTTL <= 0 {
		workerTokenTTL = defaultWorkerTokenTTL
	}
	runLeaseTTL := cfg.RunLeaseTTL
	if runLeaseTTL <= 0 {
		runLeaseTTL = defaultRunLeaseTTL
	}
	if runLeaseTTL < workerapi.RunLeaseMinTTL {
		return nil, fmt.Errorf("run lease TTL must be at least %s", workerapi.RunLeaseMinTTL)
	}
	runFinalizationTTL := cfg.RunFinalizationTTL
	if runFinalizationTTL <= 0 {
		runFinalizationTTL = 30 * time.Minute
	}
	if runFinalizationTTL < workerapi.RunFinalizationMinTTL {
		return nil, fmt.Errorf(
			"run finalization TTL must be at least %s",
			workerapi.RunFinalizationMinTTL,
		)
	}
	server := &Server{
		log:                   log,
		deploymentMode:        deploymentMode,
		db:                    cfg.DB,
		tx:                    cfg.TX,
		readinessDB:           cfg.ReadinessDB,
		auth:                  cfg.Auth,
		cas:                   cfg.CAS,
		buildPolicy:           cfg.BuildPolicy,
		platformStore:         cfg.PlatformStore,
		platformArtifactLocks: cfg.PlatformArtifactLocks,
		secrets:               cfg.Secrets,
		secretDelivery:        cfg.SecretDelivery,
		registryCredentials:   cfg.RegistryCredentials,
		cacheRepositories:     cfg.CacheRepositories,
		workspaceFencingKey:   cfg.WorkspaceFencingKey,
		tokenCredentialKey:    cfg.TokenCredentialKey,
		eventStream:           cfg.EventStream,
		telemetryReader:       telemetryReader,
		workerTokenSigningKey: cfg.WorkerTokenSigningKey,
		workerTokenTTL:        workerTokenTTL,
		runLeaseTTL:           runLeaseTTL,
		runFinalizationTTL:    runFinalizationTTL,
		workerEnrollment:      cfg.WorkerEnrollment,
		workerEnrollmentGuard: newWorkerEnrollmentGuard(),
		capacityTokenHash:     capacityTokenHash,
		setupToken:            cfg.SetupToken,
		authKeys:              authKeys,
		publicURL:             cfg.PublicURL,
		authProvider:          cfg.AuthProvider,
		mailer:                mailer,
		magicLinkDebugURLs:    cfg.MagicLinkDebugURLs,
		sessionTTL:            cfg.SessionTTL,
		magicLinkTTL:          cfg.MagicLinkTTL,
		deviceCodeTTL:         cfg.DeviceCodeTTL,
		devicePollEvery:       cfg.DevicePollEvery,
	}
	router := chi.NewRouter()
	router.Use(server.recoverPanics)
	router.Use(otelhttp.NewMiddleware("helmr-controlplane"))
	server.mountRoutes(router)
	router.NotFound(server.notFound)
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		server.methodNotAllowed(router, w, r)
	})
	return router, nil
}

func (s *Server) methodNotAllowed(routes chi.Routes, w http.ResponseWriter, r *http.Request) {
	for _, method := range []string{
		http.MethodConnect,
		http.MethodDelete,
		http.MethodGet,
		http.MethodHead,
		http.MethodOptions,
		http.MethodPatch,
		http.MethodPost,
		http.MethodPut,
		http.MethodTrace,
	} {
		if routes.Match(chi.NewRouteContext(), method, r.URL.Path) {
			w.Header().Add("Allow", method)
		}
	}
	writeError(w, apiError{kind: errMethodNotAllowed, err: codedError{
		code: "method_not_allowed", message: "method is not allowed",
	}})
}

func (s *Server) mountRoutes(router chi.Router) {
	router.Get("/healthz", s.healthz)
	router.Get("/readyz", s.readyz)
	router.Route("/api", s.mountManagementRoutes)
	router.Route("/v1", s.mountDeveloperRoutes)
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var committed bool
		wrapped := httpsnoop.Wrap(w, httpsnoop.Hooks{
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(p []byte) (int, error) {
					committed = true
					return next(p)
				}
			},
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					committed = true
					next(code)
				}
			},
			Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
				return func() {
					committed = true
					next()
				}
			},
			ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(src io.Reader) (int64, error) {
					committed = true
					return next(src)
				}
			},
		})
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("Control Plane handler panic", "panic", recovered, "stack", string(debug.Stack()))
				if committed {
					panic(recovered)
				}
				writeError(wrapped, errors.New("internal server error"))
			}
		}()
		next.ServeHTTP(wrapped, r)
	})
}

func (s *Server) mountManagementRoutes(r chi.Router) {
	r.Use(limitAPIRequestBody)
	r.With(limitRequestBody(tokenRequestBodyLimit)).
		Post("/token-callbacks/{tokenID}/{callbackSecret}", s.completeTokenWithCallback)
	r.With(limitRequestBody(tokenRequestBodyLimit)).
		Post("/public/tokens/{tokenID}/complete", s.completeTokenWithBearer)
	r.Options("/public/tokens/{tokenID}/complete", s.completeTokenBearerPreflight)
	s.mountAuthRoutes(r)
	s.mountSessionRoutes(r)
	s.mountCapacityRoutes(r)
	s.mountWorkerRoutes(r)
}

func limitAPIRequestBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := apiRequestBodyLimit
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/deployments") {
			limit = deploymentRequestBodyLimit
		}
		limitRequestBody(limit)(next).ServeHTTP(w, r)
	})
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
		r.Get("/regions", s.listRegions)
		r.Post("/organizations", s.createOrganization)
		r.Get("/auth/device/status", s.deviceStatus)
		r.Post("/auth/device/approve", s.approveDeviceCode)
		r.Post("/auth/device/deny", s.denyDeviceCode)
	})
}

func (s *Server) mountSessionRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionPermission(auth.PermissionAPIKeysManage, next)
		})
		r.Get("/projects/{projectID}/environments/{environmentID}/api-keys", s.listAPIKeys)
		r.Post("/projects/{projectID}/environments/{environmentID}/api-keys", s.issueAPIKey)
		r.Delete("/projects/{projectID}/environments/{environmentID}/api-keys/{id}", s.revokeAPIKey)
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
		r.Post("/projects/{projectID}/environments/{environmentID}/deployments", s.createDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments", s.listDeployments)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/current", s.getCurrentDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}", s.getDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/events", s.getDeploymentEvents)
		r.Post("/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/promote", s.promoteDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/schedules", s.listSchedules)
		r.Get("/projects/{projectID}/environments/{environmentID}/schedules/{scheduleID}", s.getSchedule)
		r.Get("/projects/{projectID}/environments/{environmentID}/tokens", s.listTokens)
		r.With(limitRequestBody(tokenRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/tokens", s.createToken)
		r.Get("/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}", s.getToken)
		r.With(limitRequestBody(tokenRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}/complete", s.completeToken)
		r.With(limitRequestBody(tokenRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}/cancel", s.cancelToken)
		r.Get("/projects/{projectID}/environments/{environmentID}/secrets", s.listSecrets)
		r.With(limitRequestBody(secretRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/secrets", s.createSecret)
		r.Get("/projects/{projectID}/environments/{environmentID}/secrets/{secretID}", s.getSecretByID)
		r.With(limitRequestBody(secretRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/secrets/{secretID}/rotate", s.rotateSecretByID)
		r.With(limitRequestBody(secretRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/secrets/{secretID}/revoke", s.revokeSecretByID)
		r.With(limitRequestBody(workspaceCreateBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/sandboxes/{sandboxID}/workspaces", s.createWorkspaceHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces", s.listWorkspacesHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files", s.listWorkspaceFilesHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files/content", s.readWorkspaceFileHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files/stat", s.statWorkspaceFileHTTP)
		r.With(limitRequestBody(workspaceExecBodyMaxBytes)).
			Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/exec", s.executeWorkspaceHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}", s.getWorkspaceHTTP)
		r.Delete("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}", s.deleteWorkspaceHTTP)
		r.With(limitRequestBody(taskStartBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/tasks/{taskDeclaredID}/start", s.startTaskHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/tasks", s.listTasks)
		r.Get("/projects/{projectID}/environments/{environmentID}/tasks/{taskID}", s.getTask)
		r.Get("/projects/{projectID}/environments/{environmentID}/actors", s.listActors)
		r.Get("/projects/{projectID}/environments/{environmentID}/actors/{actorID}", s.getActor)
		r.Get("/projects/{projectID}/environments/{environmentID}/sandboxes", s.listSandboxes)
		r.Get("/projects/{projectID}/environments/{environmentID}/sandboxes/{sandboxID}", s.getSandbox)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs", s.listRunSnapshotsHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{runID}", s.getRunSnapshotHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{runID}/logs", s.listRunLogsHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{runID}/events", s.listRunEventsHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/runs/{runID}/cancel", s.cancelRunHTTP)
		r.With(limitSessionInputBody).
			Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/inputs", s.sendSessionInput)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionWithErrorWriter(next, writeActorStartAuthError)
		})
		r.With(limitActorStartBody).
			Post("/projects/{projectID}/environments/{environmentID}/actors/{actorDeclaredID}/start", s.startActorHTTP)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionWithErrorWriter(next, writeSessionCloseAuthError)
		})
		r.With(limitSessionCloseBody).
			Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/close", s.closeSessionHTTP)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionWithErrorWriter(next, writeSessionReadAuthError)
		})
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions", s.listSessionsHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}", s.getSessionHTTP)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionWithErrorWriter(next, writeSessionOutputReadAuthError)
		})
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/outputs", s.readSessionOutputHTTP)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionPermission(auth.PermissionProjectsManage, next)
		})
		r.Post("/projects", s.createProject)
		r.Patch("/projects/{projectID}", s.updateProject)
		r.Delete("/projects/{projectID}", s.deleteProject)
		r.Post("/projects/{projectID}/environments", s.createEnvironment)
		r.Patch("/projects/{projectID}/environments/{environmentID}", s.updateEnvironment)
		r.Delete("/projects/{projectID}/environments/{environmentID}", s.deleteEnvironment)
	})
}

func (s *Server) mountDeveloperRoutes(r chi.Router) {
	r.Use(limitAPIRequestBody)
	r.Group(func(r chi.Router) {
		r.Use(s.requireAPIKey)
		r.Get("/deployments", s.listDeployments)
		r.Get("/deployments/current", s.getCurrentDeployment)
		r.Get("/deployments/{deploymentID}", s.getDeployment)
		r.Get("/deployments/{deploymentID}/events", s.getDeploymentEvents)
		r.Post("/deployments", s.createDeployment)
		r.Post("/deployments/{deploymentID}/promote", s.promoteDeployment)
		r.Get("/schedules", s.listSchedules)
		r.Get("/schedules/{scheduleID}", s.getSchedule)
		r.Get("/tokens", s.listTokens)
		r.With(limitRequestBody(tokenRequestBodyLimit)).Post("/tokens", s.createToken)
		r.Get("/tokens/{tokenID}", s.getToken)
		r.With(limitRequestBody(tokenRequestBodyLimit)).Post("/tokens/{tokenID}/complete", s.completeToken)
		r.With(limitRequestBody(tokenRequestBodyLimit)).Post("/tokens/{tokenID}/cancel", s.cancelToken)
		r.With(limitRequestBody(taskStartBodyLimit)).Post("/tasks/{taskDeclaredID}/start", s.startTaskHTTP)
		r.Get("/tasks", s.listTasks)
		r.Get("/tasks/{taskID}", s.getTask)
		r.Get("/actors", s.listActors)
		r.Get("/actors/{actorID}", s.getActor)
		r.Get("/sandboxes", s.listSandboxes)
		r.Get("/sandboxes/{sandboxID}", s.getSandbox)
		r.Get("/runs", s.listRunSnapshotsHTTP)
		r.Get("/runs/{runID}", s.getRunSnapshotHTTP)
		r.Get("/runs/{runID}/logs", s.listRunLogsHTTP)
		r.Get("/runs/{runID}/events", s.listRunEventsHTTP)
		r.Post("/runs/{runID}/cancel", s.cancelRunHTTP)

		r.Get("/secrets", s.listSecrets)
		r.With(limitRequestBody(secretRequestBodyLimit)).Post("/secrets", s.createSecret)
		r.Get("/secrets/{secretID}", s.getSecretByID)
		r.With(limitRequestBody(secretRequestBodyLimit)).Post("/secrets/{secretID}/rotate", s.rotateSecretByID)
		r.With(limitRequestBody(secretRequestBodyLimit)).Post("/secrets/{secretID}/revoke", s.revokeSecretByID)
		r.With(limitRequestBody(workspaceCreateBodyLimit)).Post("/sandboxes/{sandboxID}/workspaces", s.createWorkspaceHTTP)
		r.Get("/workspaces", s.listWorkspacesHTTP)
		r.Get("/workspaces/{workspaceID}/files", s.listWorkspaceFilesHTTP)
		r.Get("/workspaces/{workspaceID}/files/content", s.readWorkspaceFileHTTP)
		r.Get("/workspaces/{workspaceID}/files/stat", s.statWorkspaceFileHTTP)
		r.With(limitRequestBody(workspaceExecBodyMaxBytes)).Post("/workspaces/{workspaceID}/exec", s.executeWorkspaceHTTP)
		r.Get("/workspaces/{workspaceID}", s.getWorkspaceHTTP)
		r.Delete("/workspaces/{workspaceID}", s.deleteWorkspaceHTTP)
		r.With(limitSessionInputBody).Post("/sessions/{sessionID}/inputs", s.sendSessionInput)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireAPIKeyWithErrorWriter(next, writeActorStartAuthError)
		})
		r.With(limitActorStartBody).Post("/actors/{actorDeclaredID}/start", s.startActorHTTP)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireAPIKeyWithErrorWriter(next, writeSessionCloseAuthError)
		})
		r.With(limitSessionCloseBody).Post("/sessions/{sessionID}/close", s.closeSessionHTTP)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireAPIKeyWithErrorWriter(next, writeSessionReadAuthError)
		})
		r.Get("/sessions", s.listSessionsHTTP)
		r.Get("/sessions/{sessionID}", s.getSessionHTTP)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireAPIKeyWithErrorWriter(next, writeSessionOutputReadAuthError)
		})
		r.Get("/sessions/{sessionID}/outputs", s.readSessionOutputHTTP)
	})
}

func (s *Server) mountWorkerRoutes(r chi.Router) {
	r.Route("/worker/v0", func(r chi.Router) {
		r.Post("/enrollment/challenge", s.workerEnrollmentChallenge)
		r.Post("/enrollment", s.workerEnroll)
		r.Post("/instance/token", s.workerAuthToken)
		r.With(s.requireRecoveringWorker).Post("/instance/recover", s.workerStartupRecovery)
		r.With(s.requireRegisteringWorker).Post("/instance/activate", s.workerActivate)
		r.With(s.requireWorker).Get("/instance", s.workerStatus)
		r.With(s.requireWorkerDrainCompletion).Post("/instance/drain/complete", s.workerCompleteDrain)
		r.With(s.requireWorkerFence).Post("/instance/fence", s.workerFence)
		r.Group(func(r chi.Router) {
			r.Use(s.requireWorker)
			r.Post("/instance/observations", s.workerObserve)
			r.Post("/instance/drain", s.workerDrain)
			r.Group(func(r chi.Router) {
				r.Use(func(next http.Handler) http.Handler { return requireWorkerRole(auth.WorkerRoleBuild, next) })
				r.With(func(next http.Handler) http.Handler { return requireActiveWorkerRole(auth.WorkerRoleBuild, next) }).Post("/build/platform-acquisitions/next", s.workerNextPlatformAcquisition)
				r.Post("/build/platform-acquisitions/complete", s.workerCompletePlatformAcquisition)
				r.Post("/build/platform-acquisitions/fail", s.workerFailPlatformAcquisition)
				r.With(func(next http.Handler) http.Handler { return requireActiveWorkerRole(auth.WorkerRoleBuild, next) }).Post("/build/deployments/lease", s.workerLeaseDeploymentBuild)
				r.Post("/build/deployments/start", s.workerStartDeploymentBuild)
				r.Post("/build/deployments/renew", s.workerRenewDeploymentBuild)
				r.Post("/build/deployments/reject", s.workerRejectDeploymentBuild)
				r.Post("/build/deployments/delivery-failed", s.workerDeploymentBuildDeliveryFailed)
				r.Post("/build/deployments/workspace-images/admit", s.workerAdmitWorkspaceImage)
				r.Post("/build/deployments/workspace-images/credentials", s.workerFetchWorkspaceImageCredentials)
				r.Post("/build/deployments/workspace-images/complete", s.workerCompleteWorkspaceImage)
				r.Post("/build/deployments/complete", s.workerCompleteDeploymentBuild)
			})
			r.Group(func(r chi.Router) {
				r.Use(func(next http.Handler) http.Handler { return requireWorkerRole(auth.WorkerRoleRun, next) })
				r.Post("/run/runtime-instances/reconcile", s.workerNextRuntimeReconcileTarget)
				r.Post("/run/runtime-instances/ready", s.workerMarkRuntimeInstanceReady)
				r.Post("/run/runtime-instances/closed", s.workerMarkRuntimeInstanceClosed)
				r.Post("/run/runtime-instances/failed", s.workerMarkRuntimeInstanceFailed)
				r.Post("/run/runtime-substrates/register", s.workerRegisterRuntimeSubstrate)
				r.Post("/run/workspace-mounts/claim", s.workerClaimWorkspaceMount)
				r.Post("/run/workspace-mounts/renew", s.workerRenewWorkspaceMount)
				r.Post("/run/workspace-mounts/mounted", s.workerMarkWorkspaceMountMounted)
				r.Post("/run/workspace-mounts/capture", s.workerCaptureWorkspaceMount)
				r.Post("/run/workspace-mounts/stop", s.workerStopWorkspaceMount)
				r.Post("/run/workspace-mounts/fail", s.workerFailWorkspaceMount)
				r.Post("/run/workspace-execs/claim", s.workerClaimWorkspaceExec)
				r.With(limitRequestBody(workspaceExecResultLimit)).
					Post("/run/workspace-execs/complete", s.workerCompleteWorkspaceExec)
				r.Post("/run/leases/discover", s.workerDiscoverRunLeases)
				r.Post("/run/leases/claim", s.workerClaimRunLease)
				r.Post("/run/leases/start", s.workerStart)
				r.Post("/run/leases/resume-release", s.workerAcknowledgeRunResumeRelease)
				r.Post("/run/leases/entrypoint", s.workerEnterRunEntrypoint)
				r.Post("/run/leases/renew", s.workerRenewRunLease)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/waits/create", s.workerCreateRunWait)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/waits/poll", s.workerPollRunWait)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/waits/resume-ack", s.workerAcknowledgeRunWaitResume)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/checkpoints/ready", s.workerMarkCheckpointReady)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/checkpoints/failed", s.workerMarkCheckpointFailed)
				r.Post("/run/finalization/begin", s.workerBeginRunFinalization)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/sessions/turns/commit", s.workerCommitActorTurn)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/sessions/outputs/append", s.workerAppendActorOutput)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/sessions/inputs/send", s.workerSendActorInput)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/actors/start", s.workerStartActor)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/sessions/retrieve", s.workerGetSessionStatus)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/sessions/close", s.workerCloseSession)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/sessions/outputs/read-page", s.workerReadSessionOutputPage)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/workspaces/create", s.workerCreateWorkspace)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/workspaces/retrieve", s.workerRetrieveWorkspace)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/workspaces/files/read", s.workerReadWorkspaceFile)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/workspaces/files/stat", s.workerStatWorkspaceFile)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/workspaces/files/list", s.workerListWorkspaceFiles)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/workspaces/exec", s.workerExecuteWorkspace)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/workspaces/exec/poll", s.workerPollWorkspaceExec)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/workspaces/delete", s.workerDeleteWorkspace)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/tasks/invoke", s.workerInvokeChildTask)
				r.With(limitRequestBody(tokenRequestBodyLimit)).Post("/run/tokens/create", s.workerCreateToken)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/tasks/complete", s.workerCompleteTask)
				r.With(limitRequestBody(taskCompletionBodyLimit)).Post("/run/sessions/complete", s.workerCompleteActor)
				r.With(limitRequestBody(workerLogRequestBodyLimit)).Post("/run/logs/append", s.workerAppendRunLogs)
				r.With(limitRequestBody(workerLogRequestBodyLimit)).Post("/run/structured-logs/append", s.workerAppendStructuredLog)
				r.With(limitRequestBody(workerLogRequestBodyLimit)).Post("/run/metadata/update", s.workerUpdateRunMetadata)
			})
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
	var databaseReady int
	if err := s.readinessDB.QueryRow(ctx, `SELECT 1`).Scan(&databaseReady); err != nil {
		s.writeReadinessUnavailable(w, fmt.Errorf("regional control plane database is not ready: %w", err))
		return
	}
	if databaseReady != 1 {
		s.writeReadinessUnavailable(w, errors.New("regional control plane database is not ready"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) writeReadinessUnavailable(w http.ResponseWriter, err error) {
	s.log.Warn("Control Plane readiness check failed", "error", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
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

func decodeOptionalJSON(r io.Reader, out any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(out)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("request body must contain a single JSON value")
	}
	return nil
}

func optionalLimitQuery(r *http.Request, defaultLimit int32) (int32, error) {
	limit := defaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 || parsed > int64(maxPageSize) {
			return 0, fmt.Errorf("limit must be an integer between 1 and %d", maxPageSize)
		}
		limit = int32(parsed)
	}
	return limit, nil
}

func limitRequestBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				writeError(w, tooLarge(errors.New("request body is too large")))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

func limitSessionInputBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, sessionInputBodyLimit)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) userAuthConfigured() error {
	if s.db == nil {
		return errors.New("run storage is not configured")
	}
	if !s.authKeys.Valid() {
		return errors.New("user authentication is not configured")
	}
	if s.publicURL == nil {
		return errors.New("public URL is not configured")
	}
	return nil
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

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := ids.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s must be a canonical UUIDv7", name)
	}
	return id, nil
}
