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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	readinessTimeout                      = 2 * time.Second
	preparedRuntimeSupplyReconcileTimeout = 2 * time.Second
	apiRequestBodyLimit                   = int64(128 << 20)
	workerLogRequestBodyLimit             = int64(256 << 10)
	maxControlPageSize                    = int32(500)
)

type SecretManager interface {
	Put(ctx context.Context, orgID uuid.UUID, name string, value []byte) (db.Secret, error)
	PutScoped(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, name string, value []byte) (db.Secret, error)
	CheckNames(ctx context.Context, orgID uuid.UUID, names []string) error
	CheckScopedNames(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, names []string) error
	ResolveNames(ctx context.Context, orgID uuid.UUID, names []string) (api.ResolvedSecrets, error)
	ResolveScopedNames(ctx context.Context, orgID uuid.UUID, projectID uuid.UUID, environmentID uuid.UUID, names []string) (api.ResolvedSecrets, error)
}

type Server struct {
	log                   *slog.Logger
	deploymentMode        string
	db                    db.Querier
	tx                    TxBeginner
	readinessDB           db.DBTX
	auth                  auth.Authenticator
	cas                   cas.Store
	secrets               SecretManager
	runEnqueuer           RunEnqueuer
	preparedRuntimeSupply PreparedRuntimeSupplyReconciler
	dispatchQueue         dispatch.Queue
	scheduleEngine        ScheduleRegistrar
	eventStream           *EventStream
	workspaceStreams      *WorkspaceStreamNotifier
	workerCommandStream   *WorkerCommandStream
	workerLeaseScanSeed   atomic.Uint64
	workerTokenSecret     []byte
	workerTokenTTL        time.Duration
	workerRegisterToken   string
	setupToken            string
	authSecret            []byte
	publicURL             *url.URL
	authProvider          AuthProvider
	mailer                email.Sender
	magicLinkDebugURLs    bool
	sessionTTL            time.Duration
	magicLinkTTL          time.Duration
	deviceCodeTTL         time.Duration
	devicePollEvery       time.Duration
}

type apiVersionContextKey struct{}
type requestVersionMetadataContextKey struct{}

type requestVersionMetadata struct {
	APIVersion string
	SDKVersion string
	CLIVersion string
}

const (
	deploymentModeSelfHosted   = "self-hosted"
	deploymentModeManagedCloud = "managed-cloud"
)

type RunEnqueuer interface {
	EnqueueRun(context.Context, pgtype.UUID, pgtype.UUID) (dispatch.EnqueueResult, error)
}

type PreparedRuntimeSupplyReconciler interface {
	Reconcile(context.Context) error
	ReconcileDeploymentSandbox(context.Context, pgtype.UUID) error
}

type ScheduleRegistrar interface {
	RegisterNext(context.Context, schedule.Instance) error
	DeleteInstance(context.Context, pgtype.UUID) error
}

type TxBeginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type dbTXBeginner interface {
	db.DBTX
	TxBeginner
}

type ServerConfig struct {
	Log            *slog.Logger
	DeploymentMode string

	DB          db.Querier
	TX          TxBeginner
	ReadinessDB db.DBTX

	Auth                  auth.Authenticator
	CAS                   cas.Store
	Secrets               SecretManager
	RunEnqueuer           RunEnqueuer
	PreparedRuntimeSupply PreparedRuntimeSupplyReconciler
	DispatchQueue         dispatch.Queue
	ScheduleEngine        ScheduleRegistrar
	EventStream           *EventStream
	WorkspaceStreams      *WorkspaceStreamNotifier
	WorkerCommands        *WorkerCommandStream
	Mailer                email.Sender
	AuthProvider          AuthProvider

	WorkerTokenSecret   []byte
	WorkerTokenTTL      time.Duration
	WorkerRegisterToken string
	SetupToken          string
	AuthSecret          []byte
	PublicURL           *url.URL

	MagicLinkDebugURLs bool
	SessionTTL         time.Duration
	MagicLinkTTL       time.Duration
	DeviceCodeTTL      time.Duration
	DevicePollEvery    time.Duration

	BackgroundContext context.Context
}

