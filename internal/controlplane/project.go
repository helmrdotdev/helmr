package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

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

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	if principal.Role == "" {
		writeError(w, forbidden(errors.New("organization is required")))
		return
	}
	limit, cursor, err := parseProjectListQuery(r, principal.OrgID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var after *org.ProjectPosition
	if cursor != nil {
		after = &org.ProjectPosition{IsDefault: cursor.IsDefault, Slug: cursor.Slug, ID: uuid.MustParse(cursor.ID)}
	}
	projects, hasMore, err := org.ListProjects(r.Context(), s.db, principal.OrgID, limit, after)
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	response := api.ListProjectsResponse{Projects: make([]api.ProjectSummary, 0, len(projects))}
	for _, project := range projects {
		response.Projects = append(response.Projects, projectResponse(project))
	}
	if hasMore {
		last := projects[len(projects)-1]
		response.NextCursor, err = encodeProjectListCursor(projectListCursor{
			OrgID: principal.OrgID.String(), IsDefault: last.IsDefault,
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
	principal := principalFromContext(r.Context())
	project, err := org.GetProject(r.Context(), s.db, principal.OrgID, chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	environments, err := org.ListEnvironments(r.Context(), s.db, project)
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, projectResponseWithEnvironments(project, environments))
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
	var request api.CreateProjectRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid project request JSON: %w", err))
		return
	}
	principal := principalFromContext(r.Context())
	project, environments, err := org.CreateProject(r.Context(), s.tx, principal.OrgID, org.ProjectInput{
		ExecutionLimits: s.environmentExecutionLimits,
		ProjectDetails:  org.ProjectDetails{Slug: request.Project, Name: request.Name},
		DefaultRegionID: request.DefaultRegionID,
	})
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusCreated, projectResponseWithEnvironments(project, environments))
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	projectRecord, err := org.GetProject(r.Context(), s.db, principalFromContext(r.Context()).OrgID, chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	projectID := pgvalue.MustUUIDValue(projectRecord.ID)
	var request api.UpdateProjectRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid project request JSON: %w", err))
		return
	}
	principal := principalFromContext(r.Context())
	project, err := org.UpdateProject(r.Context(), s.db, principal.OrgID, projectID, org.ProjectDetails{
		Name: request.Name,
	})
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, projectResponse(project))
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request) {
	projectRecord, err := org.GetProject(r.Context(), s.db, principalFromContext(r.Context()).OrgID, chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	projectID := pgvalue.MustUUIDValue(projectRecord.ID)
	var request api.CreateEnvironmentRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid environment request JSON: %w", err))
		return
	}
	colorHex, err := normalizeEnvironmentColorHex(request.ColorHex)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	environment, err := org.CreateEnvironment(r.Context(), s.tx, principal.OrgID, projectID, org.EnvironmentDetails{
		Slug: request.Environment, Name: request.Name, ColorHex: colorHex,
	}, s.environmentExecutionLimits)
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusCreated, environmentResponse(environment, projectRecord.Slug))
}

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request) {
	projectRecord, err := org.GetProject(r.Context(), s.db, principalFromContext(r.Context()).OrgID, chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	projectID := pgvalue.MustUUIDValue(projectRecord.ID)
	environmentRecord, err := org.GetEnvironmentByReference(r.Context(), s.db, principalFromContext(r.Context()).OrgID, projectID, chi.URLParam(r, "environment"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, environmentResponse(environmentRecord, projectRecord.Slug))
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request) {
	projectRecord, err := org.GetProject(r.Context(), s.db, principalFromContext(r.Context()).OrgID, chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	projectID := pgvalue.MustUUIDValue(projectRecord.ID)
	environmentRecord, err := org.GetEnvironmentByReference(r.Context(), s.db, principalFromContext(r.Context()).OrgID, projectID, chi.URLParam(r, "environment"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	environmentID := pgvalue.MustUUIDValue(environmentRecord.ID)
	var request api.UpdateEnvironmentRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid environment request JSON: %w", err))
		return
	}
	colorHex, err := normalizeEnvironmentColorHex(request.ColorHex)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	principal := principalFromContext(r.Context())
	environment, err := org.UpdateEnvironment(r.Context(), s.db, principal.OrgID, projectID, environmentID, org.EnvironmentDetails{
		Name: request.Name, ColorHex: colorHex,
	})
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, environmentResponse(environment, projectRecord.Slug))
}

// normalizeEnvironmentColorHex applies the public #RRGGBB color grammar that
// the API contract shares with the CLI.
func normalizeEnvironmentColorHex(colorHex string) (string, error) {
	normalized, err := api.NormalizeEnvironmentColorHex(colorHex)
	if err != nil {
		return "", errors.New("color_hex must be a #RRGGBB color")
	}
	return normalized, nil
}

func projectResponse(project db.Project) api.ProjectSummary {
	return api.ProjectSummary{
		Project:         project.Slug,
		Name:            project.Name,
		DefaultRegionID: project.DefaultRegionID,
		IsDefault:       project.IsDefault,
		CreatedAt:       pgvalue.Time(project.CreatedAt),
		UpdatedAt:       pgvalue.Time(project.UpdatedAt),
	}
}

func projectResponseWithEnvironments(project db.Project, environments []db.Environment) api.ProjectSummary {
	response := projectResponse(project)
	response.Environments = make([]api.EnvironmentSummary, 0, len(environments))
	for _, environment := range environments {
		response.Environments = append(response.Environments, environmentResponse(environment, project.Slug))
	}
	return response
}

func environmentResponse(environment db.Environment, project string) api.EnvironmentSummary {
	return api.EnvironmentSummary{
		Environment: environment.Slug,
		Project:     project,
		Name:        environment.Name,
		ColorHex:    environment.ColorHex,
		IsDefault:   environment.IsDefault,
		CreatedAt:   pgvalue.Time(environment.CreatedAt),
		UpdatedAt:   pgvalue.Time(environment.UpdatedAt),
	}
}

func projectSettingsResponse(project db.Project) api.ProjectSettings {
	var seconds *int64
	if project.HistoryRetentionSeconds.Valid {
		seconds = &project.HistoryRetentionSeconds.Int64
	}
	return api.ProjectSettings{HistoryRetention: api.FormatHistoryRetention(seconds)}
}

func (s *Server) getProjectSettings(w http.ResponseWriter, r *http.Request) {
	project, err := org.GetProject(r.Context(), s.db, principalFromContext(r.Context()).OrgID, chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, projectSettingsResponse(project))
}

func (s *Server) updateProjectSettings(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	project, err := org.GetProject(r.Context(), s.db, principal.OrgID, chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	var request api.ProjectSettings
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid project settings JSON: %w", err))
		return
	}
	seconds, err := api.ParseHistoryRetention(request.HistoryRetention)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := org.UpdateProjectHistoryRetention(r.Context(), s.tx, audit.FromPrincipal(principal), principal.OrgID, pgvalue.MustUUIDValue(project.ID), seconds); err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, api.ProjectSettings{HistoryRetention: api.FormatHistoryRetention(seconds)})
}
