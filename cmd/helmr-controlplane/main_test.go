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
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/controlplane"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/enrollment"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workspace"
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
	handler, err := controlplane.NewServer(controlplane.ServerConfig{
		Log:                   log,
		DB:                    store,
		TX:                    panicTxBeginner{},
		Auth:                  controlplane.NewDBAuthenticator(store),
		WorkerEnrollment:      controlplanetestWorkerEnrollmentVerifier(),
		SecretDelivery:        controlplanetestSecretDeliveryOpener{},
		RegistryCredentials:   controlplanetestRegistryCredentialOpener{},
		WorkspaceFencingKey:   controlplanetestWorkspaceFencingKey(),
		TokenCredentialKey:    controlplanetestTokenCredentialKey(),
		AuthKey:               make([]byte, auth.RootKeySize),
		WorkerTokenSigningKey: make([]byte, auth.WorkerTokenSigningKeySize),
		PublicURL:             publicURL,
		TelemetryReader:       controlplanetestTelemetryReader{store: store},
		PlatformArtifactLocks: controlplanetestPlatformArtifactLocker{},
		MagicLinkDebugURLs:    true,
		Mailer:                configuredEmailSender(log, config.ControlPlane{EmailProvider: config.EmailProviderNone}),
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
	if got, want := rec.Body.String(), "{\"error\":{\"code\":\"service_unavailable\",\"message\":\"service is unavailable\"}}\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

func TestServeControlPlaneBindsBeforeStartingWorkflows(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	started := make(chan struct{}, 1)
	server := &http.Server{Addr: occupied.Addr().String(), Handler: http.NotFoundHandler()}
	err = serveControlPlane(
		t.Context(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		server,
		[]backgroundWorkflow{{name: "test", run: func(context.Context) error {
			started <- struct{}{}
			return nil
		}}},
		time.Second,
	)
	if err == nil {
		t.Fatal("occupied address was accepted")
	}
	select {
	case <-started:
		t.Fatal("workflow started before listener bind succeeded")
	default:
	}
}

func TestServeControlPlaneStopsAndWaitsAfterWorkflowFailure(t *testing.T) {
	want := errors.New("publisher failed")
	stopped := make(chan struct{})
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	err := serveControlPlane(
		t.Context(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		server,
		[]backgroundWorkflow{
			{name: "failed", run: func(context.Context) error { return want }},
			{name: "waiting", run: func(ctx context.Context) error {
				<-ctx.Done()
				close(stopped)
				return ctx.Err()
			}},
		},
		time.Second,
	)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Control Plane returned before every workflow stopped")
	}
}

func TestServeControlPlaneDrainsActiveRequests(t *testing.T) {
	addr := freeSmokeAddr(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveControlPlane(
			ctx,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			server,
			nil,
			time.Second,
		)
	}()
	requestErr := make(chan error, 1)
	go func() {
		response, err := getControlPlane(addr)
		if err == nil {
			response.Body.Close()
		}
		requestErr <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach Control Plane")
	}
	cancel()
	select {
	case err := <-serveErr:
		t.Fatalf("Control Plane stopped before request drained: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-requestErr; err != nil {
		t.Fatal(err)
	}
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
}

func TestServeControlPlaneForceClosesAfterDrainTimeout(t *testing.T) {
	addr := freeSmokeAddr(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(started)
			<-release
		}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveControlPlane(
			ctx,
			slog.New(slog.NewTextHandler(io.Discard, nil)),
			server,
			nil,
			20*time.Millisecond,
		)
	}()
	requestDone := make(chan struct{})
	go func() {
		response, err := getControlPlane(addr)
		if err == nil {
			response.Body.Close()
		}
		close(requestDone)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not reach Control Plane")
	}
	cancel()
	err := <-serveErr
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want shutdown deadline", err)
	}
	close(release)
	select {
	case <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("forced close did not release client")
	}
}

type controlplanetestPlatformArtifactLocker struct{}

func (controlplanetestPlatformArtifactLocker) With(
	_ context.Context,
	_ []string,
	fn func() error,
) error {
	return fn()
}

type controlplanetestSecretDeliveryOpener struct{}

func (controlplanetestSecretDeliveryOpener) OpenDeliveries(
	uuid.UUID,
	[]secret.DeliveryEnvelope,
) ([]secret.DeliveryMaterial, error) {
	return nil, nil
}

type controlplanetestRegistryCredentialOpener struct{}

func (controlplanetestRegistryCredentialOpener) OpenRegistryCredential(
	uuid.UUID,
	db.Secret,
	db.SecretVersion,
) ([]byte, error) {
	return []byte("registry-password"), nil
}

func controlplanetestWorkspaceFencingKey() workspace.FencingKey {
	key, err := workspace.NewFencingKey(make([]byte, workspace.FencingKeySize))
	if err != nil {
		panic(err)
	}
	return key
}

func controlplanetestTokenCredentialKey() auth.CredentialKey {
	key := make([]byte, auth.CredentialKeySize)
	for index := range key {
		key[index] = 3
	}
	credentialKey, err := auth.NewCredentialKey(key)
	if err != nil {
		panic(err)
	}
	return credentialKey
}

func TestRunServesReadyzAndDeviceStart(t *testing.T) {
	ctx := context.Background()
	databaseURL := newSmokeDatabase(t, ctx)
	redisServer := miniredis.RunT(t)
	addr := freeSmokeAddr(t)

	t.Setenv("CONTROL_PLANE_ADDR", addr)
	t.Setenv("DATABASE_URL", databaseURL)
	t.Setenv("REDIS_URL", "redis://"+redisServer.Addr()+"/0")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:1")
	t.Setenv("CAS_URI", "s3://helmr-smoke")
	buildPolicyPath := t.TempDir() + "/build-policy.json"
	buildPolicy := smokeBuildPolicy(t)
	if err := os.WriteFile(buildPolicyPath, []byte(buildPolicy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUILD_POLICY_PATH", buildPolicyPath)
	t.Setenv("PLATFORM_STORE_URI", "s3://helmr-smoke-runtime")
	originalBuildPolicyLoader := loadControlPlaneBuildPolicy
	loadControlPlaneBuildPolicy = func(string) (*deployment.BuildPolicy, error) {
		return deployment.ParseBuildPolicy([]byte(buildPolicy))
	}
	t.Cleanup(func() { loadControlPlaneBuildPolicy = originalBuildPolicyLoader })
	t.Setenv("REGION_ID", "us-east-1")
	t.Setenv("DEFAULT_REGION_ID", "us-east-1")
	t.Setenv("PROVIDER", "aws")
	t.Setenv("PROVIDER_REGION", "us-east-1")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("TOKEN_CREDENTIAL_KEY", "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM=")
	t.Setenv("WORKER_GROUPS", `[{"id":"us-east-1-worker-group-1","name":"run","enrollment_secret_env":"WORKER_GROUP_ENROLLMENT_SECRET_RUN","allows_run":true,"allows_build":false,"observation_ttl_seconds":3600,"instance_capacity":{"milli_cpu":1000,"memory_bytes":1024,"guest_ephemeral_disk_bytes":1024,"vm_slots":1}}]`)
	t.Setenv("WORKER_GROUP_ENROLLMENT_SECRET_RUN", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")
	t.Setenv("SETUP_TOKEN", "setup-token")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("WORKSPACE_FENCING_KEY", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=")
	t.Setenv("PUBLIC_URL", "http://"+addr)
	t.Setenv("EMAIL_PROVIDER", "none")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- runControlPlane(runCtx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("Control Plane run returned %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Control Plane run did not stop")
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

func getControlPlane(addr string) (*http.Response, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.Get("http://" + addr)
		if err == nil || time.Now().After(deadline) {
			return response, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type emptyStore struct {
	db.Querier
}

func controlplanetestWorkerEnrollmentVerifier() *enrollment.Verifier {
	verifier, err := enrollment.NewVerifier([]enrollment.GroupSecret{{
		GroupID: "us-east-1-worker-group-1",
		Secret:  "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8",
	}})
	if err != nil {
		panic(err)
	}
	return verifier
}

type controlplanetestTelemetryReader struct {
	store *emptyStore
}

func (r controlplanetestTelemetryReader) ListEvents(context.Context, telemetry.EventQuery) (telemetry.EventPage, error) {
	return telemetry.EventPage{}, nil
}

func (r controlplanetestTelemetryReader) ListRunLogChunks(context.Context, telemetry.RunLogChunkQuery) (telemetry.RunLogChunkPage, error) {
	return telemetry.RunLogChunkPage{}, nil
}

func (r controlplanetestTelemetryReader) ListTerminalOutput(context.Context, telemetry.TerminalOutputQuery) (telemetry.TerminalOutputPage, error) {
	return telemetry.TerminalOutputPage{}, nil
}

func (r controlplanetestTelemetryReader) GetRunLogSnapshot(context.Context, telemetry.RunLogSnapshotQuery) (telemetry.RunLogSnapshot, error) {
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
		t.Skipf("Postgres %d is older than the Helmr PostgreSQL 18 schema baseline; skipping Control Plane smoke test", serverVersion)
	}
	if err := schema.Up(ctx, databaseURL); err != nil {
		t.Fatal(err)
	}
	return databaseURL
}

func smokeBuildPolicy(t *testing.T) string {
	t.Helper()
	digest := func(character string) string {
		return "sha256:" + strings.Repeat(character, 64)
	}
	raw, err := deployment.ComposeBuildPolicy(
		deployment.RuntimeInputs{
			Harness: deployment.ArtifactDescriptor{
				Digest: digest("1"), MediaType: deployment.PlatformTreeInputMediaType, SizeBytes: 4096,
			},
		},
		deployment.ToolchainInputs{
			Base: deployment.ArtifactDescriptor{
				Digest: digest("2"), MediaType: deployment.PlatformTreeInputMediaType, SizeBytes: 4096,
			},
			Compiler: deployment.CompilerInputs{
				APIVersion: "helmr.compiler.v0",
				ConfigEvaluator: deployment.CompilerEntrypoint{
					APIVersion: deployment.ConfigEvaluatorAPIVersion,
					Digest:     digest("3"), Entrypoint: "/nix/helmr/config-evaluator.mjs",
				},
				Esbuild: deployment.EsbuildInputs{
					APIPackageDigest: digest("4"), BinaryDigest: digest("5"),
					BinaryPath: "/nix/helmr/esbuild", PackagePath: "/nix/node_modules/esbuild",
					Version: "0.28.1",
				},
				OptionsContractDigest: digest("6"),
				Output: deployment.CompilerOutputContract{
					Aggregate: "analysis-only", FinalModules: "independent", SourceMaps: "external",
				},
				ProgramCompiler: deployment.CompilerEntrypoint{
					APIVersion: "helmr.compiler.v0",
					Digest:     digest("7"), Entrypoint: "/nix/helmr/program-compiler.mjs",
				},
				Source: deployment.CompilerSourceContract{
					DeclarationExtensions: []string{".cjs", ".cts", ".js", ".jsx", ".mjs", ".mts", ".ts", ".tsx"},
					PackageDependencies:   "external",
					Semantics:             "pinned-esbuild",
					WorkspaceDependencies: "bundled",
				},
			},
		},
		[]byte("node release keyring"),
		[]string{"00112233445566778899AABBCCDDEEFF00112233"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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