func NewServer(cfg ServerConfig) (http.Handler, error) {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.DB == nil {
		return nil, errors.New("control database is required")
	}
	if cfg.Auth == nil {
		return nil, errors.New("control authenticator is required")
	}
	deploymentMode := strings.TrimSpace(cfg.DeploymentMode)
	if deploymentMode == "" {
		deploymentMode = deploymentModeSelfHosted
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
	server := &Server{
		log:                   log,
		deploymentMode:        deploymentMode,
		db:                    cfg.DB,
		tx:                    cfg.TX,
		readinessDB:           cfg.ReadinessDB,
		auth:                  cfg.Auth,
		cas:                   cfg.CAS,
		secrets:               cfg.Secrets,
		runEnqueuer:           cfg.RunEnqueuer,
		preparedRuntimeSupply: cfg.PreparedRuntimeSupply,
		dispatchQueue:         cfg.DispatchQueue,
		scheduleEngine:        cfg.ScheduleEngine,
		eventStream:           cfg.EventStream,
		workspaceStreams:      cfg.WorkspaceStreams,
		workerCommandStream:   cfg.WorkerCommands,
		workerTokenSecret:     cfg.WorkerTokenSecret,
		workerTokenTTL:        workerTokenTTL,
		workerRegisterToken:   strings.TrimSpace(cfg.WorkerRegisterToken),
		setupToken:            strings.TrimSpace(cfg.SetupToken),
		authSecret:            cfg.AuthSecret,
		publicURL:             cfg.PublicURL,
		authProvider:          cfg.AuthProvider,
		mailer:                mailer,
		magicLinkDebugURLs:    cfg.MagicLinkDebugURLs,
		sessionTTL:            cfg.SessionTTL,
		magicLinkTTL:          cfg.MagicLinkTTL,
		deviceCodeTTL:         cfg.DeviceCodeTTL,
		devicePollEvery:       cfg.DevicePollEvery,
	}
	if cfg.BackgroundContext != nil {
		go server.RunSessionRunRequestReconciler(cfg.BackgroundContext)
	}
	router := chi.NewRouter()
	router.Use(server.recoverPanics)
	router.Use(otelhttp.NewMiddleware("helmr-control"))
	router.Get("/healthz", server.healthz)
	router.Get("/readyz", server.readyz)
	router.Route("/api", server.mountAPIRoutes)
	router.NotFound(server.notFound)
	return router, nil
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
				s.log.Error("control handler panic", "panic", recovered, "stack", string(debug.Stack()))
				if committed {
					panic(recovered)
				}
				writeError(wrapped, errors.New("internal server error"))
			}
		}()
		next.ServeHTTP(wrapped, r)
	})
}

func (s *Server) mountAPIRoutes(r chi.Router) {
	r.Use(limitRequestBody(apiRequestBodyLimit))
	r.Use(s.requireCurrentAPIVersion)
	r.Options("/v1/tokens/{tokenID}/complete", s.completeTokenPublicAccessTokenPreflight)
	r.Post("/v1/tokens/{tokenID}/complete", s.completeTokenWithPublicAccessToken)
	r.Post("/v1/tokens/{tokenID}/callback/{callbackSecret}", s.completeTokenWithCallbackSecret)
	r.Options("/v1/sessions/by-external-id/inputs/{stream}", s.publicSessionInputStreamPreflight)
	r.Post("/v1/sessions/by-external-id/inputs/{stream}", s.appendSessionInputStreamWithPublicAccessToken)
	r.Options("/v1/sessions/by-external-id/outputs/{stream}/read", s.publicSessionOutputStreamReadPreflight)
	r.Get("/v1/sessions/by-external-id/outputs/{stream}/read", s.readSessionOutputStreamWithPublicAccessToken)
	r.Options("/v1/sessions/{sessionID}/inputs/{stream}", s.publicSessionInputStreamPreflight)
	r.Post("/v1/sessions/{sessionID}/inputs/{stream}", s.appendSessionInputStreamWithPublicAccessToken)
	r.Options("/v1/sessions/{sessionID}/outputs/{stream}/read", s.publicSessionOutputStreamReadPreflight)
	r.Get("/v1/sessions/{sessionID}/outputs/{stream}/read", s.readSessionOutputStreamWithPublicAccessToken)
	s.mountAuthRoutes(r)
	s.mountOwnerRoutes(r)
	s.mountRunRoutes(r)
	s.mountScheduleRoutes(r)
	s.mountWorkerRoutes(r)
}

