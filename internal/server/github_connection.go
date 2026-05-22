package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type githubInstallationConnector interface {
	InstallURL() string
	VerifyUserInstallation(ctx context.Context, userAccessToken string, installationID int64) (ghapp.VerifiedInstallation, error)
}

func (s *Server) listGitHubInstallations(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.githubConnector == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("github app is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	rows, err := s.db.ListGitHubInstallations(r.Context(), ids.ToPG(actor.OrgID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list github installations"))
		return
	}
	items := make([]api.GitHubInstallationSummary, 0, len(rows))
	for _, row := range rows {
		items = append(items, githubInstallationSummary(row))
	}
	writeJSON(w, http.StatusOK, api.GitHubInstallationsResponse{
		InstallURL:    s.githubConnector.InstallURL(),
		Installations: items,
	})
}

func (s *Server) listGitHubInstallationRepositories(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("github repository storage is not configured"))
		return
	}
	installationID, err := parseGitHubInstallationID(chi.URLParam(r, "installationID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	_, projectID, _, err := s.secretRequestScope(r.Context(), actor.OrgID, r.URL.Query().Get("project_id"), auth.DefaultEnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rows, err := s.db.ListGitHubInstallationRepositories(r.Context(), db.ListGitHubInstallationRepositoriesParams{
		OrgID:          ids.ToPG(actor.OrgID),
		InstallationID: installationID,
		ProjectID:      projectID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list github repositories"))
		return
	}
	repositories := make([]api.GitHubRepositorySummary, 0, len(rows))
	for _, row := range rows {
		repositories = append(repositories, githubRepositorySummary(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repositories})
}

func (s *Server) connectProjectGitHubRepository(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("github repository storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, _, err := s.secretRequestScope(r.Context(), actor.OrgID, chi.URLParam(r, "projectID"), auth.DefaultEnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionProjectsManage, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if !actor.HasPermission(auth.PermissionGitHubManage, auth.DefaultScope(actor.OrgID)) {
		writeError(w, http.StatusForbidden, errors.New("github management permission is required"))
		return
	}
	row, status, err := s.githubRepositoryFromRequest(r.Context(), actor, chi.URLParam(r, "githubRepositoryID"))
	if err != nil {
		writeError(w, status, err)
		return
	}
	connectedByUserID := pgtype.UUID{}
	if actor.UserID != uuidNil {
		connectedByUserID = ids.ToPG(actor.UserID)
	}
	projectRepository, err := s.db.ConnectProjectGitHubRepository(r.Context(), db.ConnectProjectGitHubRepositoryParams{
		ID:                 ids.ToPG(ids.New()),
		OrgID:              ids.ToPG(actor.OrgID),
		ProjectID:          projectID,
		GithubRepositoryID: row.GithubRepositoryID,
		ConnectedByUserID:  connectedByUserID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusBadRequest, errors.New("github repository must be installed before it can be connected to a project"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("connect project github repository"))
		return
	}
	summary := githubRepositoryAccessTargetSummary(row)
	summary.ProjectGitHubRepository = &api.GitHubProjectRepositoryStatus{
		ProjectID: ids.MustFromPG(projectRepository.ProjectID).String(),
		Connected: true,
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) disconnectProjectGitHubRepository(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("github repository storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, _, err := s.secretRequestScope(r.Context(), actor.OrgID, chi.URLParam(r, "projectID"), auth.DefaultEnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionProjectsManage, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if !actor.HasPermission(auth.PermissionGitHubManage, auth.DefaultScope(actor.OrgID)) {
		writeError(w, http.StatusForbidden, errors.New("github management permission is required"))
		return
	}
	row, status, err := s.githubRepositoryFromRequest(r.Context(), actor, chi.URLParam(r, "githubRepositoryID"))
	if err != nil {
		writeError(w, status, err)
		return
	}
	projectRepository, err := s.db.DisconnectProjectGitHubRepository(r.Context(), db.DisconnectProjectGitHubRepositoryParams{
		OrgID:              ids.ToPG(actor.OrgID),
		ProjectID:          projectID,
		GithubRepositoryID: row.GithubRepositoryID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("project github repository not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("disconnect project github repository"))
		return
	}
	summary := githubRepositoryAccessTargetSummary(row)
	summary.ProjectGitHubRepository = &api.GitHubProjectRepositoryStatus{
		ProjectID: ids.MustFromPG(projectRepository.ProjectID).String(),
		Connected: false,
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) githubSetupStart(w http.ResponseWriter, r *http.Request) {
	if s.githubConnector == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("github app is not configured"))
		return
	}
	if err := s.userAuthConfigured(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	if s.authProvider == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("auth provider is not configured"))
		return
	}
	if _, ok := s.authProvider.(tokenAuthProvider); !ok {
		writeError(w, http.StatusServiceUnavailable, errors.New("github oauth token exchange is not configured"))
		return
	}
	var request api.GitHubSetupStartRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid github setup request JSON: %w", err))
		return
	}
	installationID, err := parseGitHubInstallationID(request.InstallationID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	state, err := auth.GenerateOpaqueToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate auth state"))
		return
	}
	verifier, err := auth.GenerateOpaqueToken(64)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate pkce verifier"))
		return
	}
	flow := browserAuthFlow{
		Kind:           browserAuthGitHubAppSetup,
		State:          state,
		Verifier:       verifier,
		RedirectAfter:  githubSetupRedirectAfter(request.SetupAction),
		InstallationID: installationID,
		SetupAction:    sanitizeGitHubSetupAction(request.SetupAction),
	}
	encoded, err := s.encodeAuthFlow(flow)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode auth flow"))
		return
	}
	http.SetCookie(w, authFlowCookie(r, encoded, int(authFlowTTL.Seconds())))
	w.Header().Set("referrer-policy", "no-referrer")
	writeJSON(w, http.StatusOK, api.GitHubAuthStartResponse{RedirectURL: s.authProvider.RedirectURL(state, verifier)})
}

