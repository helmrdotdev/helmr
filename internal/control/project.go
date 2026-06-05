package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var scopeSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	if actor.Role == "" {
		writeError(w, http.StatusForbidden, errors.New("organization is required"))
		return
	}
	projects, err := s.db.ListProjects(r.Context(), ids.ToPG(actor.OrgID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list projects"))
		return
	}
	response := api.ListProjectsResponse{Projects: make([]api.ProjectSummary, 0, len(projects))}
	for _, project := range projects {
		item := projectResponse(projectRecordFromDB(project))
		environments, err := s.db.ListEnvironments(r.Context(), db.ListEnvironmentsParams{
			OrgID:     project.OrgID,
			ProjectID: project.ID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("list environments"))
			return
		}
		item.Environments = make([]api.EnvironmentSummary, 0, len(environments))
		for _, environment := range environments {
			item.Environments = append(item.Environments, environmentResponse(environmentRecordFromDB(environment)))
		}
		response.Projects = append(response.Projects, item)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	project, err := s.db.GetProject(r.Context(), db.GetProjectParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(projectID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("load project"))
		return
	}
	response, err := s.projectResponseWithEnvironments(r.Context(), projectRecordFromDB(project))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	var request api.CreateProjectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid project request JSON: %w", err))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	project, err := s.db.CreateProjectWithDefaultEnvironment(r.Context(), db.CreateProjectWithDefaultEnvironmentParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         ids.ToPG(actor.OrgID),
		Slug:          slug,
		Name:          name,
		IsDefault:     false,
		EnvironmentID: ids.ToPG(ids.New()),
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, errors.New("project slug is already in use"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("create project"))
		return
	}
	environments, err := s.db.ListEnvironments(r.Context(), db.ListEnvironmentsParams{
		OrgID:     project.OrgID,
		ProjectID: project.ID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list environments"))
		return
	}
	response := projectResponse(projectRecordFromCreated(project))
	response.Environments = make([]api.EnvironmentSummary, 0, len(environments))
	for _, environment := range environments {
		response.Environments = append(response.Environments, environmentResponse(environmentRecordFromDB(environment)))
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var request api.UpdateProjectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid project request JSON: %w", err))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	project, err := s.db.UpdateProjectDetails(r.Context(), db.UpdateProjectDetailsParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(projectID),
		Slug:  slug,
		Name:  name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, errors.New("project slug is already in use"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("update project"))
		return
	}
	writeJSON(w, http.StatusOK, projectResponse(projectRecordFromDB(project)))
}

func (s *Server) archiveProject(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	project, err := s.db.GetProject(r.Context(), db.GetProjectParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(projectID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("load project"))
		return
	}
	if project.IsDefault {
		writeError(w, http.StatusBadRequest, errors.New("default project cannot be deleted"))
		return
	}
	projects, err := s.db.ListProjects(r.Context(), ids.ToPG(actor.OrgID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list projects"))
		return
	}
	if len(projects) <= 1 {
		writeError(w, http.StatusBadRequest, errors.New("at least one active project is required"))
		return
	}
	if _, err := s.db.ArchiveProjectWithEnvironments(r.Context(), db.ArchiveProjectWithEnvironmentsParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(projectID),
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, errors.New("at least one active project is required"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("delete project"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("environment storage is not configured"))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var request api.CreateEnvironmentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid environment request JSON: %w", err))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	if _, err := s.db.GetProject(r.Context(), db.GetProjectParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(projectID),
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project not found"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("load project"))
		return
	}
	environment, err := s.db.CreateEnvironment(r.Context(), db.CreateEnvironmentParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: ids.ToPG(projectID),
		Slug:      slug,
		Name:      name,
		IsDefault: false,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, errors.New("environment slug is already in use"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("create environment"))
		return
	}
	writeJSON(w, http.StatusCreated, environmentResponse(environmentRecordFromDB(environment)))
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("environment storage is not configured"))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	environmentID, err := parseUUIDParam(r, "environmentID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	environment, err := s.db.GetEnvironment(r.Context(), db.GetEnvironmentParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: ids.ToPG(projectID),
		ID:        ids.ToPG(environmentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("environment not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("load environment"))
		return
	}
	writeJSON(w, http.StatusOK, environmentResponse(environmentRecordFromDB(environment)))
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("environment storage is not configured"))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	environmentID, err := parseUUIDParam(r, "environmentID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var request api.UpdateEnvironmentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid environment request JSON: %w", err))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	environment, err := s.db.UpdateEnvironmentDetails(r.Context(), db.UpdateEnvironmentDetailsParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: ids.ToPG(projectID),
		ID:        ids.ToPG(environmentID),
		Slug:      slug,
		Name:      name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("environment not found"))
		return
	}
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, errors.New("environment slug is already in use"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("update environment"))
		return
	}
	writeJSON(w, http.StatusOK, environmentResponse(environmentRecordFromDB(environment)))
}

