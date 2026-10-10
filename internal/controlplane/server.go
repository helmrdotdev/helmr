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
	"strings"
	"time"
	"uuid"

	"github.com/felixge/httpsnoop"
	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

const (
	readinessTimeout           = 2 * time.Second
	apiRequestBodyLimit        = int64(128 << 20)
	deploymentRequestBodyLimit = int64(642 << 20)
	workerLogRequestBodyLimit  = int64(256 << 10)
	taskCompletionBodyLimit    = int64(17 << 20)
	computerCommandResultLimit = int64(10 << 20)
	secretRequestBodyLimit     = int64(1 << 20)
	adminRequestBodyLimit      = int64(64 << 10)
	maxPageSize                = int32(500)
)

type SecretManager interface {
	Create(ctx context.Context, environmentID uuid.UUID, name string, value []byte, idempotencyKey string) (db.GetSecretSnapshotRow, error)
	Rotate(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID, value []byte, idempotencyKey string) (db.GetSecretSnapshotRow, error)
	Revoke(ctx context.Context, environmentID uuid.UUID, secretID uuid.UUID, idempotencyKey string) (db.GetSecretSnapshotRow, error)
}

type SubjectEventReader interface {
	ReadSubject(context.Context, uuid.UUID, string, uuid.UUID, int64, func(api.DiagnosticEvent) error, func() error) error
}

type Server struct {
	slackConfig                *SlackConfig
	diagnosticDB               db.TxDB
	diagnosticBounds           diagnostic.Bounds
	allocator                  *agent.Allocator
	environmentExecutionLimits org.ExecutionLimits
	allocationKeys             *agent.ComputerKeyBroker
	preparationKeys            *agent.PreparationKeyBroker
	preparationPublisher       *agent.PreparationPublisher
	savePublisher              *agent.SavePublisher
	log                        *slog.Logger
	deploymentMode             string
	db                         db.Querier
	tx                         db.TxDB
	readinessDB                db.DBTX
	auth                       auth.Authenticator
	cas                        cas.UploadStore
	bundleAdmission            bundle.Admission
	platformStore              cas.Reader
	secrets                    SecretManager
	secretDelivery             SecretDeliveryOpener
	secretProxy                *secret.Store
	computers                  computer.Creator
	computerFencingKey         disk.FencingKey
	eventStream                SubjectEventReader
	telemetryReader            telemetry.Reader
	hostAuth                   workergroup.HostAuthConfig
	workerEnrollmentGuard      *workerEnrollmentGuard
	capacityTokenHash          []byte
	setupToken                 string
	authKeys                   auth.Keys
	publicURL                  *url.URL
	authProvider               AuthProvider
	mailer                     email.Sender
	magicLinkDelivery          *MagicLinkDelivery
	magicLinkDebugURLs         bool
	identity                   identity.Config
	deploymentFinalizer        *deployment.Finalizer
}

const (
	deploymentModeSelfHosted   = "self-hosted"
	deploymentModeManagedCloud = "managed-cloud"
)

type ServerConfig struct {
	Slack                      *SlackConfig
	DiagnosticDB               db.TxDB
	DiagnosticBounds           diagnostic.Bounds
	Allocator                  *agent.Allocator
	EnvironmentExecutionLimits org.ExecutionLimits
	ComputerKeys               agent.DataKeyWrapper
	Log                        *slog.Logger
	DeploymentMode             string

	DB          db.Querier
	TX          db.TxDB
	ReadinessDB db.DBTX

	Auth               auth.Authenticator
	CAS                cas.UploadStore
	BundleAdmission    bundle.Admission
	PlatformStore      cas.Reader
	Secrets            SecretManager
	SecretDelivery     SecretDeliveryOpener
	SecretProxy        *secret.Store
	ComputerFencingKey disk.FencingKey
	EventStream        SubjectEventReader
	TelemetryReader    telemetry.Reader
	Mailer             email.Sender
	MagicLinkDelivery  *MagicLinkDelivery
	AuthProvider       AuthProvider

	WorkerHostCredentialSigningKey []byte
	WorkerHostCredentialTTL        time.Duration
	CapacityToken                  string
	SetupToken                     string
	AuthKey                        []byte
	PublicURL                      *url.URL

	MagicLinkDebugURLs bool
	AdminEmails        []string
	SessionTTL         time.Duration
	MagicLinkTTL       time.Duration
	DeviceCodeTTL      time.Duration
	DevicePollEvery    time.Duration
}

