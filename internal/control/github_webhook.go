package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const githubWebhookMaxBodyBytes = 1 << 20

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if len(s.githubWebhookSecret) == 0 {
		writeError(w, http.StatusServiceUnavailable, errors.New("github webhook secret is not configured"))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, githubWebhookMaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("github webhook body is too large"))
		return
	}
	if !verifyGitHubSignature(s.githubWebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		writeError(w, http.StatusForbidden, errors.New("github webhook signature mismatch"))
		return
	}

	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	switch event {
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case "installation":
		if err := s.handleGitHubInstallationWebhook(r, body); err != nil {
			s.writeGitHubWebhookError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case "installation_repositories":
		if err := s.handleGitHubInstallationRepositoriesWebhook(r, body); err != nil {
			s.writeGitHubWebhookError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored"})
	}
}

func verifyGitHubSignature(secret []byte, body []byte, header string) bool {
	signature, ok := strings.CutPrefix(strings.TrimSpace(header), "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func (s *Server) handleGitHubInstallationWebhook(r *http.Request, body []byte) error {
	var payload githubInstallationPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("%w: installation JSON: %v", errInvalidGitHubWebhook, err)
	}
	if err := payload.Installation.validate(); err != nil {
		return err
	}
	switch payload.Action {
	case "created":
		return nil
	case "new_permissions_accepted", "unsuspend":
		installation, err := s.refreshKnownGitHubInstallation(r, payload.Installation)
		if err != nil {
			return err
		}
		return s.upsertGitHubWebhookRepositories(r.Context(), installation.OrgID, installation.InstallationID, payload.Repositories)
	case "suspend":
		_, err := s.db.SuspendGitHubInstallationByInstallationID(r.Context(), payload.Installation.ID)
		return err
	case "deleted":
		if _, err := s.db.MarkGitHubRepositoriesDeletedByInstallationID(r.Context(), payload.Installation.ID); err != nil {
			return err
		}
		_, err := s.db.DeleteGitHubInstallationByInstallationID(r.Context(), payload.Installation.ID)
		return err
	default:
		return nil
	}
}

func (s *Server) handleGitHubInstallationRepositoriesWebhook(r *http.Request, body []byte) error {
	var payload githubInstallationRepositoriesPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("%w: installation_repositories JSON: %v", errInvalidGitHubWebhook, err)
	}
	if err := payload.Installation.validate(); err != nil {
		return err
	}
	switch payload.Action {
	case "added":
		installation, err := s.refreshKnownGitHubInstallation(r, payload.Installation)
		if err != nil {
			return err
		}
		return s.upsertGitHubWebhookRepositories(r.Context(), installation.OrgID, installation.InstallationID, payload.RepositoriesAdded)
	case "removed":
		installation, err := s.refreshKnownGitHubInstallation(r, payload.Installation)
		if err != nil {
			return err
		}
		return s.markGitHubWebhookRepositoriesDeleted(r.Context(), installation.OrgID, installation.InstallationID, payload.RepositoriesRemoved)
	default:
		return nil
	}
}

func (s *Server) refreshKnownGitHubInstallation(r *http.Request, installation githubWebhookInstallation) (db.GitHubAppInstallation, error) {
	row, err := s.db.GetKnownGitHubInstallationByInstallationID(r.Context(), installation.ID)
	if err != nil {
		return db.GitHubAppInstallation{}, ignoreMissingInstallation(err)
	}
	updated, err := s.db.UpsertGitHubInstallation(r.Context(), db.UpsertGitHubInstallationParams{
		ID:                  row.ID,
		OrgID:               row.OrgID,
		InstallationID:      installation.ID,
		AccountLogin:        installation.Account.Login,
		AccountType:         installation.Account.Type,
		RepositorySelection: pgtype.Text{String: installation.RepositorySelection, Valid: installation.RepositorySelection != ""},
		HtmlUrl:             row.HtmlUrl,
	})
	return updated, err
}