func (s *Server) archiveEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("environment storage is not configured"))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	environmentID, err := parseUUIDParam(r, "environmentID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	environment, err := s.db.GetEnvironment(r.Context(), db.GetEnvironmentParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: ids.ToPG(projectID),
		ID:        ids.ToPG(environmentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("environment not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("load environment"))
		return
	}
	if environment.IsDefault {
		writeError(w, http.StatusBadRequest, errors.New("default environment cannot be deleted"))
		return
	}
	environments, err := s.db.ListEnvironments(r.Context(), db.ListEnvironmentsParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: ids.ToPG(projectID),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list environments"))
		return
	}
	if len(environments) <= 1 {
		writeError(w, http.StatusBadRequest, errors.New("at least one active environment is required"))
		return
	}
	if _, err := s.db.ArchiveEnvironment(r.Context(), db.ArchiveEnvironmentParams{
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: ids.ToPG(projectID),
		ID:        ids.ToPG(environmentID),
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, errors.New("at least one active environment is required"))
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("delete environment"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type deploymentStore interface {
	AllocateDeploymentVersion(context.Context, db.AllocateDeploymentVersionParams) (string, error)
	CreateDeployment(context.Context, db.CreateDeploymentParams) (db.Deployment, error)
	GetReusableDeploymentByContentHash(context.Context, db.GetReusableDeploymentByContentHashParams) (db.Deployment, error)
	LockDeploymentReusableBuildKey(context.Context, db.LockDeploymentReusableBuildKeyParams) error
	UpsertCasObject(context.Context, db.UpsertCasObjectParams) (db.CasObject, error)
}

type currentDeploymentStore interface {
	GetCurrentDeployment(context.Context, db.GetCurrentDeploymentParams) (db.Deployment, error)
	ListDeploymentTasks(context.Context, db.ListDeploymentTasksParams) ([]db.DeploymentTask, error)
}

type deploymentStatusStore interface {
	GetDeployment(context.Context, db.GetDeploymentParams) (db.Deployment, error)
	GetDeploymentForOrg(context.Context, db.GetDeploymentForOrgParams) (db.Deployment, error)
	GetDeploymentByVersion(context.Context, db.GetDeploymentByVersionParams) (db.Deployment, error)
	ListDeploymentsByVersionForOrg(context.Context, db.ListDeploymentsByVersionForOrgParams) ([]db.Deployment, error)
	ListDeploymentTasks(context.Context, db.ListDeploymentTasksParams) ([]db.DeploymentTask, error)
	PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
}

type casObjectLookupStore interface {
	GetCasObject(context.Context, string) (db.CasObject, error)
}

func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("deployment storage is not configured"))
		return
	}
	deploymentID, err := parseUUIDParam(r, "deploymentID")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	scope, _, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	projectID, environmentID, err := s.runScopeIDs(r.Context(), actor.OrgID, scope)
	if err != nil {
		s.log.Error("resolve deployment scope failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get deployment"))
		return
	}
	deployment, err := store.GetDeployment(r.Context(), db.GetDeploymentParams{
		OrgID:         ids.ToPG(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            ids.ToPG(deploymentID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("deployment not found"))
		return
	}
	if err != nil {
		s.log.Error("get deployment failed", "deployment_id", deploymentID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get deployment"))
		return
	}
	response := deploymentResponse(deployment, api.DeploymentSourceArtifact{Digest: deployment.DeploymentSourceDigest})
	response.Tasks = []api.DeploymentTaskResponse{}
	if deployment.Status == db.DeploymentStatusDeployed {
		tasks, err := store.ListDeploymentTasks(r.Context(), db.ListDeploymentTasksParams{
			OrgID:         ids.ToPG(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			DeploymentID:  deployment.ID,
		})
		if err != nil {
			s.log.Error("list deployment tasks failed", "deployment_id", deploymentID.String(), "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("list deployment tasks"))
			return
		}
		response.Tasks = make([]api.DeploymentTaskResponse, 0, len(tasks))
		for _, task := range tasks {
			response.Tasks = append(response.Tasks, deploymentTaskResponse(task))
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getCurrentDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	store, ok := s.db.(currentDeploymentStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("deployment storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	scope, _, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionRunsRead, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	projectID, environmentID, err := s.runScopeIDs(r.Context(), actor.OrgID, scope)
	if err != nil {
		s.log.Error("resolve deployment scope failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get current deployment"))
		return
	}
	deployment, err := store.GetCurrentDeployment(r.Context(), db.GetCurrentDeploymentParams{
		OrgID:         ids.ToPG(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.GetCurrentDeploymentResponse{})
		return
	}
	if err != nil {
		s.log.Error("get current deployment failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get current deployment"))
		return
	}
	tasks, err := store.ListDeploymentTasks(r.Context(), db.ListDeploymentTasksParams{
		OrgID:         ids.ToPG(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deployment.ID,
	})
	if err != nil {
		s.log.Error("list deployment tasks failed", "deployment_id", ids.MustFromPG(deployment.ID).String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list deployment tasks"))
		return
	}
	response := deploymentResponse(deployment, api.DeploymentSourceArtifact{Digest: deployment.DeploymentSourceDigest})
	response.Tasks = make([]api.DeploymentTaskResponse, 0, len(tasks))
	for _, task := range tasks {
		response.Tasks = append(response.Tasks, deploymentTaskResponse(task))
	}
	writeJSON(w, http.StatusOK, api.GetCurrentDeploymentResponse{Deployment: &response})
}

func (s *Server) promoteDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	store, ok := s.db.(deploymentStatusStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("deployment storage is not configured"))
		return
	}
	var request api.PromoteDeploymentRequest
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid promotion request JSON: %w", err))
		return
	}
	actor := actorFromContext(r.Context())
	deploymentRef := strings.TrimSpace(chi.URLParam(r, "deployment"))
	if deploymentRef == "" {
		writeError(w, http.StatusBadRequest, errors.New("deployment is required"))
		return
	}
	deployment, scope, projectID, environmentID, err := s.resolvePromotionTarget(r.Context(), store, actor.OrgID, deploymentRef, request)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("deployment not found"))
		return
	}
	if errors.Is(err, errAmbiguousDeploymentVersion) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		s.log.Error("get deployment for promotion failed", "deployment", deploymentRef, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get deployment"))
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	principal, err := actorIdentityKey(actor)
	if err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	params := db.PromoteDeploymentParams{
		ID:                  ids.ToPG(ids.New()),
		OrgID:               ids.ToPG(actor.OrgID),
		ProjectID:           projectID,
		EnvironmentID:       environmentID,
		DeploymentID:        deployment.ID,
		PromotedByPrincipal: principal,
		Reason:              strings.TrimSpace(request.Reason),
	}
	promoteStore := interface {
		PromoteDeployment(context.Context, db.PromoteDeploymentParams) (db.PromoteDeploymentRow, error)
	}(store)
	if s.tx != nil {
		tx, err := s.tx.Begin(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("begin promotion transaction"))
			return
		}
		defer tx.Rollback(r.Context())
		promoteStore = db.New(tx)
		if _, err := promoteDeploymentAndSyncSchedules(r.Context(), promoteStore, params); errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusBadRequest, errors.New("deployment is not deployable"))
			return
		} else if errors.Is(err, errPermissionRequired) {
			writeError(w, http.StatusForbidden, err)
			return
		} else if err != nil {
			s.log.Error("promote deployment failed", "deployment", deploymentRef, "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("promote deployment"))
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("commit promotion"))
			return
		}
		writeJSON(w, http.StatusOK, deploymentResponse(deployment, api.DeploymentSourceArtifact{Digest: deployment.DeploymentSourceDigest}))
		return
	}
	if _, err := promoteDeploymentAndSyncSchedules(r.Context(), promoteStore, params); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, errors.New("deployment is not deployable"))
		return
	} else if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, err)
		return
	} else if err != nil {
		s.log.Error("promote deployment failed", "deployment", deploymentRef, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("promote deployment"))
		return
	}
	writeJSON(w, http.StatusOK, deploymentResponse(deployment, api.DeploymentSourceArtifact{Digest: deployment.DeploymentSourceDigest}))
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	if s.cas == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("deployment source artifact storage is not configured"))
		return
	}
	reader, request, err := s.receiveDeploymentMetadata(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(request.ProjectID) == "" {
		writeError(w, http.StatusBadRequest, errors.New("project_id is required"))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.secretRequestScope(r.Context(), actor.OrgID, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionTasksDeploy, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	archivePath, cleanup, err := receiveDeploymentArchive(reader)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer cleanup()
	if err := validateDeploymentSourceArtifactArchive(archivePath); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid deployment source artifact: %w", err))
		return
	}
	if err := validateDeploymentContentHash(archivePath, request.ContentHash); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	file, err := os.Open(archivePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("open deployment source artifact"))
		return
	}
	artifactObject, err := s.cas.Put(r.Context(), api.DeploymentSourceArtifactMediaType, file)
	closeErr := file.Close()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("store deployment source artifact: %w", err))
		return
	}
	if closeErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("close deployment source artifact: %w", closeErr))
		return
	}
	artifact := api.DeploymentSourceArtifact{
		Digest:    artifactObject.Digest,
		SizeBytes: artifactObject.SizeBytes,
		MediaType: artifactObject.MediaType,
	}
	cleanupArtifact := func() {
		s.deleteUnreferencedDeploymentSourceArtifact(r.Context(), artifact.Digest)
	}
	store, ok := s.db.(deploymentStore)
	if !ok {
		cleanupArtifact()
		writeError(w, http.StatusServiceUnavailable, errors.New("deployment storage is not configured"))
		return
	}
	if s.tx != nil {
		tx, err := s.tx.Begin(r.Context())
		if err != nil {
			cleanupArtifact()
			writeError(w, http.StatusInternalServerError, errors.New("begin deployment transaction"))
			return
		}
		defer tx.Rollback(r.Context())
		store = db.New(tx)
		response, err := createDeploymentRecords(r.Context(), store, actor.OrgID, projectID, environmentID, strings.TrimSpace(request.ContentHash), artifact)
		if err != nil {
			cleanupArtifact()
			writeDeploymentError(w, s, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			cleanupArtifact()
			writeError(w, http.StatusInternalServerError, errors.New("commit deployment"))
			return
		}
		writeJSON(w, http.StatusCreated, response)
		return
	}
	response, err := createDeploymentRecords(r.Context(), store, actor.OrgID, projectID, environmentID, strings.TrimSpace(request.ContentHash), artifact)
	if err != nil {
		cleanupArtifact()
		writeDeploymentError(w, s, err)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) receiveDeploymentMetadata(r *http.Request) (*multipart.Reader, api.CreateDeploymentRequest, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, api.CreateDeploymentRequest{}, fmt.Errorf("invalid deployment multipart form: %w", err)
	}
	var request api.CreateDeploymentRequest
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata is required")
		}
		if err != nil {
			return nil, api.CreateDeploymentRequest{}, fmt.Errorf("read deployment multipart form: %w", err)
		}
		name := part.FormName()
		switch name {
		case "metadata":
			metadata, err := readLimitedFormField(part, 1<<20)
			if err != nil {
				return nil, api.CreateDeploymentRequest{}, fmt.Errorf("read deployment metadata: %w", err)
			}
			decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(metadata)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return nil, api.CreateDeploymentRequest{}, fmt.Errorf("invalid deployment metadata JSON: %w", err)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata must contain a single JSON value")
			}
			return reader, request, nil
		case "deployment_source":
			return nil, api.CreateDeploymentRequest{}, errors.New("deployment metadata must precede deployment_source")
		default:
			return nil, api.CreateDeploymentRequest{}, fmt.Errorf("unexpected deployment multipart field %q", name)
		}
	}
}

