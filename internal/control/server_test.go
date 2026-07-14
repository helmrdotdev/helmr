package control

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/email"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5"
)

type testServerConfig struct {
	Log                *slog.Logger
	DeploymentMode     string
	WorkerGroupID      string
	RegionID           string
	DefaultRegionID    string
	DB                 db.Querier
	DBTX               dbTXBeginner
	TX                 TxBeginner
	Auth               auth.Authenticator
	CAS                cas.Store
	Secrets            SecretManager
	RunEnqueuer        RunEnqueuer
	ScheduleEngine     ScheduleRegistrar
	EventStream        *EventStream
	TelemetryReader    telemetry.Reader
	WorkerTokenSecret  []byte
	WorkerTokenTTL     time.Duration
	WorkerEnrollment   WorkerEnrollmentVerifier
	SetupToken         string
	AuthSecret         []byte
	PublicURL          *url.URL
	AuthProvider       AuthProvider
	Mailer             email.Sender
	MagicLinkDebugURLs bool
	SessionTTL         time.Duration
}

func newTestServer(testCfg testServerConfig) http.Handler {
	log := testCfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg := ServerConfig{
		Log:              log,
		DB:               testTransactionalStore{Querier: &fakeStore{}},
		TX:               panicTxBeginner{},
		Auth:             fakeAuth{},
		WorkerEnrollment: testWorkerEnrollmentVerifier{},
	}
	if testCfg.DBTX != nil {
		queries := db.New(testCfg.DBTX)
		cfg.DB = queries
		cfg.TX = testCfg.DBTX
		cfg.ReadinessDB = testCfg.DBTX
		cfg.Auth = auth.NewDBAuthenticator(queries)
	}
	if testCfg.DeploymentMode != "" {
		cfg.DeploymentMode = testCfg.DeploymentMode
	}
	cfg.WorkerGroupID = "us-east-1-worker-group-1"
	cfg.RegionID = "us-east-1"
	cfg.DefaultRegionID = "us-east-1"
	if testCfg.WorkerGroupID != "" {
		cfg.WorkerGroupID = testCfg.WorkerGroupID
	}
	if testCfg.RegionID != "" {
		cfg.RegionID = testCfg.RegionID
	}
	if testCfg.DefaultRegionID != "" {
		cfg.DefaultRegionID = testCfg.DefaultRegionID
	}
	if testCfg.DB != nil {
		cfg.DB = testTransactionalStore{Querier: testCfg.DB}
		if store, ok := testCfg.DB.(*fakeStore); ok && testCfg.TelemetryReader == nil {
			cfg.TelemetryReader = fakeTelemetryReader{store: store}
		}
	}
	if testCfg.TelemetryReader != nil {
		cfg.TelemetryReader = testCfg.TelemetryReader
	}
	if cfg.TelemetryReader == nil {
		cfg.TelemetryReader = fakeTelemetryReader{store: &fakeStore{}}
	}
	if testCfg.TX != nil {
		cfg.TX = testCfg.TX
	}
	if testCfg.Auth != nil {
		cfg.Auth = testCfg.Auth
	}
	if testCfg.CAS != nil {
		cfg.CAS = testCfg.CAS
	}
	if testCfg.Secrets != nil {
		cfg.Secrets = testCfg.Secrets
	}
	if testCfg.RunEnqueuer != nil {
		cfg.RunEnqueuer = testCfg.RunEnqueuer
	}
	if testCfg.ScheduleEngine != nil {
		cfg.ScheduleEngine = testCfg.ScheduleEngine
	}
	if testCfg.EventStream != nil {
		cfg.EventStream = testCfg.EventStream
		if cfg.EventStream.telemetryReader == nil {
			cfg.EventStream.telemetryReader = cfg.TelemetryReader
		}
	}
	if len(testCfg.WorkerTokenSecret) > 0 {
		cfg.WorkerTokenSecret = testCfg.WorkerTokenSecret
	}
	if testCfg.WorkerTokenTTL != 0 {
		cfg.WorkerTokenTTL = testCfg.WorkerTokenTTL
	}
	if testCfg.WorkerEnrollment != nil {
		cfg.WorkerEnrollment = testCfg.WorkerEnrollment
	}
	if testCfg.SetupToken != "" {
		cfg.SetupToken = testCfg.SetupToken
	}
	if len(testCfg.AuthSecret) > 0 {
		cfg.AuthSecret = testCfg.AuthSecret
	}
	if testCfg.PublicURL != nil {
		cfg.PublicURL = testCfg.PublicURL
	}
	if testCfg.AuthProvider != nil {
		cfg.AuthProvider = testCfg.AuthProvider
	}
	if testCfg.Mailer != nil {
		cfg.Mailer = testCfg.Mailer
	}
	if testCfg.MagicLinkDebugURLs {
		cfg.MagicLinkDebugURLs = testCfg.MagicLinkDebugURLs
	}
	if testCfg.SessionTTL != 0 {
		cfg.SessionTTL = testCfg.SessionTTL
	}
	handler, err := NewServer(cfg)
	if err != nil {
		panic(err)
	}
	return handler
}