func (s *Server) requireCurrentAPIVersion(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.APIVersionHeader, api.CurrentAPIVersion)
		requested := strings.TrimSpace(r.Header.Get(api.APIVersionHeader))
		if requested != "" && requested != api.CurrentAPIVersion {
			writeError(w, badRequest(fmt.Errorf("unsupported %s %q; current version is %s", api.APIVersionHeader, requested, api.CurrentAPIVersion)))
			return
		}
		ctx := context.WithValue(r.Context(), apiVersionContextKey{}, api.CurrentAPIVersion)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestAPIVersion(r *http.Request) string {
	version, _ := r.Context().Value(apiVersionContextKey{}).(string)
	if strings.TrimSpace(version) == "" {
		return api.CurrentAPIVersion
	}
	return version
}

func contextWithRequestVersionMetadata(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, requestVersionMetadataContextKey{}, requestVersionMetadata{
		APIVersion: requestAPIVersion(r),
		SDKVersion: strings.TrimSpace(r.Header.Get(api.SDKVersionHeader)),
		CLIVersion: firstNonEmptyString(r.Header.Get(api.CLIVersionHeader), r.Header.Get(api.ClientVersionHeader)),
	})
}

func requestVersionMetadataFromContext(ctx context.Context) requestVersionMetadata {
	metadata, _ := ctx.Value(requestVersionMetadataContextKey{}).(requestVersionMetadata)
	if strings.TrimSpace(metadata.APIVersion) == "" {
		metadata.APIVersion = api.CurrentAPIVersion
	}
	return metadata
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
		r.Post("/projects/{projectID}/environments/{environmentID}/deployments/{deployment}/promote", s.promoteDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/sandboxes", s.listSandboxes)
		r.Get("/projects/{projectID}/environments/{environmentID}/sandboxes/{sandboxID}", s.getSandbox)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs", s.listRuns)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/counts", s.countRuns)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{id}", s.getRun)
		r.Post("/projects/{projectID}/environments/{environmentID}/runs/{id}/cancel", s.cancelRun)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{id}/events", s.getRunEvents)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{id}/logs", s.getRunLogs)
		r.Post("/projects/{projectID}/environments/{environmentID}/tokens", s.createToken)
		r.Get("/projects/{projectID}/environments/{environmentID}/tokens", s.listTokens)
		r.Get("/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}", s.getToken)
		r.Post("/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}/complete", s.completeToken)
		r.Post("/projects/{projectID}/environments/{environmentID}/tokens/{tokenID}/cancel", s.cancelToken)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces", s.createWorkspace)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces", s.listWorkspaces)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}", s.getWorkspace)
		r.Patch("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}", s.patchWorkspace)
		r.Delete("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}", s.deleteWorkspace)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/materialize", s.requestWorkspaceMount)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/connect", s.connectWorkspace)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/stop", s.stopWorkspace)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files", s.listWorkspaceFiles)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files/content", s.readWorkspaceFile)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/files/stat", s.statWorkspaceFile)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/versions", s.listWorkspaceVersions)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/versions/{versionID}", s.getWorkspaceVersion)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs", s.createWorkspaceExec)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs", s.listWorkspaceExecs)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}", s.getWorkspaceExec)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}/stdin", s.writeWorkspaceExecStdin)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}/stdin/close", s.closeWorkspaceExecStdin)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}/stdout", s.listWorkspaceExecStdout)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/execs/{execID}/stderr", s.listWorkspaceExecStderr)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty", s.createWorkspacePty)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty", s.listWorkspacePtys)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}", s.getWorkspacePty)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}/input", s.writeWorkspacePtyInput)
		r.Get("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}/output", s.listWorkspacePtyOutput)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}/resize", s.resizeWorkspacePty)
		r.Post("/projects/{projectID}/environments/{environmentID}/workspaces/{workspaceID}/pty/{ptyID}/close", s.closeWorkspacePty)
		s.mountSessionRoutes(r, "/projects/{projectID}/environments/{environmentID}")
		r.Get("/projects/{projectID}/environments/{environmentID}/schedules", s.listSchedules)
		r.Post("/projects/{projectID}/environments/{environmentID}/schedules", s.createSchedule)
		r.Get("/projects/{projectID}/environments/{environmentID}/schedules/{id}", s.getSchedule)
		r.Put("/projects/{projectID}/environments/{environmentID}/schedules/{id}", s.updateSchedule)
		r.Post("/projects/{projectID}/environments/{environmentID}/schedules/{id}/activate", s.activateSchedule)
		r.Post("/projects/{projectID}/environments/{environmentID}/schedules/{id}/deactivate", s.deactivateSchedule)
		r.Delete("/projects/{projectID}/environments/{environmentID}/schedules/{id}", s.deleteSchedule)
		r.Get("/projects/{projectID}/environments/{environmentID}/secrets", s.listSecrets)
		r.Get("/projects/{projectID}/environments/{environmentID}/secrets/{name}", s.getSecret)
		r.Put("/projects/{projectID}/environments/{environmentID}/secrets/{name}", s.setSecret)
		r.Delete("/projects/{projectID}/environments/{environmentID}/secrets/{name}", s.deleteSecret)
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
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		r.Get("/deployments", s.listDeployments)
		r.Get("/deployments/current", s.getCurrentDeployment)
		r.Get("/deployments/{deploymentID}", s.getDeployment)
		r.Get("/deployments/{deploymentID}/events", s.getDeploymentEvents)
		r.Post("/deployments/{deployment}/promote", s.promoteDeployment)
		r.Post("/deployments", s.createDeployment)
	})
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		r.Get("/secrets", s.listSecrets)
		r.Get("/secrets/{name}", s.getSecret)
		r.Put("/secrets/{name}", s.setSecret)
		r.Delete("/secrets/{name}", s.deleteSecret)
	})
}