func receiveDeploymentArchive(reader *multipart.Reader) (string, func(), error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return "", func() {}, errors.New("deployment_source file is required")
		}
		if err != nil {
			return "", func() {}, fmt.Errorf("read deployment multipart form: %w", err)
		}
		if part.FormName() != "deployment_source" {
			part.Close()
			continue
		}
		defer part.Close()
		tmp, err := os.CreateTemp("", "helmr-deployment-source-*.tar")
		if err != nil {
			return "", func() {}, fmt.Errorf("create deployment source temp file: %w", err)
		}
		path := tmp.Name()
		cleanup := func() { _ = os.Remove(path) }
		if _, err := io.Copy(tmp, part); err != nil {
			_ = tmp.Close()
			cleanup()
			return "", func() {}, fmt.Errorf("copy deployment source artifact: %w", err)
		}
		if err := tmp.Close(); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("close deployment source artifact: %w", err)
		}
		return path, cleanup, nil
	}
}

func readLimitedFormField(part *multipart.Part, limit int64) (string, error) {
	defer part.Close()
	bytes, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(bytes)) > limit {
		return "", errors.New("field is too large")
	}
	return string(bytes), nil
}

func (s *Server) deleteUnreferencedDeploymentSourceArtifact(ctx context.Context, digest string) {
	digest = strings.TrimSpace(digest)
	if digest == "" || s.cas == nil {
		return
	}
	if store, ok := s.db.(casObjectLookupStore); ok {
		if _, err := store.GetCasObject(ctx, digest); err == nil {
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			if s.log != nil {
				s.log.Warn("skip deployment source artifact cleanup after CAS lookup failure", "digest", digest, "error", err)
			}
			return
		}
	}
	if err := s.cas.Delete(ctx, digest); err != nil && s.log != nil {
		s.log.Warn("delete unreferenced deployment source artifact", "digest", digest, "error", err)
	}
}

