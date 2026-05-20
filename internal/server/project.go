package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
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
	environment, err := s.db.CreateEnvironmentWithDefaultWorkerGroup(r.Context(), db.CreateEnvironmentWithDefaultWorkerGroupParams{
		ID:        ids.ToPG(ids.New()),
		OrgID:     ids.ToPG(actor.OrgID),
		ProjectID: ids.ToPG(projectID),
		Slug:      slug,
		Name:      name,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, errors.New("environment slug is already in use"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("create environment"))
		return
	}
	writeJSON(w, http.StatusCreated, environmentResponse(environmentRecordFromCreated(environment)))
}

type taskDeploymentStore interface {
	ActivateTaskDeployment(context.Context, db.ActivateTaskDeploymentParams) (db.TaskDeployment, error)
	CreateTaskDeployment(context.Context, db.CreateTaskDeploymentParams) (db.TaskDeployment, error)
	CreateDeployedTask(context.Context, db.CreateDeployedTaskParams) (db.DeployedTask, error)
	UpsertCasObject(context.Context, db.UpsertCasObjectParams) (db.CasObject, error)
}

type activeTaskDeploymentStore interface {
	GetActiveTaskDeployment(context.Context, db.GetActiveTaskDeploymentParams) (db.TaskDeployment, error)
	ListDeployedTasksForDeployment(context.Context, db.ListDeployedTasksForDeploymentParams) ([]db.DeployedTask, error)
}

type casObjectLookupStore interface {
	GetCasObject(context.Context, string) (db.CasObject, error)
}

func (s *Server) getActiveTaskDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	store, ok := s.db.(activeTaskDeploymentStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("task deployment storage is not configured"))
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
		s.log.Error("resolve task deployment scope failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get active task deployment"))
		return
	}
	deployment, err := store.GetActiveTaskDeployment(r.Context(), db.GetActiveTaskDeploymentParams{
		OrgID:         ids.ToPG(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.GetActiveTaskDeploymentResponse{})
		return
	}
	if err != nil {
		s.log.Error("get active task deployment failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get active task deployment"))
		return
	}
	tasks, err := store.ListDeployedTasksForDeployment(r.Context(), db.ListDeployedTasksForDeploymentParams{
		OrgID:         ids.ToPG(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		DeploymentID:  deployment.ID,
	})
	if err != nil {
		s.log.Error("list deployed tasks failed", "deployment_id", ids.MustFromPG(deployment.ID).String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("list deployed tasks"))
		return
	}
	response := taskDeploymentResponse(deployment, api.TaskSourceArtifact{Digest: deployment.SourceDigest})
	response.Tasks = make([]api.DeployedTaskResponse, 0, len(tasks))
	for _, task := range tasks {
		response.Tasks = append(response.Tasks, deployedTaskResponse(task))
	}
	writeJSON(w, http.StatusOK, api.GetActiveTaskDeploymentResponse{Deployment: &response})
}

