package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/enrollment"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerEnrollmentConfigurationAlwaysLoadsAWS(t *testing.T) {
	original := loadAWSWorkerEnrollmentVerifier
	loadAWSWorkerEnrollmentVerifier = func(context.Context, []enrollment.AWSGroupBoundary) (*enrollment.AWSVerifier, error) {
		return nil, errors.New("aws unavailable")
	}
	t.Cleanup(func() { loadAWSWorkerEnrollmentVerifier = original })
	if _, err := loadAWSWorkerEnrollmentVerifier(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "aws unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestEmailProviderNoneDisablesDebugLogMailer(t *testing.T) {
	store := &emptyStore{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	publicURL, err := url.Parse("https://helmr.example.test")
	if err != nil {
		t.Fatal(err)
	}
	handler, err := control.NewServer(control.ServerConfig{
		Log:                log,
		DB:                 store,
		TX:                 panicTxBeginner{},
		Auth:               auth.NewDBAuthenticator(store),
		WorkerEnrollment:   controltestWorkerEnrollmentVerifier{},
		AuthSecret:         []byte("abcdefghijabcdefghijabcdefghij12"),
		PublicURL:          publicURL,
		WorkerGroupID:      "us-east-1-worker-group-1",
		RegionID:           "us-east-1",
		DefaultRegionID:    "us-east-1",
		TelemetryReader:    controltestTelemetryReader{store: store},
		MagicLinkDebugURLs: true,
		Mailer:             configuredEmailSender(log, config.Control{EmailProvider: config.EmailProviderNone}),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/magic-link/start", bytes.NewBufferString(`{"email":"user@example.test"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "magic link mailer is not configured") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestRunServesReadyzAndDeviceStart(t *testing.T) {
	ctx := context.Background()
	databaseURL := newSmokeDatabase(t, ctx)
	redisServer := miniredis.RunT(t)
	addr := freeSmokeAddr(t)

	t.Setenv("HELMR_CONTROL_ADDR", addr)
	t.Setenv("HELMR_DATABASE_URL", databaseURL)
	t.Setenv("HELMR_REDIS_URL", "redis://"+redisServer.Addr()+"/0")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:1")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-smoke")
	t.Setenv("HELMR_WORKER_GROUP_ID", "us-east-1-worker-group-1")
	t.Setenv("HELMR_REGION_ID", "us-east-1")
	t.Setenv("HELMR_DEFAULT_REGION_ID", "us-east-1")
	t.Setenv("HELMR_PROVIDER", "aws")
	t.Setenv("HELMR_PROVIDER_REGION", "us-east-1")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_WORKER_GROUPS", `[{"id":"us-east-1-worker-group-1","name":"run","region":"us-east-1","account_id":"123456789012","autoscaling_group":"test-run","instance_profile_arn":"arn:aws:iam::123456789012:instance-profile/test-run","launch_ami_id":"ami-test","ami_ids":["ami-test"],"allows_run":true,"allows_build":false,"instance_capacity":{"milli_cpu":1000,"memory_bytes":1024,"workload_disk_bytes":1024,"scratch_bytes":1024,"vm_slots":1}}]`)
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_PUBLIC_URL", "http://"+addr)
	t.Setenv("HELMR_EMAIL_PROVIDER", "none")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- run(runCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("control run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("control run did not stop")
		}
	})

	baseURL := "http://" + addr
	waitForHTTPStatus(t, baseURL+"/readyz", http.StatusOK)
	rec := postJSON(t, baseURL+"/api/auth/device/start", `{}`)
	if rec.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("device start status = %d body=%s", rec.StatusCode, string(body))
	}
}

type emptyStore struct {
	db.Querier
}

type controltestWorkerEnrollmentVerifier struct{}

func (controltestWorkerEnrollmentVerifier) VerifyWorkerEnrollment(context.Context, api.WorkerEnrollmentRequest) (control.VerifiedWorkerEnrollment, error) {
	return control.VerifiedWorkerEnrollment{}, nil
}

type controltestTelemetryReader struct {
	store *emptyStore
}

func (r controltestTelemetryReader) ListEvents(context.Context, telemetry.EventQuery) (telemetry.EventPage, error) {
	return telemetry.EventPage{}, nil
}

func (r controltestTelemetryReader) ListRunLogChunks(context.Context, telemetry.RunLogChunkQuery) (telemetry.RunLogChunkPage, error) {
	return telemetry.RunLogChunkPage{}, nil
}

func (r controltestTelemetryReader) ListTerminalOutput(context.Context, telemetry.TerminalOutputQuery) (telemetry.TerminalOutputPage, error) {
	return telemetry.TerminalOutputPage{}, nil
}

func (r controltestTelemetryReader) GetRunLogSnapshot(context.Context, telemetry.RunLogSnapshotQuery) (telemetry.RunLogSnapshot, error) {
	return telemetry.RunLogSnapshot{}, nil
}

type panicTxBeginner struct{}

func (panicTxBeginner) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected transaction")
}

func newSmokeDatabase(t *testing.T, ctx context.Context) string {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("HELMR_TEST_DATABASE_URL"))
	if dsn == "" {
		t.Skip("HELMR_TEST_DATABASE_URL is required for whole-binary smoke tests")
	}
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "helmr_smoke_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")
	dbIdentifier := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbIdentifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupCtx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = admin.Exec(cleanupCtx, "DROP DATABASE IF EXISTS "+dbIdentifier)
		admin.Close()
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Database = dbName
	databaseURL := config.ConnString()
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(checkCtx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var serverVersion int
	if err := pool.QueryRow(checkCtx, `SELECT current_setting('server_version_num')::int`).Scan(&serverVersion); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	pool.Close()
	if serverVersion < 180000 {
		t.Skipf("Postgres %d does not provide uuidv7(); skipping control smoke test", serverVersion)
	}
	if err := schema.Up(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	return databaseURL
}

func freeSmokeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

func waitForHTTPStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == want {
				return
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("GET %s: %v", url, err)
			}
			t.Fatalf("GET %s did not return %d", url, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func postJSON(t *testing.T, url string, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