func validateDeploymentSourceArtifactArchive(archivePath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open deployment source artifact: %w", err)
	}
	defer file.Close()
	destination, err := os.MkdirTemp("", "helmr-deployment-source-validate-*")
	if err != nil {
		return fmt.Errorf("create deployment source validation directory: %w", err)
	}
	defer os.RemoveAll(destination)
	if err := archive.ExtractTar(file, destination); err != nil {
		return err
	}
	return nil
}

func validateDeploymentContentHash(archivePath string, contentHash string) error {
	contentHash = strings.TrimSpace(contentHash)
	if contentHash == "" {
		return errors.New("deployment content_hash is required")
	}
	digest, err := deploymentArchiveDigest(archivePath)
	if err != nil {
		return fmt.Errorf("hash deployment source artifact: %w", err)
	}
	if digest != contentHash {
		return fmt.Errorf("deployment source content_hash %s does not match uploaded archive digest %s", contentHash, digest)
	}
	return nil
}

func deploymentArchiveDigest(archivePath string) (string, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func createDeploymentRecords(ctx context.Context, store deploymentStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, contentHash string, artifact api.DeploymentSourceArtifact) (api.DeploymentResponse, error) {
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}); err != nil {
		return api.DeploymentResponse{}, err
	}
	if err := store.LockDeploymentReusableBuildKey(ctx, db.LockDeploymentReusableBuildKeyParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ContentHash:   contentHash,
	}); err != nil {
		return api.DeploymentResponse{}, err
	}
	deployment, err := store.GetReusableDeploymentByContentHash(ctx, db.GetReusableDeploymentByContentHashParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ContentHash:   contentHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		deployment, err = createQueuedDeployment(ctx, store, orgID, projectID, environmentID, contentHash, artifact)
	}
	if err != nil {
		return api.DeploymentResponse{}, err
	}
	return deploymentResponse(deployment, artifact), nil
}

