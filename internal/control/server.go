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
	readinessTimeout          = 2 * time.Second
	apiRequestBodyLimit       = int64(128 << 20)
	workerLogRequestBodyLimit = int64(256 << 10)
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
	log                 *slog.Logger
	deploymentMode      string
	db                  db.Querier
	tx                  TxBeginner
	readinessDB         db.DBTX
	auth                auth.Authenticator
	cas                 cas.Store
	secrets             SecretManager
	runEnqueuer         RunEnqueuer
	dispatchQueue       dispatch.Queue
	scheduleEngine      ScheduleRegistrar
	eventStream         *EventStream
	workerLeaseScanSeed atomic.Uint64
	workerTokenSecret   []byte
	workerTokenTTL      time.Duration
	workerRegisterToken string
	setupToken          string
	authSecret          []byte
	publicURL           *url.URL
	authProvider        AuthProvider
	mailer              email.Sender
	magicLinkDebugURLs  bool
	sessionTTL          time.Duration
	magicLinkTTL        time.Duration
	deviceCodeTTL       time.Duration
	devicePollEvery     time.Duration
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

	Auth           auth.Authenticator
	CAS            cas.Store
	Secrets        SecretManager
	RunEnqueuer    RunEnqueuer
	DispatchQueue  dispatch.Queue
	ScheduleEngine ScheduleRegistrar
	EventStream    *EventStream
	Mailer         email.Sender
	AuthProvider   AuthProvider

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
		log:                 log,
		deploymentMode:      deploymentMode,
		db:                  cfg.DB,
		tx:                  cfg.TX,
		readinessDB:         cfg.ReadinessDB,
		auth:                cfg.Auth,
		cas:                 cfg.CAS,
		secrets:             cfg.Secrets,
		runEnqueuer:         cfg.RunEnqueuer,
		dispatchQueue:       cfg.DispatchQueue,
		scheduleEngine:      cfg.ScheduleEngine,
		eventStream:         cfg.EventStream,
		workerTokenSecret:   cfg.WorkerTokenSecret,
		workerTokenTTL:      workerTokenTTL,
		workerRegisterToken: strings.TrimSpace(cfg.WorkerRegisterToken),
		setupToken:          strings.TrimSpace(cfg.SetupToken),
		authSecret:          cfg.AuthSecret,
		publicURL:           cfg.PublicURL,
		authProvider:        cfg.AuthProvider,
		mailer:              mailer,
		magicLinkDebugURLs:  cfg.MagicLinkDebugURLs,
		sessionTTL:          cfg.SessionTTL,
		magicLinkTTL:        cfg.MagicLinkTTL,
		deviceCodeTTL:       cfg.DeviceCodeTTL,
		devicePollEvery:     cfg.DevicePollEvery,
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
	s.mountAuthRoutes(r)
	s.mountWaitpointTokenRoutes(r)
	s.mountOwnerRoutes(r)
	s.mountPublicRootTaskSessionChannelRoutes(r)
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
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/current", s.getCurrentDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}", s.getDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/events", s.getDeploymentEvents)
		r.Post("/projects/{projectID}/environments/{environmentID}/deployments/{deployment}/promote", s.promoteDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs", s.listRuns)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/counts", s.countRuns)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{id}", s.getRun)
		r.Post("/projects/{projectID}/environments/{environmentID}/runs/{id}/cancel", s.cancelRun)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{id}/waitpoints", s.listRunWaitpoints)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{id}/events", s.getRunEvents)
		r.Get("/projects/{projectID}/environments/{environmentID}/runs/{id}/logs", s.getRunLogs)
		s.mountTaskSessionRoutes(r, "/projects/{projectID}/environments/{environmentID}")
		r.Post("/projects/{projectID}/environments/{environmentID}/waitpoints/tokens", s.createWaitpointToken)
		r.Get("/projects/{projectID}/environments/{environmentID}/waitpoints/tokens", s.listWaitpointTokens)
		r.Get("/projects/{projectID}/environments/{environmentID}/waitpoints/tokens/{tokenID}", s.getWaitpointToken)
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
		s.mountTaskSessionRoutes(r, "")
		r.Get("/runs", s.listRuns)
		r.Get("/runs/counts", s.countRuns)
		r.Get("/runs/{id}", s.getRun)
		r.Post("/runs/{id}/cancel", s.cancelRun)
		r.Get("/runs/{id}/waitpoints", s.listRunWaitpoints)
		r.Get("/runs/{id}/events", s.getRunEvents)
		r.Get("/runs/{id}/logs", s.getRunLogs)
		r.Post("/waitpoints/tokens", s.createWaitpointToken)
		r.Get("/waitpoints/tokens", s.listWaitpointTokens)
		r.Get("/waitpoints/tokens/{tokenID}", s.getWaitpointToken)
		r.Post("/public-access-tokens", s.createPublicAccessToken)
	})
}