type testWorkerEnrollmentVerifier struct{}

func (testWorkerEnrollmentVerifier) VerifyWorkerEnrollment(context.Context, api.WorkerEnrollmentRequest) (VerifiedWorkerEnrollment, error) {
	return VerifiedWorkerEnrollment{}, errors.New("worker enrollment is not configured for this test")
}

type panicTxBeginner struct{}

func (panicTxBeginner) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected transaction")
}

type testTransactionalStore struct {
	db.Querier
}

func (store testTransactionalStore) BeginQuerier(context.Context) (db.Querier, controlTransaction, error) {
	return store.Querier, noopControlTransaction{}, nil
}

type fakeTelemetryReader struct {
	store                 *fakeStore
	listEventsErr         error
	listRunLogChunksErr   error
	listTerminalOutputErr error
}

func (r fakeTelemetryReader) ListEvents(ctx context.Context, query telemetry.EventQuery) (telemetry.EventPage, error) {
	if r.listEventsErr != nil {
		return telemetry.EventPage{}, r.listEventsErr
	}
	events := make([]api.RunEvent, 0, len(r.store.events)+len(r.store.deploymentEvents))
	last := query.AfterSeq
	for _, row := range r.store.events {
		if query.SubjectType != eventSubjectTypeRun || row.RunID != pgvalue.UUID(query.SubjectID) || row.Seq <= query.AfterSeq {
			continue
		}
		events = append(events, eventResponseFromClaim(row))
		last = row.Seq
		if int32(len(events)) == query.Limit {
			return telemetry.EventPage{Events: events, LastSeq: last}, nil
		}
	}
	for _, row := range r.store.deploymentEvents {
		if query.SubjectType != eventSubjectTypeDeployment || row.DeploymentID != pgvalue.UUID(query.SubjectID) || row.Seq <= query.AfterSeq {
			continue
		}
		events = append(events, eventResponseFromClaim(row))
		last = row.Seq
		if int32(len(events)) == query.Limit {
			break
		}
	}
	return telemetry.EventPage{Events: events, LastSeq: last}, nil
}

func (r fakeTelemetryReader) ListRunLogChunks(ctx context.Context, query telemetry.RunLogChunkQuery) (telemetry.RunLogChunkPage, error) {
	if r.listRunLogChunksErr != nil {
		return telemetry.RunLogChunkPage{}, r.listRunLogChunksErr
	}
	rows, err := r.store.ListRunLogChunksAfter(ctx, query)
	if err != nil {
		return telemetry.RunLogChunkPage{}, err
	}
	chunks := make([]api.RunLogChunk, 0, len(rows))
	last := query.AfterSeq
	for _, row := range rows {
		chunks = append(chunks, runLogChunkResponse(row))
		last = row.Seq
	}
	return telemetry.RunLogChunkPage{Chunks: chunks, LastSeq: last}, nil
}