func createQueuedDeployment(ctx context.Context, store deploymentStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, contentHash string, artifact api.DeploymentSourceArtifact) (db.Deployment, error) {
	version, err := nextDeploymentVersion(ctx, store, orgID, projectID, environmentID)
	if err != nil {
		return db.Deployment{}, err
	}
	return store.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID:                     ids.ToPG(ids.New()),
		OrgID:                  ids.ToPG(orgID),
		ProjectID:              projectID,
		EnvironmentID:          environmentID,
		Version:                version,
		ContentHash:            contentHash,
		DeploymentSourceDigest: artifact.Digest,
		Status:                 db.DeploymentStatusQueued,
	})
}

func nextDeploymentVersion(ctx context.Context, store deploymentStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID) (string, error) {
	return store.AllocateDeploymentVersion(ctx, db.AllocateDeploymentVersionParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Prefix:        deploymentVersionPrefix(),
	})
}

func deploymentVersionPrefix() string {
	return time.Now().UTC().Format("20060102")
}

var errAmbiguousDeploymentVersion = errors.New("deployment version is ambiguous; provide project_id and environment_id")

func (s *Server) resolvePromotionTarget(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, deploymentRef string, request api.PromoteDeploymentRequest) (db.Deployment, auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectRef := strings.TrimSpace(request.ProjectID)
	environmentRef := strings.TrimSpace(request.EnvironmentID)
	if projectRef != "" || environmentRef != "" {
		scope, projectID, environmentID, err := s.secretRequestScope(ctx, orgID, projectRef, environmentRef)
		if err != nil {
			return db.Deployment{}, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
		}
		deployment, err := deploymentByIDOrVersion(ctx, store, orgID, projectID, environmentID, deploymentRef)
		return deployment, scope, projectID, environmentID, err
	}
	deployment, err := deploymentByIDOrVersionForOrg(ctx, store, orgID, deploymentRef)
	if err != nil {
		return db.Deployment{}, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	scope, projectID, environmentID, err := s.deploymentScope(ctx, orgID, deployment)
	if err != nil {
		return db.Deployment{}, auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return deployment, scope, projectID, environmentID, nil
}

func deploymentByIDOrVersionForOrg(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, deploymentRef string) (db.Deployment, error) {
	if deploymentID, err := ids.Parse(deploymentRef); err == nil {
		return store.GetDeploymentForOrg(ctx, db.GetDeploymentForOrgParams{
			OrgID: ids.ToPG(orgID),
			ID:    ids.ToPG(deploymentID),
		})
	}
	deployments, err := store.ListDeploymentsByVersionForOrg(ctx, db.ListDeploymentsByVersionForOrgParams{
		OrgID:   ids.ToPG(orgID),
		Version: strings.TrimSpace(deploymentRef),
	})
	if err != nil {
		return db.Deployment{}, err
	}
	switch len(deployments) {
	case 0:
		return db.Deployment{}, pgx.ErrNoRows
	case 1:
		return deployments[0], nil
	default:
		return db.Deployment{}, errAmbiguousDeploymentVersion
	}
}

func (s *Server) deploymentScope(ctx context.Context, orgID uuid.UUID, deployment db.Deployment) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	defaultScope, err := s.db.GetDefaultProjectEnvironment(ctx, ids.ToPG(orgID))
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	if deployment.ProjectID == defaultScope.ProjectID && deployment.EnvironmentID == defaultScope.EnvironmentID {
		return auth.DefaultScope(orgID), deployment.ProjectID, deployment.EnvironmentID, nil
	}
	return auth.Scope{
		OrgID:         orgID,
		ProjectID:     ids.MustFromPG(deployment.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(deployment.EnvironmentID).String(),
	}, deployment.ProjectID, deployment.EnvironmentID, nil
}

func deploymentByIDOrVersion(ctx context.Context, store deploymentStatusStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, deploymentRef string) (db.Deployment, error) {
	if deploymentID, err := ids.Parse(deploymentRef); err == nil {
		return store.GetDeployment(ctx, db.GetDeploymentParams{
			OrgID:         ids.ToPG(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            ids.ToPG(deploymentID),
		})
	}
	return store.GetDeploymentByVersion(ctx, db.GetDeploymentByVersionParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Version:       strings.TrimSpace(deploymentRef),
	})
}

func deploymentResponse(deployment db.Deployment, artifact api.DeploymentSourceArtifact) api.DeploymentResponse {
	if artifact.Digest == "" {
		artifact.Digest = deployment.DeploymentSourceDigest
	}
	return api.DeploymentResponse{
		ID:                       ids.MustFromPG(deployment.ID).String(),
		Version:                  deployment.Version,
		ProjectID:                ids.MustFromPG(deployment.ProjectID).String(),
		EnvironmentID:            ids.MustFromPG(deployment.EnvironmentID).String(),
		ContentHash:              deployment.ContentHash,
		DeploymentSource:         artifact,
		BuildManifestDigest:      pgTextString(deployment.BuildManifestDigest),
		DeploymentManifestDigest: pgTextString(deployment.DeploymentManifestDigest),
		Status:                   string(deployment.Status),
		Error:                    deploymentErrorResponse(deployment.Failure),
		CreatedAt:                pgTime(deployment.CreatedAt),
		BuildingAt:               pgTime(deployment.BuildingAt),
		BuiltAt:                  pgTime(deployment.BuiltAt),
		DeployedAt:               pgTime(deployment.DeployedAt),
		FailedAt:                 pgTime(deployment.FailedAt),
	}
}

func deploymentErrorResponse(raw []byte) *api.DeploymentErrorResponse {
	message := strings.TrimSpace(string(raw))
	if message == "" || message == "null" {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return &api.DeploymentErrorResponse{Message: message}
	}
	if value, ok := payload["message"].(string); ok && strings.TrimSpace(value) != "" {
		return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
	}
	if value, ok := payload["error"].(string); ok && strings.TrimSpace(value) != "" {
		return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		if value, ok := nested["message"].(string); ok && strings.TrimSpace(value) != "" {
			return &api.DeploymentErrorResponse{Message: strings.TrimSpace(value)}
		}
	}
	return nil
}

func deploymentTaskResponse(task db.DeploymentTask) api.DeploymentTaskResponse {
	return api.DeploymentTaskResponse{
		ID:                ids.MustFromPG(task.ID).String(),
		TaskID:            task.TaskID,
		FilePath:          task.FilePath,
		ExportName:        task.ExportName,
		HandlerEntrypoint: task.HandlerEntrypoint,
		BundleDigest:      task.BundleDigest,
		QueueName:         task.QueueName,
		ConcurrencyLimit:  pgInt4Response(task.QueueConcurrencyLimit),
		TTL:               task.Ttl,
		CreatedAt:         pgTime(task.CreatedAt),
	}
}

func pgInt4Response(value pgtype.Int4) *int32 {
	if !value.Valid {
		return nil
	}
	return &value.Int32
}

func pgTextString(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func writeDeploymentError(w http.ResponseWriter, s *Server, err error) {
	if isUniqueViolation(err) {
		writeError(w, http.StatusBadRequest, errors.New("deployment conflicts with existing task metadata"))
		return
	}
	s.log.Error("create deployment failed", "error", err)
	writeError(w, http.StatusInternalServerError, errors.New("create deployment"))
}

func normalizeScopeCreateInput(slug string, name string) (string, string, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	name = strings.TrimSpace(name)
	if !scopeSlugPattern.MatchString(slug) {
		return "", "", fmt.Errorf("slug must match %s", scopeSlugPattern.String())
	}
	if name == "" {
		name = slug
	}
	if len(name) > 80 || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", "", errors.New("name must be 1-80 characters and contain no control characters")
	}
	return slug, name, nil
}

type projectRecord struct {
	id        pgtype.UUID
	orgID     pgtype.UUID
	slug      string
	name      string
	isDefault bool
	createdAt pgtype.Timestamptz
	updatedAt pgtype.Timestamptz
}

type environmentRecord struct {
	id        pgtype.UUID
	projectID pgtype.UUID
	slug      string
	name      string
	isDefault bool
	createdAt pgtype.Timestamptz
	updatedAt pgtype.Timestamptz
}

func projectResponse(project projectRecord) api.ProjectSummary {
	return api.ProjectSummary{
		ID:        ids.MustFromPG(project.id).String(),
		Slug:      project.slug,
		Name:      project.name,
		IsDefault: project.isDefault,
		CreatedAt: pgTime(project.createdAt),
		UpdatedAt: pgTime(project.updatedAt),
	}
}

func (s *Server) projectResponseWithEnvironments(ctx context.Context, project projectRecord) (api.ProjectSummary, error) {
	response := projectResponse(project)
	environments, err := s.db.ListEnvironments(ctx, db.ListEnvironmentsParams{
		OrgID:     project.orgID,
		ProjectID: project.id,
	})
	if err != nil {
		return api.ProjectSummary{}, errors.New("list environments")
	}
	response.Environments = make([]api.EnvironmentSummary, 0, len(environments))
	for _, environment := range environments {
		response.Environments = append(response.Environments, environmentResponse(environmentRecordFromDB(environment)))
	}
	return response, nil
}

func environmentResponse(environment environmentRecord) api.EnvironmentSummary {
	return api.EnvironmentSummary{
		ID:        ids.MustFromPG(environment.id).String(),
		ProjectID: ids.MustFromPG(environment.projectID).String(),
		Slug:      environment.slug,
		Name:      environment.name,
		IsDefault: environment.isDefault,
		CreatedAt: pgTime(environment.createdAt),
		UpdatedAt: pgTime(environment.updatedAt),
	}
}

func projectRecordFromDB(project db.Project) projectRecord {
	return projectRecord{
		id:        project.ID,
		orgID:     project.OrgID,
		slug:      project.Slug,
		name:      project.Name,
		isDefault: project.IsDefault,
		createdAt: project.CreatedAt,
		updatedAt: project.UpdatedAt,
	}
}

func projectRecordFromCreated(project db.CreateProjectWithDefaultEnvironmentRow) projectRecord {
	return projectRecord{
		id:        project.ID,
		orgID:     project.OrgID,
		slug:      project.Slug,
		name:      project.Name,
		isDefault: project.IsDefault,
		createdAt: project.CreatedAt,
		updatedAt: project.UpdatedAt,
	}
}

func environmentRecordFromDB(environment db.Environment) environmentRecord {
	return environmentRecord{
		id:        environment.ID,
		projectID: environment.ProjectID,
		slug:      environment.Slug,
		name:      environment.Name,
		isDefault: environment.IsDefault,
		createdAt: environment.CreatedAt,
		updatedAt: environment.UpdatedAt,
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