func (s *Server) mountRunRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		s.mountSessionRoutes(r, "")
		r.Get("/runs", s.listRuns)
		r.Get("/runs/counts", s.countRuns)
		r.Get("/runs/{id}", s.getRun)
		r.Post("/runs/{id}/cancel", s.cancelRun)
		r.Get("/runs/{id}/events", s.getRunEvents)
		r.Get("/runs/{id}/logs", s.getRunLogs)
		r.Post("/tokens", s.createToken)
		r.Get("/tokens", s.listTokens)
		r.Get("/tokens/{tokenID}", s.getToken)
		r.Post("/tokens/{tokenID}/complete", s.completeToken)
		r.Post("/tokens/{tokenID}/cancel", s.cancelToken)
		r.Post("/public-access-tokens", s.createPublicAccessToken)
		r.Post("/workspaces", s.createWorkspace)
		r.Get("/workspaces", s.listWorkspaces)
		r.Get("/workspaces/{workspaceID}", s.getWorkspace)
		r.Patch("/workspaces/{workspaceID}", s.patchWorkspace)
		r.Delete("/workspaces/{workspaceID}", s.deleteWorkspace)
		r.Post("/workspaces/{workspaceID}/materialize", s.requestWorkspaceMount)
		r.Post("/workspaces/{workspaceID}/connect", s.connectWorkspace)
		r.Post("/workspaces/{workspaceID}/stop", s.stopWorkspace)
		r.Get("/workspaces/{workspaceID}/files", s.listWorkspaceFiles)
		r.Get("/workspaces/{workspaceID}/files/content", s.readWorkspaceFile)
		r.Get("/workspaces/{workspaceID}/files/stat", s.statWorkspaceFile)
		r.Get("/workspaces/{workspaceID}/versions", s.listWorkspaceVersions)
		r.Get("/workspaces/{workspaceID}/versions/{versionID}", s.getWorkspaceVersion)
		r.Post("/workspaces/{workspaceID}/execs", s.createWorkspaceExec)
		r.Get("/workspaces/{workspaceID}/execs", s.listWorkspaceExecs)
		r.Get("/workspaces/{workspaceID}/execs/{execID}", s.getWorkspaceExec)
		r.Post("/workspaces/{workspaceID}/execs/{execID}/stdin", s.writeWorkspaceExecStdin)
		r.Post("/workspaces/{workspaceID}/execs/{execID}/stdin/close", s.closeWorkspaceExecStdin)
		r.Get("/workspaces/{workspaceID}/execs/{execID}/stdout", s.listWorkspaceExecStdout)
		r.Get("/workspaces/{workspaceID}/execs/{execID}/stderr", s.listWorkspaceExecStderr)
		r.Post("/workspaces/{workspaceID}/pty", s.createWorkspacePty)
		r.Get("/workspaces/{workspaceID}/pty", s.listWorkspacePtys)
		r.Get("/workspaces/{workspaceID}/pty/{ptyID}", s.getWorkspacePty)
		r.Post("/workspaces/{workspaceID}/pty/{ptyID}/input", s.writeWorkspacePtyInput)
		r.Get("/workspaces/{workspaceID}/pty/{ptyID}/output", s.listWorkspacePtyOutput)
		r.Post("/workspaces/{workspaceID}/pty/{ptyID}/resize", s.resizeWorkspacePty)
		r.Post("/workspaces/{workspaceID}/pty/{ptyID}/close", s.closeWorkspacePty)
		r.Get("/sandboxes", s.listSandboxes)
		r.Get("/sandboxes/{sandboxID}", s.getSandbox)
	})
}

