package main

import (
	"bytes"
	"context"
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
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
		Auth:               auth.NewDBAuthenticator(store),
		AuthSecret:         []byte("abcdefghijabcdefghijabcdefghij12"),
		PublicURL:          publicURL,
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
	t.Setenv("HELMR_CAS_URI", "s3://helmr-smoke")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_WORKER_BOOTSTRAP_TOKEN", "worker-bootstrap-token")
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
	dbName := "helmr_smoke_" + strings.ReplaceAll(ids.New().String(), "-", "")
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
