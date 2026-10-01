package controlplane

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

// startSession starts the fixture's Actor on its indexed Computer through the
// session owner, with the request normalized as a public start is.
func startSession(t *testing.T, f sessiontest.Fixture, index int, key *string, idempotencyKey string) session.Started {
	t.Helper()
	start, claim, err := prepareActorStart(actorStartRequest{
		OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID,
		ActorDeclaredID: sessiontest.ActorDeclaredID, ComputerID: f.ComputerIDs[index],
		Key: key, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	started, err := session.Start(t.Context(), f.Pool, claim, start)
	if err != nil {
		t.Fatal(err)
	}
	return started
}

// startTaskRun normalizes a Task start as a public start is and admits it
// through the run owner.
func startTaskRun(ctx context.Context, f sessiontest.Fixture, request taskStartRequest) (run.TaskStarted, error) {
	normalized, err := normalizeTaskStart(request)
	if err != nil {
		return run.TaskStarted{}, err
	}
	var claim idempotency.Request
	if normalized.IdempotencyKey != "" {
		claim, err = idempotency.NewTaskStartRequest(normalized.EnvironmentID, normalized.TaskDeclaredID, normalized.IdempotencyKey, normalized.fingerprint)
		if err != nil {
			return run.TaskStarted{}, err
		}
	}
	return run.StartTask(ctx, f.Pool, claim, normalized.runTaskStart())
}

// sessionHTTP serves the control plane built by NewServer over a Session
// fixture's database. Login sessions authenticate as in production; API key
// bearer tokens authenticate as the principal registered for them.
type sessionHTTP struct {
	httpPostgresFixture
	sessiontest.Fixture
	principals *principalAuthenticator
}

func newSessionHTTP(t *testing.T, f sessiontest.Fixture) sessionHTTP {
	t.Helper()
	keys, err := auth.NewKeys(completeServerConfig(t).AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	principals := &principalAuthenticator{principals: map[string]auth.Principal{}}
	handler := newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
		cfg.Auth = principals
		cfg.PublicURL = &url.URL{Scheme: "https", Host: "console.example.test"}
	})
	return sessionHTTP{
		httpPostgresFixture: httpPostgresFixture{pool: f.Pool, queries: db.New(f.Pool), handler: handler, keys: keys},
		Fixture:             f, principals: principals,
	}
}

// apiKey registers an API key bearer token that authenticates as principal.
func (h sessionHTTP) apiKey(principal auth.Principal) string {
	return h.principals.apiKey(principal)
}

// memberSession returns the login session token of a new member of the
// fixture's organization with the role.
func (h sessionHTTP) memberSession(t *testing.T, role db.OrgMemberRole) string {
	t.Helper()
	userID := h.user(t, string(role))
	h.member(t, h.OrgID, userID, role)
	return h.session(t, userID, h.OrgID)
}

// environmentPath is the management route path of the fixture's environment
// followed by suffix.
func (h sessionHTTP) environmentPath(suffix string) string {
	return "/api/projects/" + h.ProjectID.String() + "/environments/" + h.EnvironmentID.String() + suffix
}

type principalAuthenticator struct {
	mu         sync.Mutex
	principals map[string]auth.Principal
}

// apiKey registers an API key bearer token that authenticates as principal.
func (a *principalAuthenticator) apiKey(principal auth.Principal) string {
	token := auth.APIKeyPrefix + uuid.NewV7().String()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.principals[token] = principal
	return token
}

func (a *principalAuthenticator) Authenticate(_ context.Context, token string) (auth.Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	principal, ok := a.principals[token]
	if !ok {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return principal, nil
}

// settleActorBootRun records the boot Run's success, with the Session's
// committed input at committedInputSequence and no current Run.
func settleActorBootRun(
	t *testing.T,
	fixture sessiontest.Fixture,
	started session.Started,
	committedInputSequence int64,
) {
	t.Helper()
	tx, err := fixture.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SET CONSTRAINTS ALL DEFERRED`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE run_attempts
		   SET entrypoint_entered_at = now(),
		       terminal_session_input_sequence = $2,
		       terminal_outcome = 'succeeded',
		       terminal_reason_code = 'completed',
		       terminal_at = now()
		 WHERE run_id = $1
		   AND number = 1
	`, started.BootRunID, committedInputSequence); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE runs
		   SET status = 'succeeded',
		       terminal_at = now(),
		       updated_at = now()
		 WHERE id = $1
	`, started.BootRunID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `
		UPDATE sessions
		   SET current_run_id = NULL,
		       committed_input_sequence = $2,
		       run_generation = run_generation + 1,
		       revision = revision + 1,
		       updated_at = now()
		 WHERE id = $1
	`, started.SessionID, committedInputSequence); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