func (s *Server) createTaskDeployment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("project storage is not configured"))
		return
	}
	if s.cas == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("task source artifact storage is not configured"))
		return
	}
	reader, request, err := s.receiveTaskDeploymentMetadata(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
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
	tasks, err := validateIndexedDeployedTasks(request.Tasks)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid task deployment metadata: %w", err))
		return
	}
	archivePath, cleanup, err := receiveTaskDeploymentArchive(reader)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	defer cleanup()
	if err := validateTaskSourceArtifactArchive(archivePath); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid task source artifact: %w", err))
		return
	}
	file, err := os.Open(archivePath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("open task source artifact"))
		return
	}
	artifactObject, err := s.cas.Put(r.Context(), api.TaskSourceArtifactMediaType, file)
	closeErr := file.Close()
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("store task source artifact: %w", err))
		return
	}
	if closeErr != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("close task source artifact: %w", closeErr))
		return
	}
	artifact := api.TaskSourceArtifact{
		Digest:    artifactObject.Digest,
		SizeBytes: artifactObject.SizeBytes,
		MediaType: artifactObject.MediaType,
	}
	cleanupArtifact := func() {
		s.deleteUnreferencedTaskSourceArtifact(r.Context(), artifact.Digest)
	}
	store, ok := s.db.(taskDeploymentStore)
	if !ok {
		cleanupArtifact()
		writeError(w, http.StatusServiceUnavailable, errors.New("task deployment storage is not configured"))
		return
	}
	if s.tx != nil {
		tx, err := s.tx.Begin(r.Context())
		if err != nil {
			cleanupArtifact()
			writeError(w, http.StatusInternalServerError, errors.New("begin task deployment transaction"))
			return
		}
		defer tx.Rollback(r.Context())
		store = db.New(tx)
		response, err := createTaskDeploymentRecords(r.Context(), store, actor.OrgID, projectID, environmentID, artifact, tasks)
		if err != nil {
			cleanupArtifact()
			writeTaskDeploymentError(w, s, err)
			return
		}
		if err := tx.Commit(r.Context()); err != nil {
			cleanupArtifact()
			writeError(w, http.StatusInternalServerError, errors.New("commit task deployment"))
			return
		}
		writeJSON(w, http.StatusCreated, response)
		return
	}
	response, err := createTaskDeploymentRecords(r.Context(), store, actor.OrgID, projectID, environmentID, artifact, tasks)
	if err != nil {
		cleanupArtifact()
		writeTaskDeploymentError(w, s, err)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) receiveTaskDeploymentMetadata(r *http.Request) (*multipart.Reader, api.CreateTaskDeploymentRequest, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, api.CreateTaskDeploymentRequest{}, fmt.Errorf("invalid task deployment multipart form: %w", err)
	}
	var request api.CreateTaskDeploymentRequest
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, api.CreateTaskDeploymentRequest{}, errors.New("task deployment metadata is required")
		}
		if err != nil {
			return nil, api.CreateTaskDeploymentRequest{}, fmt.Errorf("read task deployment multipart form: %w", err)
		}
		name := part.FormName()
		switch name {
		case "metadata":
			metadata, err := readLimitedFormField(part, 1<<20)
			if err != nil {
				return nil, api.CreateTaskDeploymentRequest{}, fmt.Errorf("read task deployment metadata: %w", err)
			}
			decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(metadata)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&request); err != nil {
				return nil, api.CreateTaskDeploymentRequest{}, fmt.Errorf("invalid task deployment metadata JSON: %w", err)
			}
			if err := decoder.Decode(&struct{}{}); err != io.EOF {
				return nil, api.CreateTaskDeploymentRequest{}, errors.New("task deployment metadata must contain a single JSON value")
			}
			return reader, request, nil
		case "project_id":
			value, err := readLimitedFormField(part, 1024)
			if err != nil {
				return nil, api.CreateTaskDeploymentRequest{}, fmt.Errorf("read project_id: %w", err)
			}
			request.ProjectID = strings.TrimSpace(value)
		case "environment_id":
			value, err := readLimitedFormField(part, 1024)
			if err != nil {
				return nil, api.CreateTaskDeploymentRequest{}, fmt.Errorf("read environment_id: %w", err)
			}
			request.EnvironmentID = strings.TrimSpace(value)
		case "source_tar":
			return nil, api.CreateTaskDeploymentRequest{}, errors.New("task deployment metadata must precede source_tar")
		default:
			return nil, api.CreateTaskDeploymentRequest{}, fmt.Errorf("unexpected task deployment multipart field %q", name)
		}
	}
}

func receiveTaskDeploymentArchive(reader *multipart.Reader) (string, func(), error) {
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return "", func() {}, errors.New("source_tar file is required")
		}
		if err != nil {
			return "", func() {}, fmt.Errorf("read task deployment multipart form: %w", err)
		}
		if part.FormName() != "source_tar" {
			part.Close()
			continue
		}
		defer part.Close()
		tmp, err := os.CreateTemp("", "helmr-task-source-*.tar")
		if err != nil {
			return "", func() {}, fmt.Errorf("create task source temp file: %w", err)
		}
		path := tmp.Name()
		cleanup := func() { _ = os.Remove(path) }
		if _, err := io.Copy(tmp, part); err != nil {
			_ = tmp.Close()
			cleanup()
			return "", func() {}, fmt.Errorf("copy task source artifact: %w", err)
		}
		if err := tmp.Close(); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("close task source artifact: %w", err)
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

func (s *Server) deleteUnreferencedTaskSourceArtifact(ctx context.Context, digest string) {
	digest = strings.TrimSpace(digest)
	if digest == "" || s.cas == nil {
		return
	}
	if store, ok := s.db.(casObjectLookupStore); ok {
		if _, err := store.GetCasObject(ctx, digest); err == nil {
			return
		} else if !errors.Is(err, pgx.ErrNoRows) {
			if s.log != nil {
				s.log.Warn("skip task source artifact cleanup after CAS lookup failure", "digest", digest, "error", err)
			}
			return
		}
	}
	if err := s.cas.Delete(ctx, digest); err != nil && s.log != nil {
		s.log.Warn("delete unreferenced task source artifact", "digest", digest, "error", err)
	}
}

func validateTaskSourceArtifactArchive(archivePath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open task source artifact: %w", err)
	}
	defer file.Close()
	destination, err := os.MkdirTemp("", "helmr-task-source-validate-*")
	if err != nil {
		return fmt.Errorf("create task source validation directory: %w", err)
	}
	defer os.RemoveAll(destination)
	if err := sourcetar.ExtractTar(file, destination); err != nil {
		return err
	}
	return nil
}

