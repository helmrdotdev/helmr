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
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
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
	if principal.MemberID == uuid.Nil() {
		writeError(w, forbidden(errors.New("organization is required")))
		return
	}
	limit, cursor, err := parseProjectListQuery(r, principal.OrgID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	params := db.ListAuthorizedProjectsParams{OrgID: pgvalue.UUID(principal.OrgID), RowLimit: limit + 1, AllProjects: directoryAdministrator(principal), ProjectIds: []pgtype.UUID{}}
	for _, grant := range principal.Grants {
		if grant.ProjectID != "" {
			id, e := uuid.Parse(grant.ProjectID)
			if e == nil {
				params.ProjectIds = append(params.ProjectIds, pgvalue.UUID(id))
			}
		}
	}
	if cursor != nil {
		params.HasAfter = true
		params.AfterIsDefault = cursor.IsDefault
		params.AfterSlug = cursor.Slug
		params.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	projects, err := s.db.ListAuthorizedProjects(r.Context(), params)
	hasMore := len(projects) > int(limit)
	if hasMore {
		projects = projects[:limit]
	}
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	response := api.ListProjectsResponse{Projects: make([]api.ProjectSummary, 0, len(projects))}
	for _, project := range projects {
		response.Projects = append(response.Projects, visibleProjectResponse(principal, project))
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
	if !directoryVisible(principal, pgvalue.UUIDString(project.ID), "") {
		writeError(w, notFound(org.ErrProjectNotFound))
		return
	}
	environments, err := org.ListEnvironments(r.Context(), s.db, project)
	if err != nil {
		writeError(w, orgError(err))
		return
	}
	response := visibleProjectResponse(principal, project)
	response.Environments = []api.EnvironmentSummary{}
	for _, environment := range environments {
		if directoryVisible(principal, pgvalue.UUIDString(project.ID), pgvalue.UUIDString(environment.ID)) {
			response.Environments = append(response.Environments, visibleEnvironmentResponse(principal, environment, project.Slug))
		}
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
	var request api.CreateProjectRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid project request JSON: %w", err))
		return
	}
	principal := principalFromContext(r.Context())
	project, environments, err := org.CreateProject(r.Context(), s.tx, managingMember(principal), org.ProjectInput{
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
	project, err := org.UpdateProject(r.Context(), s.tx, managingMember(principal), projectID, org.ProjectDetails{
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
	environment, err := org.CreateEnvironment(r.Context(), s.tx, managingMember(principal), projectID, org.EnvironmentDetails{
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
	principal := principalFromContext(r.Context())
	if !directoryVisible(principal, projectID.String(), pgvalue.UUIDString(environmentRecord.ID)) {
		writeError(w, notFound(org.ErrEnvironmentNotFound))
		return
	}
	writeJSON(w, http.StatusOK, visibleEnvironmentResponse(principal, environmentRecord, projectRecord.Slug))
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
	environment, err := org.UpdateEnvironment(r.Context(), s.tx, managingMember(principal), projectID, environmentID, org.EnvironmentDetails{
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
		ID: pgvalue.UUIDString(project.ID), Project: project.Slug,
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
		ID: pgvalue.UUIDString(environment.ID), Environment: environment.Slug,
		Project:   project,
		Name:      environment.Name,
		ColorHex:  environment.ColorHex,
		IsDefault: environment.IsDefault,
		CreatedAt: pgvalue.Time(environment.CreatedAt),
		UpdatedAt: pgvalue.Time(environment.UpdatedAt),
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
	p := principalFromContext(r.Context())
	if !p.HasPermission(auth.PermissionProjectsRead, auth.Scope{OrgID: p.OrgID}) {
		writeError(w, notFound(org.ErrProjectNotFound))
		return
	}
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
	if err := org.UpdateProjectHistoryRetention(r.Context(), s.tx, managingMember(principal), pgvalue.MustUUIDValue(project.ID), seconds); err != nil {
		writeError(w, orgError(err))
		return
	}
	writeJSON(w, http.StatusOK, api.ProjectSettings{HistoryRetention: api.FormatHistoryRetention(seconds)})
}

func directoryAdministrator(p auth.Principal) bool {
	scope := auth.Scope{OrgID: p.OrgID}
	return p.HasPermission(auth.PermissionAssignmentsRead, scope) || p.HasPermission(auth.PermissionProjectsRead, scope) || p.HasPermission(auth.PermissionEnvironmentsRead, scope)
}
func directoryVisible(p auth.Principal, project, environment string) bool {
	if directoryAdministrator(p) {
		return true
	}
	for _, g := range p.Grants {
		if g.ProjectID == project && (environment == "" || g.EnvironmentID == environment) {
			return true
		}
	}
	return false
}
func visibleProjectResponse(p auth.Principal, project db.Project) api.ProjectSummary {
	if p.HasPermission(auth.PermissionProjectsRead, auth.Scope{OrgID: p.OrgID}) {
		return projectResponse(project)
	}
	return api.ProjectSummary{ID: pgvalue.UUIDString(project.ID), Project: project.Slug, Name: project.Name}
}
func visibleEnvironmentResponse(p auth.Principal, environment db.Environment, project string) api.EnvironmentSummary {
	if p.HasPermission(auth.PermissionEnvironmentsRead, auth.Scope{OrgID: p.OrgID}) {
		return environmentResponse(environment, project)
	}
	return api.EnvironmentSummary{ID: pgvalue.UUIDString(environment.ID), Project: project, Environment: environment.Slug, Name: environment.Name}
}