func (s *Server) upsertGitHubWebhookRepositories(ctx context.Context, orgID pgtype.UUID, installationID int64, repositories []githubWebhookRepository) error {
	for _, repository := range repositories {
		normalized, err := repository.toRepository()
		if err != nil {
			return err
		}
		if err := s.upsertGitHubRepositories(ctx, orgID, installationID, []ghapp.Repository{normalized}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) markGitHubWebhookRepositoriesDeleted(ctx context.Context, orgID pgtype.UUID, installationID int64, repositories []githubWebhookRepository) error {
	for _, repository := range repositories {
		if repository.ID <= 0 {
			return fmt.Errorf("%w: repository.id is required", errInvalidGitHubWebhook)
		}
		if _, err := s.db.MarkGitHubRepositoryDeleted(ctx, db.MarkGitHubRepositoryDeletedParams{
			OrgID:              orgID,
			InstallationID:     installationID,
			GithubRepositoryID: repository.ID,
		}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}

func (s *Server) writeGitHubWebhookError(w http.ResponseWriter, err error) {
	if errors.Is(err, errInvalidGitHubWebhook) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.log.Error("github webhook failed", "error", err)
	writeError(w, http.StatusInternalServerError, errors.New("github webhook"))
}

func ignoreMissingInstallation(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

type githubInstallationPayload struct {
	Action       string                    `json:"action"`
	Installation githubWebhookInstallation `json:"installation"`
	Repositories []githubWebhookRepository `json:"repositories"`
}

type githubInstallationRepositoriesPayload struct {
	Action              string                    `json:"action"`
	Installation        githubWebhookInstallation `json:"installation"`
	RepositoriesAdded   []githubWebhookRepository `json:"repositories_added"`
	RepositoriesRemoved []githubWebhookRepository `json:"repositories_removed"`
}

type githubWebhookInstallation struct {
	ID                  int64                `json:"id"`
	Account             githubWebhookAccount `json:"account"`
	RepositorySelection string               `json:"repository_selection"`
}

func (i githubWebhookInstallation) validate() error {
	if i.ID <= 0 {
		return fmt.Errorf("%w: installation.id is required", errInvalidGitHubWebhook)
	}
	if strings.TrimSpace(i.Account.Login) == "" {
		return fmt.Errorf("%w: installation.account.login is required", errInvalidGitHubWebhook)
	}
	if strings.TrimSpace(i.Account.Type) == "" {
		return fmt.Errorf("%w: installation.account.type is required", errInvalidGitHubWebhook)
	}
	return nil
}

type githubWebhookAccount struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type githubWebhookRepository struct {
	ID            int64                `json:"id"`
	Name          string               `json:"name"`
	FullName      string               `json:"full_name"`
	Private       bool                 `json:"private"`
	Archived      bool                 `json:"archived"`
	DefaultBranch string               `json:"default_branch"`
	HTMLURL       string               `json:"html_url"`
	Owner         githubWebhookAccount `json:"owner"`
}

func (r githubWebhookRepository) toRepository() (ghapp.Repository, error) {
	if r.ID <= 0 {
		return ghapp.Repository{}, fmt.Errorf("%w: repository.id is required", errInvalidGitHubWebhook)
	}
	fullName := strings.TrimSpace(r.FullName)
	ownerLogin := strings.TrimSpace(r.Owner.Login)
	name := strings.TrimSpace(r.Name)
	if fullName == "" && ownerLogin != "" && name != "" {
		fullName = ownerLogin + "/" + name
	}
	if ownerLogin == "" {
		ownerLogin, _, _ = strings.Cut(fullName, "/")
	}
	if name == "" {
		_, name, _ = strings.Cut(fullName, "/")
	}
	if ownerLogin == "" || name == "" || fullName == "" {
		return ghapp.Repository{}, fmt.Errorf("%w: repository.full_name is required", errInvalidGitHubWebhook)
	}
	return ghapp.Repository{
		ID:            r.ID,
		OwnerLogin:    ownerLogin,
		Name:          name,
		FullName:      fullName,
		Private:       r.Private,
		Archived:      r.Archived,
		DefaultBranch: strings.TrimSpace(r.DefaultBranch),
		HTMLURL:       strings.TrimSpace(r.HTMLURL),
	}, nil
}

var errInvalidGitHubWebhook = errors.New("invalid github webhook")
