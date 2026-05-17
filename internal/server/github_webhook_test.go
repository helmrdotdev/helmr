package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

const testWebhookSecret = "webhook-secret"

func TestGitHubWebhookRejectsInvalidSignature(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithGitHubWebhookSecret(testWebhookSecret))
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewBufferString(`{"zen":"ok"}`))
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", "sha256=bad")
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestVerifyGitHubSignatureMatchesGitHubTestVector(t *testing.T) {
	ok := verifyGitHubSignature(
		[]byte("It's a Secret to Everybody"),
		[]byte("Hello, World!"),
		"sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17",
	)
	if !ok {
		t.Fatal("signature did not match GitHub test vector")
	}
}

func TestGitHubWebhookPing(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithGitHubWebhookSecret(testWebhookSecret))
	body := []byte(`{"zen":"ok"}`)
	req := signedGitHubWebhook(http.MethodPost, "/webhooks/github", "ping", body)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGitHubWebhookCreatedInstallationDoesNotBindOrg(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithGitHubWebhookSecret(testWebhookSecret))
	body := []byte(`{
		"action": "created",
		"installation": {
			"id": 123,
			"repository_selection": "selected",
			"account": {"login": "helmrdotdev", "type": "Organization"}
		}
	}`)
	req := signedGitHubWebhook(http.MethodPost, "/webhooks/github", "installation", body)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.githubUpsert != nil {
		t.Fatalf("created webhook should not bind an org: %+v", store.githubUpsert)
	}
}

func TestGitHubWebhookIgnoresUnknownInstallationUpdates(t *testing.T) {
	for name, tc := range map[string]struct {
		event  string
		action string
	}{
		"new_permissions_accepted": {event: "installation", action: "new_permissions_accepted"},
		"unsuspend":                {event: "installation", action: "unsuspend"},
		"repositories_added":       {event: "installation_repositories", action: "added"},
		"repositories_removed":     {event: "installation_repositories", action: "removed"},
	} {
		t.Run(name, func(t *testing.T) {
			store := &fakeStore{}
			server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithGitHubWebhookSecret(testWebhookSecret))
			body := []byte(`{"action":"` + tc.action + `","installation":{"id":123,"repository_selection":"selected","account":{"login":"helmrdotdev","type":"Organization"}}}`)
			req := signedGitHubWebhook(http.MethodPost, "/webhooks/github", tc.event, body)
			rec := httptest.NewRecorder()

			server.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			if store.githubUpsert != nil {
				t.Fatalf("unknown installation should not be upserted: %+v", store.githubUpsert)
			}
		})
	}
}

func TestGitHubWebhookSuspendsAndDeletesInstallation(t *testing.T) {
	store := &fakeStore{}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithGitHubWebhookSecret(testWebhookSecret))
	for event, want := range map[string]string{
		"suspend": "suspend",
		"deleted": "delete",
	} {
		t.Run(event, func(t *testing.T) {
			body := []byte(`{"action":"` + event + `","installation":{"id":123,"account":{"login":"helmrdotdev","type":"Organization"}}}`)
			req := signedGitHubWebhook(http.MethodPost, "/webhooks/github", "installation", body)
			rec := httptest.NewRecorder()

			server.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			switch want {
			case "suspend":
				if store.githubSuspendByInstallationID == nil || *store.githubSuspendByInstallationID != 123 {
					t.Fatalf("suspend = %+v", store.githubSuspendByInstallationID)
				}
			case "delete":
				if store.githubDeleteByInstallationID == nil || *store.githubDeleteByInstallationID != 123 {
					t.Fatalf("delete = %+v", store.githubDeleteByInstallationID)
				}
			}
		})
	}
}

func TestGitHubWebhookInstallationRepositoriesUpdatesInstallation(t *testing.T) {
	store := &fakeStore{githubInstallation: db.GitHubAppInstallation{InstallationID: 123}}
	server := New(slog.New(slog.NewTextHandler(io.Discard, nil)), WithDB(store), WithGitHubWebhookSecret(testWebhookSecret))
	body := []byte(`{
		"action": "added",
		"installation": {
			"id": 123,
			"repository_selection": "selected",
			"account": {"login": "helmrdotdev", "type": "Organization"}
		}
	}`)
	req := signedGitHubWebhook(http.MethodPost, "/webhooks/github", "installation_repositories", body)
	rec := httptest.NewRecorder()

	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.githubUpsert == nil || store.githubUpsert.InstallationID != 123 {
		t.Fatalf("upsert = %+v", store.githubUpsert)
	}
}