func (s *Server) completeGitHubSetupAuth(r *http.Request, flow browserAuthFlow, userAccessToken string) (string, error) {
	if flow.InstallationID <= 0 {
		return "", errInvalidGitHubInstallation
	}
	if s.githubConnector == nil {
		return "", errors.New("github app is not configured")
	}
	actor, rawSession, err := s.sessionActor(r)
	if err != nil {
		return "", err
	}
	if !actor.HasPermission(auth.PermissionGitHubManage, auth.DefaultScope(actor.OrgID)) {
		return "", errOwnerAccessRequired
	}
	verified, err := s.githubConnector.VerifyUserInstallation(r.Context(), userAccessToken, flow.InstallationID)
	if err != nil {
		if ghapp.IsInvalidSource(err) {
			return "", fmt.Errorf("%w: %v", errInvalidGitHubInstallation, err)
		}
		return "", err
	}
	existing, err := s.db.GetKnownGitHubInstallationByInstallationID(r.Context(), verified.InstallationID)
	if err == nil {
		existingOrgID, err := ids.FromPG(existing.OrgID)
		if err != nil {
			return "", err
		}
		if existingOrgID != actor.OrgID {
			return "", errGitHubInstallationAlreadyConnected
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	installation, err := s.db.UpsertGitHubInstallation(r.Context(), db.UpsertGitHubInstallationParams{
		ID:                  ids.ToPG(ids.New()),
		OrgID:               ids.ToPG(actor.OrgID),
		InstallationID:      verified.InstallationID,
		AccountLogin:        verified.AccountLogin,
		AccountType:         verified.AccountType,
		RepositorySelection: nullableText(verified.RepositorySelection),
		HtmlUrl:             nullableText(verified.HTMLURL),
	})
	if err != nil {
		return "", err
	}
	if err := s.upsertGitHubRepositories(r.Context(), installation.OrgID, installation.InstallationID, verified.Repositories); err != nil {
		return "", err
	}
	if verified.Suspended {
		if _, err := s.db.SuspendGitHubInstallation(r.Context(), db.SuspendGitHubInstallationParams{
			OrgID:          installation.OrgID,
			InstallationID: installation.InstallationID,
		}); err != nil {
			return "", err
		}
	}
	return rawSession, nil
}

func (s *Server) upsertGitHubRepositories(ctx context.Context, orgID pgtype.UUID, installationID int64, repositories []ghapp.Repository) error {
	for _, repository := range repositories {
		if repository.ID <= 0 {
			continue
		}
		_, err := s.db.UpsertGitHubRepository(ctx, db.UpsertGitHubRepositoryParams{
			ID:                 ids.ToPG(ids.New()),
			OrgID:              orgID,
			InstallationID:     installationID,
			GithubRepositoryID: repository.ID,
			OwnerLogin:         strings.TrimSpace(repository.OwnerLogin),
			Name:               strings.TrimSpace(repository.Name),
			FullName:           strings.TrimSpace(repository.FullName),
			Private:            repository.Private,
			Archived:           repository.Archived,
			DefaultBranch:      nullableText(repository.DefaultBranch),
			HtmlUrl:            nullableText(repository.HTMLURL),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func githubInstallationSummary(row db.GitHubAppInstallation) api.GitHubInstallationSummary {
	status := "active"
	if row.DeletedAt.Valid {
		status = "deleted"
	} else if row.SuspendedAt.Valid {
		status = "suspended"
	}
	return api.GitHubInstallationSummary{
		InstallationID:      strconv.FormatInt(row.InstallationID, 10),
		AccountLogin:        row.AccountLogin,
		AccountType:         row.AccountType,
		RepositorySelection: row.RepositorySelection.String,
		Status:              status,
		HTMLURL:             row.HtmlUrl.String,
		CreatedAt:           row.CreatedAt.Time.Format(time.RFC3339Nano),
		UpdatedAt:           row.UpdatedAt.Time.Format(time.RFC3339Nano),
	}
}

func githubRepositorySummary(row db.ListGitHubInstallationRepositoriesRow) api.GitHubRepositorySummary {
	status := "active"
	if row.RepositoryDeletedAt.Valid {
		status = "removed"
	}
	summary := api.GitHubRepositorySummary{
		GitHubRepositoryID: strconv.FormatInt(row.GithubRepositoryID, 10),
		InstallationID:     strconv.FormatInt(row.InstallationID, 10),
		FullName:           row.FullName,
		OwnerLogin:         row.OwnerLogin,
		Name:               row.RepositoryName,
		Private:            row.Private,
		Archived:           row.Archived,
		DefaultBranch:      row.DefaultBranch.String,
		Status:             status,
		HTMLURL:            row.RepositoryHtmlUrl.String,
		UpdatedAt:          row.RepositoryUpdatedAt.Time.Format(time.RFC3339Nano),
	}
	if row.ProjectGithubRepositoryID.Valid {
		summary.ProjectGitHubRepository = &api.GitHubProjectRepositoryStatus{
			ProjectID: ids.MustFromPG(row.ProjectGithubRepositoryProjectID).String(),
			Connected: true,
		}
	}
	return summary
}

func githubRepositoryAccessTargetSummary(row db.GetActiveGitHubRepositoryTargetRow) api.GitHubRepositorySummary {
	status := "active"
	if row.RepositoryDeletedAt.Valid {
		status = "removed"
	}
	return api.GitHubRepositorySummary{
		GitHubRepositoryID: strconv.FormatInt(row.GithubRepositoryID, 10),
		InstallationID:     strconv.FormatInt(row.InstallationID, 10),
		FullName:           row.FullName,
		OwnerLogin:         row.OwnerLogin,
		Name:               row.RepositoryName,
		Private:            row.Private,
		Archived:           row.Archived,
		DefaultBranch:      row.DefaultBranch.String,
		Status:             status,
		HTMLURL:            row.RepositoryHtmlUrl.String,
		UpdatedAt:          row.RepositoryUpdatedAt.Time.Format(time.RFC3339Nano),
	}
}

func (s *Server) githubRepositoryFromRequest(ctx context.Context, actor auth.Actor, githubRepositoryIDValue string) (db.GetActiveGitHubRepositoryTargetRow, int, error) {
	githubRepositoryIDValue = strings.TrimSpace(githubRepositoryIDValue)
	if githubRepositoryIDValue == "" {
		return db.GetActiveGitHubRepositoryTargetRow{}, http.StatusBadRequest, errors.New("github_repository_id is required")
	}
	githubRepositoryID, err := parseGitHubRepositoryID(githubRepositoryIDValue)
	if err != nil {
		return db.GetActiveGitHubRepositoryTargetRow{}, http.StatusBadRequest, err
	}
	row, err := s.db.GetActiveGitHubRepositoryTarget(ctx, db.GetActiveGitHubRepositoryTargetParams{
		OrgID:              ids.ToPG(actor.OrgID),
		GithubRepositoryID: githubRepositoryID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetActiveGitHubRepositoryTargetRow{}, http.StatusNotFound, errors.New("github repository not found")
	}
	if err != nil {
		return db.GetActiveGitHubRepositoryTargetRow{}, http.StatusInternalServerError, errors.New("load github repository")
	}
	return row, http.StatusOK, nil
}

func parseGitHubInstallationID(value string) (int64, error) {
	installationID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || installationID <= 0 {
		return 0, errInvalidGitHubInstallation
	}
	return installationID, nil
}

func parseGitHubRepositoryID(value string) (int64, error) {
	githubRepositoryID, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || githubRepositoryID <= 0 {
		return 0, errors.New("invalid github repository")
	}
	return githubRepositoryID, nil
}

func sanitizeGitHubSetupAction(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	return value
}

func githubSetupRedirectAfter(action string) string {
	switch sanitizeGitHubSetupAction(action) {
	case "onboarding":
		return "/github/connect/repositories"
	default:
		return "/settings/github"
	}
}

func nullableText(value string) pgtype.Text {
	value = strings.TrimSpace(value)
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

var errInvalidGitHubInstallation = errors.New("invalid github installation")