func NewServer(cfg ServerConfig) (http.Handler, error) {
	if cfg.Slack != nil && (len(cfg.Slack.ControlKey) != 32 || cfg.Slack.Credentials == nil || cfg.Slack.Client == nil || cfg.PublicURL == nil || len(cfg.AuthKey) != 32) {
		return nil, errors.New("complete Slack adapter configuration is required")
	}
	if err := cfg.EnvironmentExecutionLimits.Validate(); err != nil {
		return nil, fmt.Errorf("environment execution limits: %w", err)
	}
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
	if cfg.CAS == nil {
		return nil, errors.New("control plane CAS is required")
	}
	if cfg.PlatformStore == nil {
		return nil, errors.New("platform artifact store is required")
	}
	if err := cfg.BundleAdmission.Validate(); err != nil {
		return nil, err
	}
	deploymentMode := strings.TrimSpace(cfg.DeploymentMode)
	if deploymentMode == "" {
		deploymentMode = deploymentModeSelfHosted
	}
	if deploymentMode != deploymentModeSelfHosted && deploymentMode != deploymentModeManagedCloud {
		return nil, errors.New("deployment mode must be self-hosted or managed-cloud")
	}
	capacityTokenHash, err := hashCapacityToken(cfg.CapacityToken)
	if err != nil {
		return nil, err
	}
	if cfg.SecretDelivery == nil {
		return nil, errors.New("secret delivery opener is required")
	}
	if !cfg.ComputerFencingKey.Valid() {
		return nil, errors.New("computer fencing key is required")
	}
	authKeys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		return nil, err
	}
	hostAuth, err := workergroup.NewHostAuthConfig(authKeys.WorkerHost, cfg.WorkerHostCredentialSigningKey, cfg.WorkerHostCredentialTTL)
	if err != nil {
		return nil, err
	}
	if cfg.DiagnosticDB == nil {
		return nil, errors.New("diagnostic database pool is required")
	}
	if err := cfg.DiagnosticBounds.Validate(); err != nil || cfg.DiagnosticBounds.ChunkBytes > 16*1024*1024 {
		return nil, errors.New("control plane requires valid diagnostic bounds within the Session transport limit")
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
	if _, unconfigured := mailer.(email.Unconfigured); !unconfigured && cfg.MagicLinkDelivery == nil {
		return nil, errors.New("magic link delivery worker is required")
	}
	allocationKeys, err := agent.NewComputerKeyBroker(cfg.TX, cfg.ComputerKeys)
	if err != nil {
		return nil, err
	}
	preparationKeys, err := agent.NewPreparationKeyBroker(cfg.TX, cfg.ComputerKeys)
	if err != nil {
		return nil, err
	}
	preparationPublisher, err := agent.NewPreparationPublisher(cfg.TX, cfg.CAS)
	if err != nil {
		return nil, err
	}
	savePublisher, err := agent.NewSavePublisher(cfg.TX, cfg.CAS)
	if err != nil {
		return nil, err
	}
	if cfg.Allocator == nil {
		return nil, errors.New("allocation owner is required")
	}
	server := &Server{
		slackConfig:                cfg.Slack,
		allocator:                  cfg.Allocator,
		environmentExecutionLimits: cfg.EnvironmentExecutionLimits,
		allocationKeys:             allocationKeys,
		preparationKeys:            preparationKeys,
		preparationPublisher:       preparationPublisher,
		savePublisher:              savePublisher,
		log:                        log,
		deploymentMode:             deploymentMode,
		db:                         cfg.DB,
		tx:                         cfg.TX,
		readinessDB:                cfg.ReadinessDB,
		auth:                       cfg.Auth,
		cas:                        cfg.CAS,
		bundleAdmission:            cfg.BundleAdmission,
		platformStore:              cfg.PlatformStore,
		secrets:                    cfg.Secrets,
		secretDelivery:             cfg.SecretDelivery,
		secretProxy:                cfg.SecretProxy,
		computers:                  computer.NewCreator(cfg.SecretProxy),
		computerFencingKey:         cfg.ComputerFencingKey,
		eventStream:                cfg.EventStream,
		telemetryReader:            telemetryReader,
		diagnosticBounds:           cfg.DiagnosticBounds,
		diagnosticDB:               cfg.DiagnosticDB,
		hostAuth:                   hostAuth,
		workerEnrollmentGuard:      newWorkerEnrollmentGuard(),
		capacityTokenHash:          capacityTokenHash,
		setupToken:                 cfg.SetupToken,
		authKeys:                   authKeys,
		publicURL:                  cfg.PublicURL,
		authProvider:               cfg.AuthProvider,
		mailer:                     mailer,
		magicLinkDelivery:          cfg.MagicLinkDelivery,
		magicLinkDebugURLs:         cfg.MagicLinkDebugURLs,
		identity: identity.NewConfig(authKeys, identity.Lifetimes{
			Session:            cfg.SessionTTL,
			MagicLink:          cfg.MagicLinkTTL,
			DeviceCode:         cfg.DeviceCodeTTL,
			DevicePollInterval: cfg.DevicePollEvery,
		}, cfg.AdminEmails),
		deploymentFinalizer: deployment.NewFinalizer(cfg.CAS, cfg.PlatformStore, cfg.BundleAdmission, log),
	}
	router := chi.NewRouter()
	router.Use(server.recoverPanics)
	router.Use(otelhttp.NewMiddleware("helmr-controlplane"))
	router.Use(server.requestCorrelation)
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
	router.Post("/integrations/slack/apps/{registrationID}/events", s.slackAppEvents)
	router.Post("/integrations/slack/apps/{registrationID}/interactions", s.slackAppInteractions)
	router.Group(func(r chi.Router) {
		r.Use(limitAPIRequestBody)
		s.mountCapacityRoutes(r)
		s.mountWorkerRoutes(r)
	})
	router.Route("/admin/api/v1", s.mountAdminRoutes)
	router.Route("/v1", s.mountDeveloperRoutes)
}