func signedGitHubWebhook(method string, target string, event string, body []byte) *http.Request {
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", signGitHubWebhook(testWebhookSecret, body))
	return req
}

func signGitHubWebhook(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (f *fakeStore) UpsertGitHubInstallation(_ context.Context, arg db.UpsertGitHubInstallationParams) (db.GitHubAppInstallation, error) {
	f.githubUpsert = &arg
	return db.GitHubAppInstallation{
		ID:                  arg.ID,
		OrgID:               arg.OrgID,
		InstallationID:      arg.InstallationID,
		AccountLogin:        arg.AccountLogin,
		AccountType:         arg.AccountType,
		RepositorySelection: arg.RepositorySelection,
		HtmlUrl:             arg.HtmlUrl,
	}, nil
}

func (f *fakeStore) GetKnownGitHubInstallationByInstallationID(_ context.Context, installationID int64) (db.GitHubAppInstallation, error) {
	if f.githubInstallation.InstallationID != installationID {
		return db.GitHubAppInstallation{}, pgx.ErrNoRows
	}
	return f.githubInstallation, nil
}

func (f *fakeStore) SuspendGitHubInstallation(_ context.Context, arg db.SuspendGitHubInstallationParams) (db.GitHubAppInstallation, error) {
	f.githubSuspend = &arg
	return db.GitHubAppInstallation{InstallationID: arg.InstallationID}, nil
}

func (f *fakeStore) DeleteGitHubInstallation(_ context.Context, arg db.DeleteGitHubInstallationParams) (db.GitHubAppInstallation, error) {
	f.githubDelete = &arg
	return db.GitHubAppInstallation{InstallationID: arg.InstallationID}, nil
}

func (f *fakeStore) SuspendGitHubInstallationByInstallationID(_ context.Context, installationID int64) ([]db.GitHubAppInstallation, error) {
	f.githubSuspendByInstallationID = &installationID
	return []db.GitHubAppInstallation{{InstallationID: installationID}}, nil
}

func (f *fakeStore) DeleteGitHubInstallationByInstallationID(_ context.Context, installationID int64) ([]db.GitHubAppInstallation, error) {
	f.githubDeleteByInstallationID = &installationID
	return []db.GitHubAppInstallation{{InstallationID: installationID}}, nil
}

func (f *fakeStore) UpsertGitHubRepository(_ context.Context, arg db.UpsertGitHubRepositoryParams) (db.GithubRepository, error) {
	return db.GithubRepository{
		ID:                 arg.ID,
		OrgID:              arg.OrgID,
		InstallationID:     arg.InstallationID,
		GithubRepositoryID: arg.GithubRepositoryID,
		OwnerLogin:         arg.OwnerLogin,
		Name:               arg.Name,
		FullName:           arg.FullName,
		Private:            arg.Private,
		Archived:           arg.Archived,
		DefaultBranch:      arg.DefaultBranch,
		HtmlUrl:            arg.HtmlUrl,
	}, nil
}

func (f *fakeStore) MarkGitHubRepositoryDeleted(_ context.Context, arg db.MarkGitHubRepositoryDeletedParams) (db.MarkGitHubRepositoryDeletedRow, error) {
	return db.MarkGitHubRepositoryDeletedRow{
		OrgID:              arg.OrgID,
		InstallationID:     arg.InstallationID,
		GithubRepositoryID: arg.GithubRepositoryID,
		DeletedAt:          testTime(),
	}, nil
}

func (f *fakeStore) MarkGitHubRepositoriesDeletedByInstallationID(_ context.Context, installationID int64) ([]db.MarkGitHubRepositoriesDeletedByInstallationIDRow, error) {
	return []db.MarkGitHubRepositoriesDeletedByInstallationIDRow{{InstallationID: installationID, DeletedAt: testTime()}}, nil
}
