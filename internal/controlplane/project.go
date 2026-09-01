package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/region"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

var scopeSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

const (
	projectListDefaultLimit = int32(50)
	projectListMaxLimit     = int32(100)
)

type projectListCursor struct {
	OrgID     string `json:"org_id"`
	IsDefault bool   `json:"is_default"`
	Slug      string `json:"slug"`
	ID        string `json:"id"`
}

func protectedEnvironmentSlug(slug string) bool {
	return slug == "production" || slug == "staging"
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
	limit, cursor, err := parseProjectListQuery(r, actor.OrgID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	params := db.ListProjectsParams{
		OrgID: pgvalue.UUID(actor.OrgID), RowLimit: limit + 1,
	}
	if cursor != nil {
		params.HasAfter = true
		params.AfterIsDefault = cursor.IsDefault
		params.AfterSlug = cursor.Slug
		params.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	projects, err := s.db.ListProjects(r.Context(), params)
	if err != nil {
		writeError(w, errors.New("list projects"))
		return
	}
	hasMore := len(projects) > int(limit)
	if hasMore {
		projects = projects[:limit]
	}
	response := api.ListProjectsResponse{Projects: make([]api.ProjectSummary, 0, len(projects))}
	for _, project := range projects {
		response.Projects = append(response.Projects, projectResponse(projectRecordFromDB(project)))
	}
	if hasMore {
		last := projects[len(projects)-1]
		response.NextCursor, err = encodeProjectListCursor(projectListCursor{
			OrgID: actor.OrgID.String(), IsDefault: last.IsDefault,
			Slug: last.Slug, ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			writeError(w, errors.New("list projects"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("project storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	ref := strings.TrimSpace(chi.URLParam(r, "projectRef"))
	projectID, idErr := ids.Parse(ref)
	var project db.Project
	var err error
	if idErr == nil {
		project, err = s.db.GetProject(r.Context(), db.GetProjectParams{
			OrgID: pgvalue.UUID(actor.OrgID), ID: pgvalue.UUID(projectID),
		})
	} else {
		slug := strings.ToLower(ref)
		if !scopeSlugPattern.MatchString(slug) {
			writeError(w, badRequest(errors.New("invalid project reference")))
			return
		}
		project, err = s.db.GetProjectBySlug(r.Context(), db.GetProjectBySlugParams{
			OrgID: pgvalue.UUID(actor.OrgID), Slug: slug,
		})
	}
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

func parseProjectListQuery(r *http.Request, orgID uuid.UUID) (int32, *projectListCursor, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "cursor" && name != "limit" {
			return 0, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	limit := projectListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(projectListMaxLimit) {
			return 0, nil, errors.New("limit must be an integer in [1,100]")
		}
		limit = int32(parsed)
	}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeProjectListCursor(raw)
		if err != nil {
			return 0, nil, err
		}
		if cursor.OrgID != orgID.String() {
			return 0, nil, errors.New("project cursor belongs to another organization")
		}
		return limit, &cursor, nil
	}
	return limit, nil, nil
}

func encodeProjectListCursor(cursor projectListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeProjectListCursor(raw string) (projectListCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return projectListCursor{}, errors.New("project cursor is malformed")
	}
	var cursor projectListCursor
	if err := json.Unmarshal(payload, &cursor); err != nil || ids.Validate(cursor.OrgID) != nil ||
		cursor.Slug == "" || ids.Validate(cursor.ID) != nil {
		return projectListCursor{}, errors.New("project cursor is malformed")
	}
	return cursor, nil
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
	slug, name, err := normalizeProjectInput(request.Slug, request.Name)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	defaultRegionID := request.DefaultRegionID
	if defaultRegionID == "" {
		regions, err := s.db.ListRegions(r.Context())
		if err != nil {
			writeError(w, errors.New("list regions"))
			return
		}
		if len(regions) == 0 {
			writeError(w, badRequest(errors.New("no region configured")))
			return
		}
		defaultRegionID = regions[0].ID
	}
	if err := region.ValidateID(defaultRegionID); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid default_region_id: %w", err)))
		return
	}
	var project db.CreateProjectWithDefaultEnvironmentRow
	var environments []db.Environment
	err = s.inTx(r.Context(), func(work *txWork) error {
		if _, err := work.q.LockOrganizationForProjectDefaults(r.Context(), pgvalue.UUID(actor.OrgID)); err != nil {
			return errors.New("lock organization")
		}
		region, err := work.q.GetRegion(r.Context(), defaultRegionID)
		if isNoRows(err) {
			return badRequest(errors.New("default region not found"))
		}
		if err != nil {
			return errors.New("load default region")
		}
		project, err = work.q.CreateProjectWithDefaultEnvironment(r.Context(), db.CreateProjectWithDefaultEnvironmentParams{
			ID:                   pgvalue.UUID(uuid.NewV7()),
			OrgID:                pgvalue.UUID(actor.OrgID),
			DefaultRegionID:      region.ID,
			Slug:                 slug,
			Name:                 name,
			IsDefault:            false,
			EnvironmentID:        pgvalue.UUID(uuid.NewV7()),
			StagingEnvironmentID: pgvalue.UUID(uuid.NewV7()),
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
	slug, name, err := normalizeProjectInput(request.Slug, request.Name)
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
	orgID := pgvalue.UUID(actor.OrgID)
	targetProjectID := pgvalue.UUID(projectID)
	err = s.inTx(r.Context(), func(work *txWork) error {
		if _, err := work.q.LockOrganizationForProjectDefaults(r.Context(), orgID); err != nil {
			return errors.New("lock organization")
		}
		project, err := work.q.DeleteProject(r.Context(), db.DeleteProjectParams{
			OrgID: orgID,
			ID:    targetProjectID,
		})
		if isNoRows(err) {
			return notFound(errors.New("project not found"))
		} else if err != nil {
			return errors.New("delete project")
		}
		if project.IsDefault {
			if _, err := work.q.PromoteFirstProjectDefault(r.Context(), orgID); err != nil {
				return errors.New("set default project")
			}
		}
		return nil
	})
	if err != nil {
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
		_, err := work.q.GetProject(r.Context(), db.GetProjectParams{
			OrgID: pgvalue.UUID(actor.OrgID),
			ID:    pgvalue.UUID(projectID),
		})
		if isNoRows(err) {
			return notFound(errors.New("project not found"))
		} else if err != nil {
			return errors.New("load project")
		}
		environment, err = work.q.CreateEnvironment(r.Context(), db.CreateEnvironmentParams{
			ID:        pgvalue.UUID(uuid.NewV7()),
			OrgID:     pgvalue.UUID(actor.OrgID),
			ProjectID: pgvalue.UUID(projectID),
			Slug:      slug,
			Name:      name,
			ColorHex:  colorHex,
			IsDefault: false,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return badRequest(errors.New("environment slug is already in use"))
			}
			return errors.New("create environment")
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
	orgID := pgvalue.UUID(actor.OrgID)
	targetProjectID := pgvalue.UUID(projectID)
	targetEnvironmentID := pgvalue.UUID(environmentID)
	if _, err := s.db.DeleteEnvironment(r.Context(), db.DeleteEnvironmentParams{
		OrgID:     orgID,
		ProjectID: targetProjectID,
		ID:        targetEnvironmentID,
	}); isNoRows(err) {
		writeError(w, notFound(errors.New("environment not found")))
		return
	} else if err != nil {
		writeError(w, errors.New("delete environment"))
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

func normalizeProjectInput(slug string, name string) (string, string, error) {
	slug, name, err := normalizeScopeCreateInput(slug, name)
	if err != nil {
		return "", "", err
	}
	if _, err := ids.Parse(slug); err == nil {
		return "", "", errors.New("project slug must not be a UUID")
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
	id              pgtype.UUID
	orgID           pgtype.UUID
	defaultRegionID string
	slug            string
	name            string
	isDefault       bool
	createdAt       pgtype.Timestamptz
	updatedAt       pgtype.Timestamptz
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
		ID:              pgvalue.MustUUIDValue(project.id).String(),
		Slug:            project.slug,
		Name:            project.name,
		DefaultRegionID: project.defaultRegionID,
		IsDefault:       project.isDefault,
		CreatedAt:       pgvalue.Time(project.createdAt),
		UpdatedAt:       pgvalue.Time(project.updatedAt),
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
		id:              project.ID,
		orgID:           project.OrgID,
		defaultRegionID: project.DefaultRegionID,
		slug:            project.Slug,
		name:            project.Name,
		isDefault:       project.IsDefault,
		createdAt:       project.CreatedAt,
		updatedAt:       project.UpdatedAt,
	}
}

func projectRecordFromCreated(project db.CreateProjectWithDefaultEnvironmentRow) projectRecord {
	return projectRecord{
		id:              project.ID,
		orgID:           project.OrgID,
		defaultRegionID: project.DefaultRegionID,
		slug:            project.Slug,
		name:            project.Name,
		isDefault:       project.IsDefault,
		createdAt:       project.CreatedAt,
		updatedAt:       project.UpdatedAt,
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