func (s *Server) mountTaskSessionRoutes(r chi.Router, prefix string) {
	r.Post(prefix+"/tasks/{taskID}/start", s.startTask)
	r.Post(prefix+"/tasks/{taskID}/start-and-wait", s.startTaskAndWait)
	r.Get(prefix+"/sessions", s.listTaskSessions)
	r.Get(prefix+"/sessions/{sessionID}", s.getTaskSession)
	r.Patch(prefix+"/sessions/{sessionID}", s.patchTaskSession)
	r.Post(prefix+"/sessions/{sessionID}/wait", s.waitTaskSession)
	r.Post(prefix+"/sessions/{sessionID}/close", s.closeTaskSession)
	r.Post(prefix+"/sessions/{sessionID}/cancel", s.cancelTaskSession)
	r.Get(prefix+"/sessions/{sessionID}/runs", s.listTaskSessionRuns)
	r.Get(prefix+"/sessions/{sessionID}/workspace", s.getTaskSessionWorkspace)
	r.Get(prefix+"/sessions/{sessionID}/channels", s.listTaskSessionChannels)
	if prefix == "" {
		return
	}
	r.Post(prefix+"/sessions/{sessionID}/channels/{channel}/inputs", s.appendTaskSessionChannelInput)
	r.Get(prefix+"/sessions/{sessionID}/channels/{channel}/inputs", s.listTaskSessionChannelInputs)
	r.Get(prefix+"/sessions/{sessionID}/channels/{channel}/outputs", s.listTaskSessionChannelOutputs)
	r.Get(prefix+"/sessions/{sessionID}/channels/{channel}/outputs/stream", s.streamTaskSessionChannelOutputs)
}

func (s *Server) mountPublicRootTaskSessionChannelRoutes(r chi.Router) {
	r.Options("/sessions/{sessionID}/channels/{channel}/inputs", s.optionsTaskSessionChannelRecords)
	r.Options("/sessions/{sessionID}/channels/{channel}/outputs", s.optionsTaskSessionChannelRecords)
	r.Options("/sessions/{sessionID}/channels/{channel}/outputs/stream", s.optionsTaskSessionChannelRecords)
	r.Post("/sessions/{sessionID}/channels/{channel}/inputs", s.appendTaskSessionChannelInputEntry)
	r.Get("/sessions/{sessionID}/channels/{channel}/inputs", s.listTaskSessionChannelInputsEntry)
	r.Get("/sessions/{sessionID}/channels/{channel}/outputs", s.listTaskSessionChannelOutputsEntry)
	r.Get("/sessions/{sessionID}/channels/{channel}/outputs/stream", s.streamTaskSessionChannelOutputsEntry)
}

func (s *Server) mountWaitpointTokenRoutes(r chi.Router) {
	r.Options("/waitpoints/tokens/{tokenID}/complete", s.optionsCompleteWaitpointToken)
	r.Post("/waitpoints/tokens/{tokenID}/complete", s.completeWaitpointToken)
	r.Post("/waitpoints/tokens/{tokenID}/callback/{callbackSecret}", s.callbackWaitpointToken)
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
			r.Post("/leases/lease", s.workerLease)
			r.Post("/leases/start", s.workerStart)
			r.Post("/leases/restores/ack", s.workerAcknowledgeRestore)
			r.Post("/leases/renew", s.workerRenew)
			r.Post("/leases/release", s.workerRelease)
			r.With(limitRequestBody(workerLogRequestBodyLimit)).Post("/leases/logs", s.workerAppendLogs)
			r.Post("/leases/log-entries", s.workerRecordLogEntry)
			r.Post("/leases/channels", s.workerWriteOutput)
			r.Post("/leases/metadata", s.workerUpdateRunMetadata)
			r.Post("/leases/waitpoints", s.workerCreateWaitpoint)
			r.Post("/leases/waitpoint-tokens", s.workerCreateWaitpointToken)
			r.Post("/leases/checkpoints/ready", s.workerCheckpointReady)
			r.Post("/leases/checkpoints/failed", s.workerCheckpointFailed)
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