func (s *Server) mountSessionRoutes(r chi.Router, prefix string) {
	r.Get(prefix+"/tasks", s.listTasks)
	r.Get(prefix+"/tasks/{taskID}", s.getTask)
	r.Post(prefix+"/sessions", s.startSession)
	r.Post(prefix+"/sessions/start-and-wait", s.startSessionAndWait)
	r.Get(prefix+"/sessions", s.listSessions)
	r.Get(prefix+"/sessions/by-external-id", s.getSession)
	r.Patch(prefix+"/sessions/by-external-id", s.patchSession)
	r.Post(prefix+"/sessions/by-external-id/close", s.closeSession)
	r.Post(prefix+"/sessions/by-external-id/cancel", s.cancelSession)
	r.Get(prefix+"/sessions/by-external-id/runs", s.listSessionRuns)
	r.Get(prefix+"/sessions/by-external-id/streams", s.listSessionStreams)
	r.Post(prefix+"/sessions/by-external-id/inputs/{stream}", s.appendSessionInputStream)
	r.Get(prefix+"/sessions/by-external-id/inputs/{stream}", s.listSessionInputStreamRecords)
	r.Post(prefix+"/sessions/by-external-id/outputs/{stream}", s.appendSessionOutputStream)
	r.Get(prefix+"/sessions/by-external-id/outputs/{stream}", s.listSessionOutputStreamRecords)
	r.Get(prefix+"/sessions/by-external-id/outputs/{stream}/read", s.readSessionOutputStreamRecord)
	r.Get(prefix+"/sessions/{sessionID}", s.getSession)
	r.Patch(prefix+"/sessions/{sessionID}", s.patchSession)
	r.Post(prefix+"/sessions/{sessionID}/close", s.closeSession)
	r.Post(prefix+"/sessions/{sessionID}/cancel", s.cancelSession)
	r.Get(prefix+"/sessions/{sessionID}/runs", s.listSessionRuns)
	r.Get(prefix+"/sessions/{sessionID}/streams", s.listSessionStreams)
	r.Post(prefix+"/sessions/{sessionID}/inputs/{stream}", s.appendSessionInputStream)
	r.Get(prefix+"/sessions/{sessionID}/inputs/{stream}", s.listSessionInputStreamRecords)
	r.Post(prefix+"/sessions/{sessionID}/outputs/{stream}", s.appendSessionOutputStream)
	r.Get(prefix+"/sessions/{sessionID}/outputs/{stream}", s.listSessionOutputStreamRecords)
	r.Get(prefix+"/sessions/{sessionID}/outputs/{stream}/read", s.readSessionOutputStreamRecord)
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
			r.Get("/commands", s.workerReadCommands)
			r.Post("/commands/accept", s.workerAcceptCommand)
			r.Post("/commands/ack", s.workerAcknowledgeCommand)
			r.Post("/runtime-instances/prepared-runtime", s.workerCreatePreparedRuntimeInstance)
			r.Post("/runtime-instances/prepared-runtime-warm", s.workerCreateRuntimePrepareInstance)
			r.Post("/runtime-instances/renew", s.workerRenewRuntimeInstance)
			r.Post("/runtime-instances/ready", s.workerMarkRuntimeInstanceReady)
			r.Post("/runtime-instances/closed", s.workerMarkRuntimeInstanceClosed)
			r.Post("/runtime-instances/failed", s.workerMarkRuntimeInstanceFailed)
			r.Post("/runtime-substrate-artifacts/register", s.workerRegisterRuntimeSubstrateArtifact)
			r.Post("/runtime-substrate-artifacts/lookup", s.workerLookupRuntimeSubstrateArtifact)
			r.Post("/deployments/lease", s.workerLeaseDeploymentBuild)
			r.Post("/deployments/complete", s.workerCompleteDeploymentBuild)
			r.Post("/leases/lease", s.workerLease)
			r.Post("/leases/start", s.workerStart)
			r.Post("/leases/renew", s.workerRenew)
			r.Post("/leases/release", s.workerRelease)
			r.Post("/leases/tokens", s.workerCreateToken)
			r.Post("/leases/run-waits", s.workerCreateRunWait)
			r.Post("/leases/run-waits/workspace-capture", s.workerCaptureRunWaitWorkspace)
			r.Post("/leases/streams/input/read", s.workerReadInputStream)
			r.Post("/leases/streams/output", s.workerAppendOutputStream)
			r.Post("/leases/metadata", s.workerUpdateRunMetadata)
			r.Post("/leases/checkpoints/claim", s.workerClaimRuntimeCheckpointWait)
			r.Post("/leases/checkpoints/ready", s.workerMarkCheckpointReady)
			r.Post("/leases/checkpoints/failed", s.workerMarkCheckpointFailed)
			r.Post("/leases/restores/ack", s.workerAcknowledgeRestore)
			r.With(limitRequestBody(workerLogRequestBodyLimit)).Post("/leases/logs", s.workerAppendLogs)
			r.Post("/leases/log-entries", s.workerRecordLogEntry)
			r.Post("/workspaces/mounts/claim", s.workerClaimWorkspaceMount)
			r.Post("/workspaces/mounts/renew", s.workerRenewWorkspaceMount)
			r.Post("/workspaces/mounts/mounted", s.workerMarkWorkspaceMountMounted)
			r.Post("/workspaces/mounts/capture", s.workerCaptureWorkspaceMount)
			r.Post("/workspaces/mounts/fail", s.workerFailWorkspaceMount)
			r.Post("/workspaces/mounts/stop", s.workerStopWorkspaceMount)
			r.Post("/workspaces/mounts/operations/claim", s.workerClaimWorkspaceOperation)
			r.Post("/workspaces/mounts/operations/start", s.workerStartWorkspaceOperation)
			r.Post("/workspaces/mounts/operations/complete", s.workerCompleteWorkspaceOperation)
			r.Post("/workspaces/execs/started", s.workerMarkWorkspaceExecStarted)
			r.With(limitRequestBody(workerLogRequestBodyLimit)).Post("/workspaces/execs/output", s.workerAppendWorkspaceExecOutput)
			r.Post("/workspaces/execs/input", s.workerListWorkspaceExecInput)
			r.Post("/workspaces/execs/input-delivered", s.workerAdvanceWorkspaceExecInputDelivered)
			r.Post("/workspaces/execs/exited", s.workerMarkWorkspaceExecExited)
			r.Post("/workspaces/ptys/opened", s.workerMarkWorkspacePtyOpened)
			r.With(limitRequestBody(workerLogRequestBodyLimit)).Post("/workspaces/ptys/output", s.workerAppendWorkspacePtyOutput)
			r.Post("/workspaces/ptys/input", s.workerListWorkspacePtyInput)
			r.Post("/workspaces/ptys/input-delivered", s.workerAdvanceWorkspacePtyInputDelivered)
			r.Post("/workspaces/ptys/resize-applied", s.workerMarkWorkspacePtyResizeApplied)
			r.Post("/workspaces/ptys/closed", s.workerMarkWorkspacePtyClosed)
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

func optionalText(value string) pgtype.Text {
	value = strings.TrimSpace(value)
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

func optionalLimitQuery(r *http.Request, defaultLimit int32) (int32, error) {
	limit := defaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed <= 0 || parsed > int64(maxControlPageSize) {
			return 0, fmt.Errorf("limit must be an integer between 1 and %d", maxControlPageSize)
		}
		limit = int32(parsed)
	}
	return limit, nil
}

func optionalUUIDString(value pgtype.UUID) string {
	if !value.Valid {
		return ""
	}
	return uuid.UUID(value.Bytes).String()
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

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s must be a UUID", name)
	}
	return id, nil
}

func (s *Server) mountScheduleRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireActor)
		r.Get("/schedules", s.listSchedules)
		r.Post("/schedules", s.createSchedule)
		r.Get("/schedules/{id}", s.getSchedule)
		r.Put("/schedules/{id}", s.updateSchedule)
		r.Post("/schedules/{id}/activate", s.activateSchedule)
		r.Post("/schedules/{id}/deactivate", s.deactivateSchedule)
		r.Delete("/schedules/{id}", s.deleteSchedule)
	})
}
