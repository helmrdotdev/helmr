package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cell"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var scopeSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func protectedEnvironmentSlug(slug string) bool {
	return slug == "production" || slug == "staging"
}

func (s *Server) failDeletionJob(ctx context.Context, orgID pgtype.UUID, jobID pgtype.UUID, failure error) {
	if failure == nil || s.db == nil {
		return
	}
	if ctx.Err() != nil {
		fallbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx = fallbackCtx
	}
	if _, err := s.db.FailDeletionJob(ctx, db.FailDeletionJobParams{
		OrgID:   orgID,
		ID:      jobID,
		Failure: failure.Error(),
	}); err != nil && s.log != nil {
		s.log.Error("fail deletion job", "job_id", pgvalue.MustUUIDValue(jobID).String(), "error", err)
	}
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	if actor.Role == "" {
		writeError(w, forbidden(errors.New("organization is required")))
		return
	}
	projects, err := s.db.ListProjects(r.Context(), pgvalue.UUID(actor.OrgID))
	if err != nil {
		writeError(w, errors.New("list projects"))
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
			writeError(w, errors.New("list environments"))
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
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	project, err := s.db.GetProject(r.Context(), db.GetProjectParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(projectID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("project not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load project"))
		return
	}
	response, err := s.projectResponseWithEnvironments(r.Context(), projectRecordFromDB(project))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	var request api.CreateProjectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid project request JSON: %w", err)))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	var project db.CreateProjectWithDefaultEnvironmentRow
	var environments []db.Environment
	err = s.inTx(r.Context(), func(work *txWork) error {
		var projectPublicID, productionPublicID, stagingPublicID string
		project, err = createWithPublicID(r.Context(), []publicIDSlot{
			{prefix: publicid.Project, value: &projectPublicID},
			{prefix: publicid.Environment, value: &productionPublicID},
			{prefix: publicid.Environment, value: &stagingPublicID},
		}, func() (db.CreateProjectWithDefaultEnvironmentRow, error) {
			return work.q.CreateProjectWithDefaultEnvironment(r.Context(), db.CreateProjectWithDefaultEnvironmentParams{
				ID:                         pgvalue.UUID(uuid.Must(uuid.NewV7())),
				PublicID:                   projectPublicID,
				OrgID:                      pgvalue.UUID(actor.OrgID),
				DefaultRegionID:            s.defaultRegionID,
				Slug:                       slug,
				Name:                       name,
				IsDefault:                  false,
				EnvironmentID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
				EnvironmentPublicID:        productionPublicID,
				StagingEnvironmentID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
				StagingEnvironmentPublicID: stagingPublicID,
			})
		})
		if err != nil {
			if isUniqueViolation(err) {
				return badRequest(errors.New("project slug is already in use"))
			}
			return errors.New("create project")
		}
		environments, err = work.q.ListEnvironments(r.Context(), db.ListEnvironmentsParams{
			OrgID:     project.OrgID,
			ProjectID: project.ID,
		})
		if err != nil {
			return errors.New("list environments")
		}
		for _, environment := range environments {
			if _, err := cell.EnsureEnvironmentRoute(r.Context(), work.q, cell.EnsureEnvironmentRouteParams{
				OrgID:         project.OrgID,
				ProjectID:     project.ID,
				EnvironmentID: environment.ID,
				RegionID:      environment.DefaultRegionID,
				LocalCellID:   s.cellID,
			}); err != nil {
				if errors.Is(err, cell.ErrRouteUnavailable) || errors.Is(err, cell.ErrPlacementOutsideLocalCell) {
					return unavailable(errors.New("environment route is unavailable"))
				}
				return errors.New("create environment route")
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
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
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.UpdateProjectRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid project request JSON: %w", err)))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	project, err := s.db.UpdateProjectDetails(r.Context(), db.UpdateProjectDetailsParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(projectID),
		Slug:  slug,
		Name:  name,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("project not found")))
		return
	}
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, badRequest(errors.New("project slug is already in use")))
			return
		}
		writeError(w, errors.New("update project"))
		return
	}
	writeJSON(w, http.StatusOK, projectResponse(projectRecordFromDB(project)))
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	project, err := s.db.GetProject(r.Context(), db.GetProjectParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(projectID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("project not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load project"))
		return
	}
	principal, err := auth.ActorPrincipalAllowSystem(actor)
	if err != nil {
		writeError(w, forbidden(err))
		return
	}
	orgID := pgvalue.UUID(actor.OrgID)
	targetProjectID := pgvalue.UUID(projectID)
	job, err := s.db.CreateDeletionJob(r.Context(), db.CreateDeletionJobParams{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                orgID,
		TargetType:           "project",
		TargetID:             targetProjectID,
		TargetProjectID:      pgtype.UUID{},
		TargetSlug:           project.Slug,
		TargetName:           project.Name,
		RequestedByPrincipal: principal,
	})
	if err != nil {
		writeError(w, errors.New("create deletion job"))
		return
	}
	if _, err := s.db.MarkDeletionJobRunning(r.Context(), db.MarkDeletionJobRunningParams{
		OrgID: orgID,
		ID:    job.ID,
	}); err != nil {
		s.failDeletionJob(r.Context(), orgID, job.ID, err)
		writeError(w, errors.New("mark deletion job running"))
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		projectsForPromotion, err := work.q.ListProjectsForUpdate(r.Context(), orgID)
		if err != nil {
			return errors.New("lock projects")
		}
		projectFound := false
		for _, candidate := range projectsForPromotion {
			if candidate.ID == targetProjectID {
				project = candidate
				projectFound = true
				break
			}
		}
		if !projectFound {
			return notFound(errors.New("project not found"))
		}
		promotedProjectID := pgtype.UUID{}
		if project.IsDefault {
			for _, candidate := range projectsForPromotion {
				if candidate.ID != project.ID {
					promotedProjectID = candidate.ID
					break
				}
			}
		}
		if _, err := work.q.DeleteProject(r.Context(), db.DeleteProjectParams{
			OrgID: orgID,
			ID:    targetProjectID,
		}); isNoRows(err) {
			return notFound(errors.New("project not found"))
		} else if err != nil {
			return errors.New("delete project")
		}
		if promotedProjectID != (pgtype.UUID{}) {
			if rows, err := work.q.SetDefaultProject(r.Context(), db.SetDefaultProjectParams{
				OrgID: orgID,
				ID:    promotedProjectID,
			}); err != nil {
				return errors.New("set default project")
			} else if rows == 0 {
				return errors.New("set default project affected no rows")
			}
		}
		if _, err := work.q.CompleteDeletionJob(r.Context(), db.CompleteDeletionJobParams{
			OrgID:         orgID,
			ID:            job.ID,
			DeletedCounts: json.RawMessage(`{"projects":1}`),
		}); err != nil {
			return errors.New("complete deletion job")
		}
		return nil
	})
	if err != nil {
		s.failDeletionJob(r.Context(), orgID, job.ID, err)
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("environment storage is not configured")))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.CreateEnvironmentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid environment request JSON: %w", err)))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	colorHex, err := normalizeEnvironmentColorHex(request.ColorHex)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	var environment db.Environment
	err = s.inTx(r.Context(), func(work *txWork) error {
		if _, err := work.q.GetProject(r.Context(), db.GetProjectParams{
			OrgID: pgvalue.UUID(actor.OrgID),
			ID:    pgvalue.UUID(projectID),
		}); isNoRows(err) {
			return notFound(errors.New("project not found"))
		} else if err != nil {
			return errors.New("load project")
		}
		var publicID string
		environment, err = createWithPublicID(r.Context(), []publicIDSlot{{prefix: publicid.Environment, value: &publicID}}, func() (db.Environment, error) {
			return work.q.CreateEnvironment(r.Context(), db.CreateEnvironmentParams{
				ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
				PublicID:        publicID,
				OrgID:           pgvalue.UUID(actor.OrgID),
				ProjectID:       pgvalue.UUID(projectID),
				DefaultRegionID: s.defaultRegionID,
				Slug:            slug,
				Name:            name,
				ColorHex:        colorHex,
				IsDefault:       false,
			})
		})
		if err != nil {
			if isUniqueViolation(err) {
				return badRequest(errors.New("environment slug is already in use"))
			}
			return errors.New("create environment")
		}
		if _, err := cell.EnsureEnvironmentRoute(r.Context(), work.q, cell.EnsureEnvironmentRouteParams{
			OrgID:         environment.OrgID,
			ProjectID:     environment.ProjectID,
			EnvironmentID: environment.ID,
			RegionID:      environment.DefaultRegionID,
			LocalCellID:   s.cellID,
		}); err != nil {
			if errors.Is(err, cell.ErrRouteUnavailable) || errors.Is(err, cell.ErrPlacementOutsideLocalCell) {
				return unavailable(errors.New("environment route is unavailable"))
			}
			return errors.New("create environment route")
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, environmentResponse(environmentRecordFromDB(environment)))
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("environment storage is not configured")))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	environmentID, err := parseUUIDParam(r, "environmentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	environment, err := s.db.GetEnvironment(r.Context(), db.GetEnvironmentParams{
		OrgID:     pgvalue.UUID(actor.OrgID),
		ProjectID: pgvalue.UUID(projectID),
		ID:        pgvalue.UUID(environmentID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("environment not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load environment"))
		return
	}
	writeJSON(w, http.StatusOK, environmentResponse(environmentRecordFromDB(environment)))
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("environment storage is not configured")))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	environmentID, err := parseUUIDParam(r, "environmentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.UpdateEnvironmentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid environment request JSON: %w", err)))
		return
	}
	slug, name, err := normalizeScopeCreateInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	colorHex, err := normalizeEnvironmentColorHex(request.ColorHex)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	current, err := s.db.GetEnvironment(r.Context(), db.GetEnvironmentParams{
		OrgID:     pgvalue.UUID(actor.OrgID),
		ProjectID: pgvalue.UUID(projectID),
		ID:        pgvalue.UUID(environmentID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("environment not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load environment"))
		return
	}
	if current.Slug != slug && (protectedEnvironmentSlug(current.Slug) || protectedEnvironmentSlug(slug)) {
		writeError(w, badRequest(errors.New("production and staging environment slugs cannot be renamed")))
		return
	}
	environment, err := s.db.UpdateEnvironmentDetails(r.Context(), db.UpdateEnvironmentDetailsParams{
		OrgID:     pgvalue.UUID(actor.OrgID),
		ProjectID: pgvalue.UUID(projectID),
		ID:        pgvalue.UUID(environmentID),
		Slug:      slug,
		Name:      name,
		ColorHex:  colorHex,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("environment not found")))
		return
	}
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, badRequest(errors.New("environment slug is already in use")))
			return
		}
		writeError(w, errors.New("update environment"))
		return
	}
	writeJSON(w, http.StatusOK, environmentResponse(environmentRecordFromDB(environment)))
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("environment storage is not configured")))
		return
	}
	projectID, err := parseUUIDParam(r, "projectID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	environmentID, err := parseUUIDParam(r, "environmentID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	environment, err := s.db.GetEnvironment(r.Context(), db.GetEnvironmentParams{
		OrgID:     pgvalue.UUID(actor.OrgID),
		ProjectID: pgvalue.UUID(projectID),
		ID:        pgvalue.UUID(environmentID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("environment not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load environment"))
		return
	}
	if protectedEnvironmentSlug(environment.Slug) {
		writeError(w, badRequest(errors.New("production and staging environments cannot be deleted")))
		return
	}
	principal, err := auth.ActorPrincipalAllowSystem(actor)
	if err != nil {
		writeError(w, forbidden(err))
		return
	}
	orgID := pgvalue.UUID(actor.OrgID)
	targetProjectID := pgvalue.UUID(projectID)
	targetEnvironmentID := pgvalue.UUID(environmentID)
	job, err := s.db.CreateDeletionJob(r.Context(), db.CreateDeletionJobParams{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                orgID,
		TargetType:           "environment",
		TargetID:             targetEnvironmentID,
		TargetProjectID:      targetProjectID,
		TargetSlug:           environment.Slug,
		TargetName:           environment.Name,
		RequestedByPrincipal: principal,
	})
	if err != nil {
		writeError(w, errors.New("create deletion job"))
		return
	}
	if _, err := s.db.MarkDeletionJobRunning(r.Context(), db.MarkDeletionJobRunningParams{
		OrgID: orgID,
		ID:    job.ID,
	}); err != nil {
		s.failDeletionJob(r.Context(), orgID, job.ID, err)
		writeError(w, errors.New("mark deletion job running"))
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		if _, err := work.q.DeleteEnvironment(r.Context(), db.DeleteEnvironmentParams{
			OrgID:     orgID,
			ProjectID: targetProjectID,
			ID:        targetEnvironmentID,
		}); isNoRows(err) {
			return notFound(errors.New("environment not found"))
		} else if err != nil {
			return errors.New("delete environment")
		}
		if _, err := work.q.CompleteDeletionJob(r.Context(), db.CompleteDeletionJobParams{
			OrgID:         orgID,
			ID:            job.ID,
			DeletedCounts: json.RawMessage(`{"environments":1}`),
		}); err != nil {
			return errors.New("complete deletion job")
		}
		return nil
	})
	if err != nil {
		s.failDeletionJob(r.Context(), orgID, job.ID, err)
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

func normalizeEnvironmentColorHex(colorHex string) (string, error) {
	normalized, err := api.NormalizeEnvironmentColorHex(colorHex)
	if err != nil {
		return "", errors.New("color_hex must be a #RRGGBB color")
	}
	return normalized, nil
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
	colorHex  string
	isDefault bool
	createdAt pgtype.Timestamptz
	updatedAt pgtype.Timestamptz
}

func projectResponse(project projectRecord) api.ProjectSummary {
	return api.ProjectSummary{
		ID:        pgvalue.MustUUIDValue(project.id).String(),
		Slug:      project.slug,
		Name:      project.name,
		IsDefault: project.isDefault,
		CreatedAt: pgvalue.Time(project.createdAt),
		UpdatedAt: pgvalue.Time(project.updatedAt),
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
		ID:        pgvalue.MustUUIDValue(environment.id).String(),
		ProjectID: pgvalue.MustUUIDValue(environment.projectID).String(),
		Slug:      environment.slug,
		Name:      environment.name,
		ColorHex:  environment.colorHex,
		IsDefault: environment.isDefault,
		CreatedAt: pgvalue.Time(environment.createdAt),
		UpdatedAt: pgvalue.Time(environment.updatedAt),
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
		colorHex:  environment.ColorHex,
		isDefault: environment.IsDefault,
		createdAt: environment.CreatedAt,
		updatedAt: environment.UpdatedAt,
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