func (r fakeTelemetryReader) ListTerminalOutput(ctx context.Context, query telemetry.TerminalOutputQuery) (telemetry.TerminalOutputPage, error) {
	if r.listTerminalOutputErr != nil {
		return telemetry.TerminalOutputPage{}, r.listTerminalOutputErr
	}
	page := telemetry.TerminalOutputPage{LastOffset: query.AfterOffset}
	switch {
	case query.ResourceKind == "workspace_process" && (query.StreamName == workspaceStreamStdout || query.StreamName == workspaceStreamStderr):
		rows, err := r.store.ListWorkspaceExecStreamChunksAfter(ctx, db.ListWorkspaceExecStreamChunksAfterParams{
			OrgID:         pgvalue.UUID(query.OrgID),
			ProjectID:     pgvalue.UUID(query.ProjectID),
			EnvironmentID: pgvalue.UUID(query.EnvironmentID),
			WorkspaceID:   pgvalue.UUID(query.WorkspaceID),
			ProcessID:     pgvalue.UUID(query.ResourceID),
			StreamName:    string(query.StreamName),
			CursorOffset:  query.AfterOffset,
			LimitCount:    query.Limit,
		})
		if err != nil {
			return telemetry.TerminalOutputPage{}, err
		}
		for _, row := range rows {
			chunk := workspaceExecStreamChunkResponse(row)
			page.Chunks = append(page.Chunks, telemetry.TerminalOutputChunk{
				ID:          chunk.ID,
				Stream:      chunk.Stream,
				OffsetStart: chunk.OffsetStart,
				OffsetEnd:   chunk.OffsetEnd,
				Data:        chunk.Data,
				ObservedAt:  chunk.ObservedAt,
				CreatedAt:   chunk.CreatedAt,
			})
			page.LastOffset = row.OffsetEnd
		}
	case query.ResourceKind == "workspace_process" && query.StreamName == workspaceStreamOutput:
		rows, err := r.store.ListWorkspacePtyStreamChunksAfter(ctx, db.ListWorkspacePtyStreamChunksAfterParams{
			OrgID:         pgvalue.UUID(query.OrgID),
			ProjectID:     pgvalue.UUID(query.ProjectID),
			EnvironmentID: pgvalue.UUID(query.EnvironmentID),
			WorkspaceID:   pgvalue.UUID(query.WorkspaceID),
			ProcessID:     pgvalue.UUID(query.ResourceID),
			StreamName:    string(query.StreamName),
			CursorOffset:  query.AfterOffset,
			LimitCount:    query.Limit,
		})
		if err != nil {
			return telemetry.TerminalOutputPage{}, err
		}
		for _, row := range rows {
			chunk := workspacePtyStreamChunkResponse(row)
			page.Chunks = append(page.Chunks, telemetry.TerminalOutputChunk{
				ID:          chunk.ID,
				Stream:      chunk.Stream,
				OffsetStart: chunk.OffsetStart,
				OffsetEnd:   chunk.OffsetEnd,
				Data:        chunk.Data,
				ObservedAt:  chunk.ObservedAt,
				CreatedAt:   chunk.CreatedAt,
			})
			page.LastOffset = row.OffsetEnd
		}
	}
	return page, nil
}

func (r fakeTelemetryReader) GetRunLogSnapshot(ctx context.Context, query telemetry.RunLogSnapshotQuery) (telemetry.RunLogSnapshot, error) {
	row, err := r.store.GetRunLogSnapshot(ctx, query)
	if errors.Is(err, pgx.ErrNoRows) {
		return telemetry.RunLogSnapshot{}, nil
	}
	return row, err
}

type noopControlTransaction struct{}

func (noopControlTransaction) Commit(context.Context) error {
	return nil
}

func (noopControlTransaction) Rollback(context.Context) error {
	return nil
}

func mustParseTestURL(raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestNewServerAllowsEventStreamComponentForDifferentWorkerGroup(t *testing.T) {
	store := &fakeStore{}
	_, err := NewServer(ServerConfig{
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		WorkerGroupID:    dbtest.DefaultWorkerGroupID,
		RegionID:         "us-east-1",
		DefaultRegionID:  "us-east-1",
		DB:               testTransactionalStore{Querier: store},
		TX:               panicTxBeginner{},
		Auth:             fakeAuth{},
		WorkerEnrollment: testWorkerEnrollmentVerifier{},
		TelemetryReader:  fakeTelemetryReader{store: store},
		EventStream: &EventStream{
			log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
			telemetryReader: fakeTelemetryReader{store: store},
		},
	})
	if err != nil {
		t.Fatalf("NewServer err = %v", err)
	}
}

func TestAPIRejectsOversizedRequestBody(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}, Secrets: fakeSecrets{}})
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(strings.Repeat("x", int(apiRequestBodyLimit)+1)))
	req.Header.Set("authorization", "Bearer test-key")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIRejectsUnsupportedAPIVersion(t *testing.T) {
	handler := newTestServer(testServerConfig{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: &fakeStore{}, Auth: fakeAuth{}})
	req := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"task_id":"deploy"}`))
	req.Header.Set("authorization", "Bearer test-key")
	req.Header.Set(api.APIVersionHeader, "2099-01-01")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(api.APIVersionHeader); got != api.CurrentAPIVersion {
		t.Fatalf("response %s = %q", api.APIVersionHeader, got)
	}
	if !strings.Contains(rec.Body.String(), "unsupported "+api.APIVersionHeader) {
		t.Fatalf("body = %s", rec.Body.String())
	}
	requireErrorCode(t, rec.Body.Bytes(), "unsupported_api_version")
}

func TestRecoverPanicsWritesJSONError(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := server.recoverPanics(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("content-type"); contentType != "application/json" {
		t.Fatalf("content-type = %q", contentType)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestRecoverPanicsRepanicsAfterResponseCommitted(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := server.recoverPanics(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	}))
	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	rec := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		handler.ServeHTTP(rec, req)
	}()

	if recovered == nil {
		t.Fatal("expected panic after committed response")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); body != "partial" {
		t.Fatalf("body = %s", body)
	}
}