func validateIndexedDeployedTasks(input []api.DeployedTaskCreate) ([]api.DeployedTaskCreate, error) {
	if len(input) == 0 {
		return nil, errors.New("task source must contain at least one deployed task")
	}
	seen := map[string]struct{}{}
	tasks := make([]api.DeployedTaskCreate, 0, len(input))
	for _, task := range input {
		taskID := strings.TrimSpace(task.TaskID)
		if err := api.ValidateTaskID(taskID); err != nil {
			return nil, err
		}
		if _, ok := seen[taskID]; ok {
			return nil, fmt.Errorf("duplicate task_id %q", taskID)
		}
		modulePath := strings.TrimSpace(task.ModulePath)
		if modulePath == "" {
			return nil, fmt.Errorf("task %q module_path is required", taskID)
		}
		exportName := strings.TrimSpace(task.ExportName)
		if exportName == "" {
			return nil, fmt.Errorf("task %q export_name is required", taskID)
		}
		seen[taskID] = struct{}{}
		resources := compute.DefaultRunResources()
		if task.RequestedMilliCPU != 0 {
			resources.MilliCPU = task.RequestedMilliCPU
		}
		if task.RequestedMemoryMiB != 0 {
			resources.MemoryMiB = task.RequestedMemoryMiB
		}
		if err := resources.Validate(true); err != nil {
			return nil, fmt.Errorf("task %q resources: %w", taskID, err)
		}
		tasks = append(tasks, api.DeployedTaskCreate{
			TaskID:             taskID,
			ModulePath:         modulePath,
			ExportName:         exportName,
			RequestedMilliCPU:  resources.MilliCPU,
			RequestedMemoryMiB: resources.MemoryMiB,
		})
	}
	return tasks, nil
}

func createTaskDeploymentRecords(ctx context.Context, store taskDeploymentStore, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, artifact api.TaskSourceArtifact, tasks []api.DeployedTaskCreate) (api.TaskDeploymentResponse, error) {
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}); err != nil {
		return api.TaskDeploymentResponse{}, err
	}
	deployment, err := store.CreateTaskDeployment(ctx, db.CreateTaskDeploymentParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		SourceDigest:  artifact.Digest,
		Status:        db.TaskDeploymentStatusCreating,
	})
	if err != nil {
		return api.TaskDeploymentResponse{}, err
	}
	deployedTasks := make([]api.DeployedTaskResponse, 0, len(tasks))
	for _, task := range tasks {
		deployedTask, err := store.CreateDeployedTask(ctx, db.CreateDeployedTaskParams{
			ID:                 ids.ToPG(ids.New()),
			OrgID:              ids.ToPG(orgID),
			ProjectID:          projectID,
			EnvironmentID:      environmentID,
			DeploymentID:       deployment.ID,
			TaskID:             task.TaskID,
			ModulePath:         task.ModulePath,
			ExportName:         task.ExportName,
			RequestedMilliCpu:  task.RequestedMilliCPU,
			RequestedMemoryMib: task.RequestedMemoryMiB,
		})
		if err != nil {
			return api.TaskDeploymentResponse{}, err
		}
		deployedTasks = append(deployedTasks, deployedTaskResponse(deployedTask))
	}
	deployment, err = store.ActivateTaskDeployment(ctx, db.ActivateTaskDeploymentParams{
		OrgID:         ids.ToPG(orgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            deployment.ID,
	})
	if err != nil {
		return api.TaskDeploymentResponse{}, err
	}
	response := taskDeploymentResponse(deployment, artifact)
	response.Tasks = deployedTasks
	return response, nil
}

func taskDeploymentResponse(deployment db.TaskDeployment, artifact api.TaskSourceArtifact) api.TaskDeploymentResponse {
	if artifact.Digest == "" {
		artifact.Digest = deployment.SourceDigest
	}
	return api.TaskDeploymentResponse{
		ID:             ids.MustFromPG(deployment.ID).String(),
		ProjectID:      ids.MustFromPG(deployment.ProjectID).String(),
		EnvironmentID:  ids.MustFromPG(deployment.EnvironmentID).String(),
		SourceArtifact: artifact,
		Status:         string(deployment.Status),
		CreatedAt:      pgTime(deployment.CreatedAt),
		DeployedAt:     pgTime(deployment.DeployedAt),
	}
}

func deployedTaskResponse(task db.DeployedTask) api.DeployedTaskResponse {
	return api.DeployedTaskResponse{
		ID:         ids.MustFromPG(task.ID).String(),
		TaskID:     task.TaskID,
		ModulePath: task.ModulePath,
		ExportName: task.ExportName,
		CreatedAt:  pgTime(task.CreatedAt),
	}
}

func writeTaskDeploymentError(w http.ResponseWriter, s *Server, err error) {
	if isUniqueViolation(err) {
		writeError(w, http.StatusBadRequest, errors.New("task deployment conflicts with an active deployment"))
		return
	}
	s.log.Error("create task deployment failed", "error", err)
	writeError(w, http.StatusInternalServerError, errors.New("create task deployment"))
}

func relabelGitHubSourceError(err error, field string) error {
	return errors.New(strings.ReplaceAll(err.Error(), "source.", field+"."))
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

func environmentRecordFromCreated(environment db.CreateEnvironmentWithDefaultWorkerGroupRow) environmentRecord {
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