func (s *Server) mountAdminRoutes(r chi.Router) {
	r.Use(limitRequestBody(adminRequestBodyLimit))
	r.Use(s.requireAdmin)
	r.Get("/regions", s.adminListRegions)
	r.Post("/regions", s.adminCreateRegion)
	r.Get("/regions/{regionID}", s.adminGetRegion)
	r.Patch("/regions/{regionID}", s.adminUpdateRegion)
	r.Get("/worker-groups", s.adminListWorkerGroups)
	r.Post("/worker-groups", s.adminCreateWorkerGroup)
	r.Get("/worker-groups/{groupID}", s.adminGetWorkerGroup)
	r.Patch("/worker-groups/{groupID}", s.adminUpdateWorkerGroup)
	r.Post("/worker-groups/{groupID}/pause", s.adminPauseWorkerGroup)
	r.Post("/worker-groups/{groupID}/activate", s.adminActivateWorkerGroup)
	r.Post("/worker-groups/{groupID}/drain", s.adminDrainWorkerGroup)
	r.Post("/worker-groups/{groupID}/disable", s.adminDisableWorkerGroup)
	r.Post("/worker-groups/{groupID}/token/rotate", s.adminRotateWorkerGroupToken)
	r.Get("/worker-groups/{groupID}/pools", s.adminListWorkerPools)
	r.Post("/worker-groups/{groupID}/pools", s.adminCreateWorkerPool)
	r.Post("/worker-groups/{groupID}/pools/{poolID}/primary", s.adminSwitchWorkerPoolPrimary)
	r.Post("/worker-groups/{groupID}/pools/{poolID}/drain", s.adminDrainWorkerPool)
	r.Post("/worker-groups/{groupID}/pools/{poolID}/disable", s.adminDisableWorkerPool)
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
				s.log.ErrorContext(
					r.Context(),
					"Control Plane handler panic",
					"request_id", wrapped.Header().Get(requestIDHeader),
					"panic", recovered,
					"stack", string(debug.Stack()),
				)
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
	s.mountAuthRoutes(r)
	s.mountSlackRoutes(r)
	s.mountSessionRoutes(r)
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
		r.Get("/projects/{projectRef}", s.getProject)
		r.Get("/projects/{projectID}/environments/{environmentID}", s.getEnvironment)
		r.With(limitRequestBody(bundle.MaxBytes)).
			Post("/projects/{projectID}/environments/{environmentID}/deployment-bundles/upload-plan", s.planDeploymentBundleUpload)
		r.Post("/projects/{projectID}/environments/{environmentID}/deployment-bundles/finalize", s.finalizeDeploymentBundle)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments", s.listDeployments)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/current", s.getCurrentDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}", s.getDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/events", s.getDeploymentEvents)
		r.Post("/projects/{projectID}/environments/{environmentID}/deployments/{deploymentID}/promote", s.promoteDeployment)
		r.Get("/projects/{projectID}/environments/{environmentID}/agents", s.listAgents)
		r.Get("/projects/{projectID}/environments/{environmentID}/agents/{agentName}", s.getAgent)
		r.Get("/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack", s.agentSlackPublication)
		r.Post("/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack", s.agentSlackPublication)
		r.Put("/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack/{publicationID}/credentials", s.agentSlackPublication)
		r.Post("/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack/{publicationID}/authorize", s.agentSlackPublication)
		r.Delete("/projects/{projectID}/environments/{environmentID}/agents/{agentName}/slack/{publicationID}", s.agentSlackPublication)
		r.Post("/projects/{projectID}/environments/{environmentID}/agents/{agentName}/start", s.startAgentHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions", s.listAgentSessionsHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}", s.getAgentSessionHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/slack-delivery", s.sessionSlackDelivery)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/slack-delivery/{postID}/{recovery:check|abandon}", s.sessionSlackDelivery)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/events", s.listAgentEventsHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns", s.listAgentTurnsHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}", s.getAgentTurnHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}/asks", s.listAgentAsksHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}/asks/{askID}", s.getAgentAskHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}/asks/{askID}/respond", s.respondAgentAskHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/enqueue", s.enqueueSessionHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/send", s.sendSessionHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/turns/{turnID}/messages", s.sendSessionHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/interrupt", s.interruptSessionHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/close", s.closeSessionHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/cancel", s.cancelSessionHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/sessions/{sessionID}/resume", s.resumeSessionHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/schedules", s.listSchedules)
		r.Get("/projects/{projectID}/environments/{environmentID}/schedules/{scheduleID}", s.getSchedule)
		r.Get("/projects/{projectID}/environments/{environmentID}/secrets", s.listSecrets)
		r.With(limitRequestBody(secretRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/secrets", s.createSecret)
		r.Get("/projects/{projectID}/environments/{environmentID}/secrets/{secretID}", s.getSecretByID)
		r.With(limitRequestBody(secretRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/secrets/{secretID}/rotate", s.rotateSecretByID)
		r.With(limitRequestBody(secretRequestBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/secrets/{secretID}/revoke", s.revokeSecretByID)
		r.With(limitRequestBody(computerCreateBodyLimit)).
			Post("/projects/{projectID}/environments/{environmentID}/computer-definitions/{computerDefinitionID}/computers", s.createComputerHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/computers", s.listComputersHTTP)
		r.With(limitRequestBody(computerCommandBodyMaxBytes)).
			Post("/projects/{projectID}/environments/{environmentID}/computers/{computerID}/exec", s.executeComputerHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/commands/{commandID}", s.getComputerCommandHTTP)
		r.Post("/projects/{projectID}/environments/{environmentID}/commands/{commandID}/cancel", s.cancelCommandHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/commands/{commandID}/logs", s.listCommandLogsHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/computers/{computerID}", s.getComputerHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/computers/{computerID}/members", s.listComputerMembersHTTP)
		r.Delete("/projects/{projectID}/environments/{environmentID}/computers/{computerID}", s.deleteComputerHTTP)
		r.Get("/projects/{projectID}/environments/{environmentID}/computer-definitions", s.listComputerDefinitions)
		r.Get("/projects/{projectID}/environments/{environmentID}/computer-definitions/{computerDefinitionID}", s.getComputerDefinition)
	})
	r.Group(func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return s.requireSessionPermission(auth.PermissionProjectsManage, next)
		})
		r.Post("/projects", s.createProject)
		r.Patch("/projects/{projectID}", s.updateProject)
		r.Post("/projects/{projectID}/environments", s.createEnvironment)
		r.Patch("/projects/{projectID}/environments/{environmentID}", s.updateEnvironment)
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
		r.With(limitRequestBody(bundle.MaxBytes)).Post("/deployment-bundles/upload-plan", s.planDeploymentBundleUpload)
		r.Post("/deployment-bundles/finalize", s.finalizeDeploymentBundle)
		r.Post("/deployments/{deploymentID}/promote", s.promoteDeployment)
		r.Get("/agents", s.listAgents)
		r.Get("/agents/{agentName}", s.getAgent)
		r.Post("/agents/{agentName}/start", s.startAgentHTTP)
		r.Get("/sessions", s.listAgentSessionsHTTP)
		r.Get("/sessions/{sessionID}", s.getAgentSessionHTTP)
		r.Get("/sessions/{sessionID}/events", s.listAgentEventsHTTP)
		r.Get("/sessions/{sessionID}/turns", s.listAgentTurnsHTTP)
		r.Get("/sessions/{sessionID}/turns/{turnID}", s.getAgentTurnHTTP)
		r.Get("/sessions/{sessionID}/turns/{turnID}/asks", s.listAgentAsksHTTP)
		r.Get("/sessions/{sessionID}/turns/{turnID}/asks/{askID}", s.getAgentAskHTTP)
		r.Post("/sessions/{sessionID}/turns/{turnID}/asks/{askID}/respond", s.respondAgentAskHTTP)
		r.Post("/sessions/{sessionID}/enqueue", s.enqueueSessionHTTP)
		r.Post("/sessions/{sessionID}/send", s.sendSessionHTTP)
		r.Post("/sessions/{sessionID}/turns/{turnID}/messages", s.sendSessionHTTP)
		r.Post("/sessions/{sessionID}/interrupt", s.interruptSessionHTTP)
		r.Post("/sessions/{sessionID}/close", s.closeSessionHTTP)
		r.Post("/sessions/{sessionID}/cancel", s.cancelSessionHTTP)
		r.Post("/sessions/{sessionID}/resume", s.resumeSessionHTTP)
		r.Get("/schedules", s.listSchedules)
		r.Get("/schedules/{scheduleID}", s.getSchedule)
		r.Get("/computer-definitions", s.listComputerDefinitions)
		r.Get("/computer-definitions/{computerDefinitionID}", s.getComputerDefinition)

		r.Get("/secrets", s.listSecrets)
		r.With(limitRequestBody(secretRequestBodyLimit)).Post("/secrets", s.createSecret)
		r.Get("/secrets/{secretID}", s.getSecretByID)
		r.With(limitRequestBody(secretRequestBodyLimit)).Post("/secrets/{secretID}/rotate", s.rotateSecretByID)
		r.With(limitRequestBody(secretRequestBodyLimit)).Post("/secrets/{secretID}/revoke", s.revokeSecretByID)
		r.With(limitRequestBody(computerCreateBodyLimit)).Post("/computer-definitions/{computerDefinitionID}/computers", s.createComputerHTTP)
		r.Get("/computers", s.listComputersHTTP)
		r.With(limitRequestBody(computerCommandBodyMaxBytes)).Post("/computers/{computerID}/exec", s.executeComputerHTTP)
		r.Get("/commands/{commandID}", s.getComputerCommandHTTP)
		r.Post("/commands/{commandID}/cancel", s.cancelCommandHTTP)
		r.Get("/commands/{commandID}/logs", s.listCommandLogsHTTP)
		r.Get("/computers/{computerID}", s.getComputerHTTP)
		r.Get("/computers/{computerID}/members", s.listComputerMembersHTTP)
		r.Delete("/computers/{computerID}", s.deleteComputerHTTP)
	})
}

func (s *Server) mountWorkerRoutes(r chi.Router) {
	r.Route("/worker/v1", func(r chi.Router) {
		r.Post("/enrollment", s.workerEnroll)
		r.Post("/instance/credential", s.workerIssueHostCredential)
		r.With(s.requireRecoveringWorker).Post("/instance/recover", s.workerStartupRecovery)
		r.With(s.requireWorkerActivation).Post("/instance/activate", s.workerActivate)
		r.With(s.requireWorker).Get("/instance", s.workerStatus)
		r.With(s.requireWorkerDrainCompletion).Post("/instance/drain/complete", s.workerCompleteDrain)
		r.With(s.requireWorkerFence).Post("/instance/fence", s.workerFence)
		r.Group(func(r chi.Router) {
			r.Use(s.requireDiagnosticWorker)
			r.With(limitRequestBody(((s.diagnosticBounds.ChunkBytes+2)/3)*4+8192)).Post("/sessions/logs", s.workerSessionLog)
			r.With(limitRequestBody(((s.diagnosticBounds.ChunkBytes+2)/3)*4+8192)).Post("/allocations/preparation/logs", s.workerPreparationLog)
			r.With(limitRequestBody(((s.diagnosticBounds.ChunkBytes+2)/3)*4+8192)).Post("/computer-commands/logs/append", s.workerAppendCommandLogs)
		})
		r.Group(func(r chi.Router) {
			r.Use(s.requireWorker)
			r.With(limitRequestBody(4096)).Post("/allocations/list", s.workerListAllocations)
			r.With(limitRequestBody(4096)).Post("/allocations/preparation/deliver", s.workerDeliverPreparationAllocation)
			r.With(limitRequestBody(4096)).Post("/allocations/preparation/stopped", s.workerPreparationStopped)
			r.With(limitRequestBody(4096)).Post("/allocations/preparation/renew", s.workerRenewPreparation)
			r.With(limitRequestBody(4096)).Post("/allocations/preparation/key", s.workerPreparationWriteKey)
			r.With(limitRequestBody(4096)).Post("/allocations/preparation/secrets", s.workerPreparationSecrets)
			r.With(limitRequestBody(4096)).Post("/allocations/preparation/start", s.workerPreparationStart)
			r.With(limitRequestBody(4096)).Post("/allocations/preparation/fail", s.workerFailPreparation)
			r.With(limitRequestBody(4096)).Post("/allocations/preparation/capture/begin", s.workerBeginPreparationCapture)
			r.With(limitRequestBody(1048576)).Post("/allocations/preparation/objects/register", s.workerRegisterPreparationObject)
			r.With(limitRequestBody(1048576)).Post("/allocations/preparation/objects/certify", s.workerCertifyPreparationObject)
			r.With(limitRequestBody(8192)).Post("/allocations/preparation/capture", s.workerRecordPreparationCapture)
			r.With(limitRequestBody(8192)).Post("/allocations/preparation/publish", s.workerPublishPreparation)
			r.With(limitRequestBody(4096)).Post("/computer-saves/next", s.workerNextAgentSave)
			r.With(limitRequestBody(1048576)).Post("/computer-saves/objects/register", s.workerRegisterAgentSaveObject)
			r.With(limitRequestBody(1048576)).Post("/computer-saves/objects/certify", s.workerCertifyAgentSaveObject)
			r.With(limitRequestBody(8192)).Post("/computer-saves/capture", s.workerCaptureAgentSave)
			r.With(limitRequestBody(8192)).Post("/computer-saves/publish", s.workerPublishAgentSave)
			r.With(limitRequestBody(4096)).Post("/allocations/computer/deliver", s.workerDeliverComputerAllocation)
			r.With(limitRequestBody(4096)).Post("/allocations/computer/ready", s.workerFreshComputerReady)
			r.With(limitRequestBody(4096)).Post("/allocations/computer/processes", s.workerListComputerProcesses)
			r.With(limitRequestBody(4096)).Post("/allocations/computer/source", s.workerComputerAllocationSource)
			r.With(limitRequestBody(2<<20)).Post("/agent-computers/checkpoint/register", s.workerRegisterAgentCheckpoint)
			r.With(limitRequestBody(2<<20)).Post("/agent-computers/checkpoint/complete", s.workerCompleteAgentCheckpoint)
			r.With(limitRequestBody(4096)).Post("/agent-computers/checkpoint/read", s.workerReadAgentCheckpoint)
			r.With(limitRequestBody(32768)).Post("/agent-computers/capture/begin", s.workerBeginAgentComputerCapture)
			r.With(limitRequestBody(128<<10)).Post("/agent-computers/capture/seal", s.workerSealAgentComputerCapture)
			r.With(limitRequestBody(4096)).Post("/agent-computers/capture/cancel", s.workerCancelAgentComputerCapture)
			r.With(limitRequestBody(32768)).Post("/agent-computers/capture/save-absence", s.workerAgentComputerSaveAbsence)
			r.With(limitRequestBody(32768)).Post("/agent-computers/restore/prepare", s.workerPrepareAgentComputerRestore)
			r.With(limitRequestBody(23<<20)).Post("/agent-computers/controls", s.workerAgentComputerControls)
			r.With(limitRequestBody(128<<10)).Post("/agent-computers/source-abort/prepare", s.workerPrepareAgentComputerSourceAbort)
			r.With(limitRequestBody(23<<20)).Post("/agent-computers/source-abort/validate", s.workerValidateAgentComputerSourceAbort)
			r.With(limitRequestBody(23<<20)).Post("/agent-computers/source-abort/commit", s.workerCommitAgentComputerSourceAbort)
			r.With(limitRequestBody(23<<20)).Post("/agent-computers/source-abort/complete", s.workerCompleteAgentComputerSourceAbort)
			r.With(limitRequestBody(23<<20)).Post("/agent-computers/restore/validate", s.workerValidateAgentComputerRestore)
			r.With(limitRequestBody(23<<20)).Post("/agent-computers/restore/commit", s.workerCommitAgentComputerRestore)
			r.With(limitRequestBody(23<<20)).Post("/agent-computers/restore/complete", s.workerCompleteAgentComputerRestore)
			r.With(limitRequestBody(4096)).Post("/agent-computers/lease/renew", s.workerRenewAgentComputerLease)
			r.With(limitRequestBody(4096)).Post("/agent-computers/stopped", s.workerAgentComputerStopped)
			r.With(limitRequestBody(4096)).Post("/sessions/authority", s.workerAgentAuthority)
			r.With(limitRequestBody(4096)).Post("/sessions/attachment", s.workerAgentAttachment)
			r.With(limitRequestBody(4096)).Post("/sessions/control", s.workerAgentControl)
			r.With(limitRequestBody(8192)).Post("/sessions/control/receipt", s.workerAgentControlReceipt)
			r.With(limitRequestBody(4096)).Post("/sessions/ready", s.workerAgentReady)
			r.With(limitRequestBody(4096)).Post("/sessions/turn", s.workerAgentTurn)
			r.With(limitRequestBody(4096)).Post("/sessions/message", s.workerAgentMessage)
			r.With(limitRequestBody(4096)).Post("/sessions/message/receipt", s.workerAgentMessageReceipt)
			r.With(limitRequestBody(1<<20)).Post("/sessions/turn/receipt", s.workerAgentTurnReceipt)
			r.With(limitRequestBody(4096)).Post("/sessions/start", s.workerAgentStart)
			r.With(limitRequestBody(4096)).Post("/sessions/start/release", s.workerAgentStartRelease)
			r.With(limitRequestBody(4096)).Post("/sessions/stopped", s.workerAgentStopped)
			r.With(limitRequestBody(4096)).Post("/sessions/failed", s.workerAgentFailure)
			r.With(limitRequestBody(17<<20)).Post("/sessions/operations", s.workerAgentOperation)
			r.Post("/instance/observations", s.workerObserve)
			r.Post("/instance/drain", s.workerDrain)
			r.Group(func(r chi.Router) {
				r.With(limitRequestBody(16384)).Post("/run/secret-proxy/prepare", s.workerPrepareSecretProxy)
				r.With(limitRequestBody(16384)).Post("/run/secret-proxy/resolve", s.workerResolveSecretProxy)

				r.Post("/computer-commands/claim", s.workerClaimComputerCommand)
				r.With(limitRequestBody(computerCommandResultLimit)).Post("/computer-commands/reconcile", s.workerReconcileComputerCommand)
				r.With(limitRequestBody(computerCommandResultLimit)).
					Post("/computer-commands/complete", s.workerCompleteComputerCommand)
			})
		})
	})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
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
	var databaseReady bool
	if err := s.readinessDB.QueryRow(ctx, `SELECT NOT current_setting('transaction_read_only')::boolean`).Scan(&databaseReady); err != nil {
		s.writeReadinessUnavailable(w, fmt.Errorf("regional control plane database is not ready: %w", err))
		return
	}
	if !databaseReady {
		s.writeReadinessUnavailable(w, errors.New("regional control plane database session is read only"))
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

func (s *Server) userAuthConfigured() error {
	if !s.authKeys.Valid() {
		return errors.New("user authentication is not configured")
	}
	if s.publicURL == nil {
		return errors.New("public URL is not configured")
	}
	return nil
}

func parseUUIDParam(r *http.Request, name string) (uuid.UUID, error) {
	id, err := ids.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil(), fmt.Errorf("%s must be a canonical UUIDv7", name)
	}
	return id, nil
}
