package config

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRootKeyRequiresBase64EncodingOf32Bytes(t *testing.T) {
	for _, value := range []string{"", "short", "AQ==", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", " AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TEST_ROOT_KEY", value)
			if _, err := rootKey("TEST_ROOT_KEY"); err == nil {
				t.Fatalf("accepted %q", value)
			}
		})
	}
	t.Setenv("TEST_ROOT_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	key, err := rootKey("TEST_ROOT_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(key, make([]byte, 32)) {
		t.Fatalf("key = %x", key)
	}
}

func TestEnvSecretPreservesValue(t *testing.T) {
	t.Setenv("TEST_SECRET", " secret with surrounding whitespace\n")
	if got := envSecret("TEST_SECRET"); got != " secret with surrounding whitespace\n" {
		t.Fatalf("secret = %q", got)
	}
}

func TestLoadImageCacheIsAbsentOrCompletelyConfigured(t *testing.T) {
	config, err := loadImageCache()
	if err != nil || config != nil {
		t.Fatalf("empty config = %+v, %v", config, err)
	}

	t.Setenv("IMAGE_CACHE_REGISTRY_AUTHORITY", "123456789012.dkr.ecr.us-east-1.amazonaws.com")
	if _, err := loadImageCache(); err == nil {
		t.Fatal("partial image cache configuration accepted")
	}
	t.Setenv("IMAGE_CACHE_REPOSITORY_PREFIX", "helmr-cache")
	t.Setenv("IMAGE_CACHE_ROLE_ARN", "arn:aws:iam::123456789012:role/helmr-cache")
	t.Setenv("IMAGE_CACHE_REPOSITORY_ARN_PREFIX", "arn:aws:ecr:us-east-1:123456789012:repository/helmr-cache/")
	config, err = loadImageCache()
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || config.RepositoryPrefix != "helmr-cache" || config.CacheRoleARN == "" {
		t.Fatalf("config = %+v", config)
	}
}

func TestLoadDispatcherReadsScheduleClaimConfig(t *testing.T) {
	setDispatcherFencing(t)
	t.Setenv("DATABASE_URL", " postgres://example ")
	t.Setenv("REDIS_URL", " redis://redis.example.test:6379/0 ")
	t.Setenv("CLICKHOUSE_URL", " https://clickhouse.example.test ")
	t.Setenv("SCHEDULE_POLL_INTERVAL", " 250ms ")
	t.Setenv("SCHEDULE_CLAIM_LIMIT", " 25 ")
	t.Setenv("SCHEDULE_CONCURRENCY", " 4 ")
	t.Setenv("SCHEDULE_CLAIM_LEASE", " 2m ")
	t.Setenv("RUN_RESERVATION_TTL", " 3m ")
	t.Setenv("RUN_LEASE_START_DEADLINE", " 30s ")
	t.Setenv("RUN_LEASE_TTL", " 4m ")

	cfg, err := LoadDispatcher()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://example" ||
		cfg.RedisURL != "redis://redis.example.test:6379/0" ||
		cfg.ClickHouseURL != "https://clickhouse.example.test" ||
		cfg.SchedulePollInterval != 250*time.Millisecond ||
		cfg.ScheduleClaimLimit != 25 ||
		cfg.ScheduleConcurrency != 4 ||
		cfg.ScheduleClaimLease != 2*time.Minute ||
		cfg.RunReservationTTL != 3*time.Minute ||
		cfg.RunLeaseStartDeadline != 30*time.Second ||
		cfg.RunLeaseTTL != 4*time.Minute {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadDispatcherRejectsNonPositiveScheduleClaimConfig(t *testing.T) {
	for _, variable := range []string{
		"SCHEDULE_POLL_INTERVAL",
		"SCHEDULE_CLAIM_LIMIT",
		"SCHEDULE_CONCURRENCY",
		"SCHEDULE_CLAIM_LEASE",
	} {
		t.Run(variable, func(t *testing.T) {
			setDispatcherFencing(t)
			t.Setenv("DATABASE_URL", "postgres://example")
			t.Setenv("CLICKHOUSE_URL", "https://clickhouse.example.test")
			t.Setenv(variable, "0")

			_, err := LoadDispatcher()
			if err == nil {
				t.Fatalf("expected %s validation error", variable)
			}
		})
	}
}

func TestLoadDispatcherRejectsScheduleClaimConfigAboveInt32(t *testing.T) {
	setDispatcherFencing(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "https://clickhouse.example.test")
	t.Setenv("SCHEDULE_CLAIM_LIMIT", "2147483648")

	if _, err := LoadDispatcher(); err == nil {
		t.Fatal("expected Schedule claim limit error")
	}
}

func TestLoadDispatcherRejectsInvalidRunLeasePolicy(t *testing.T) {
	setDispatcherFencing(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "https://clickhouse.example.test")
	t.Setenv("RUN_LEASE_START_DEADLINE", "6m")
	t.Setenv("RUN_LEASE_TTL", "5m")

	if _, err := LoadDispatcher(); err == nil {
		t.Fatal("expected Run Lease policy error")
	}
}

func TestLoadDispatcherRejectsRunLeaseTTLBelowWorkerContract(t *testing.T) {
	setDispatcherFencing(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "https://clickhouse.example.test")
	t.Setenv("RUN_LEASE_START_DEADLINE", "10s")
	t.Setenv("RUN_LEASE_TTL", "29s")
	if _, err := LoadDispatcher(); err == nil {
		t.Fatal("LoadDispatcher() accepted a Run Lease TTL below the worker contract")
	}
}

func TestLoadDispatcherRejectsInvalidWorkspaceFencingKey(t *testing.T) {
	setDispatcherFencing(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "https://clickhouse.example.test")
	t.Setenv("WORKSPACE_FENCING_KEY", "AQ==")

	if _, err := LoadDispatcher(); err == nil {
		t.Fatal("expected Workspace fencing key error")
	}
}

func setDispatcherFencing(t *testing.T) {
	t.Helper()
	t.Setenv("WORKSPACE_FENCING_KEY", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=")
}

func TestLoadControlPlaneReadsRequiredConfig(t *testing.T) {
	setControlPlaneTokenCredentialEnv(t)
	t.Setenv("DATABASE_URL", " postgres://example\n")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("DEPLOYMENT_MODE", " managed-cloud ")
	t.Setenv("REDIS_URL", "\nredis://redis.example.test:6379/0 ")
	t.Setenv("CLICKHOUSE_URL", " https://clickhouse.example.test ")
	t.Setenv("CLICKHOUSE_USER", " telemetry ")
	t.Setenv("CLICKHOUSE_PASSWORD", "clickhouse-password")
	t.Setenv("CAS_URI", " s3://helmr-cas ")
	t.Setenv("BUILD_POLICY_PATH", " /etc/helmr/build-policy.json ")
	t.Setenv("PLATFORM_STORE_URI", " s3://helmr-cas/runtimes ")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("SETUP_TOKEN", "setup-token")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("WORKSPACE_FENCING_KEY", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=")
	t.Setenv("PUBLIC_URL", " https://helmr.example.test ")
	t.Setenv("API_ORIGIN", " https://API.HELMR.EXAMPLE.TEST/ ")
	t.Setenv("MAGIC_LINK_DEBUG_URLS", " true ")
	t.Setenv("SMTP_ADDR", " smtp.example.test:587 ")
	t.Setenv("SMTP_USERNAME", " smtp-user ")
	t.Setenv("SMTP_PASSWORD", "smtp-password")
	t.Setenv("EMAIL_FROM", " Helmr <noreply@example.test> ")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", " client-id ")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")
	t.Setenv("RUN_LEASE_TTL", " 4m ")
	t.Setenv("RUN_FINALIZATION_TTL", " 45m ")

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://example" || cfg.DeploymentMode != "managed-cloud" || cfg.RedisURL != "redis://redis.example.test:6379/0" || cfg.ClickHouseURL != "https://clickhouse.example.test" || cfg.ClickHouseUser != "telemetry" || cfg.ClickHousePassword != "clickhouse-password" || cfg.CASURI != "s3://helmr-cas" || cfg.BuildPolicyPath != "/etc/helmr/build-policy.json" || cfg.PlatformStoreURI != "s3://helmr-cas/runtimes" || !bytes.Equal(cfg.WorkerTokenSigningKey, bytes.Repeat([]byte{1}, 32)) || cfg.SetupToken != "setup-token" || !bytes.Equal(cfg.AuthKey, bytes.Repeat([]byte{4}, 32)) || !bytes.Equal(cfg.EncryptionKey, make([]byte, 32)) || !bytes.Equal(cfg.WorkspaceFencingKey, bytes.Repeat([]byte{2}, 32)) || !bytes.Equal(cfg.TokenCredentialKey, bytes.Repeat([]byte{3}, 32)) || cfg.PublicURL != "https://helmr.example.test" || cfg.APIOrigin != "https://api.helmr.example.test" || !cfg.MagicLinkDebugURLs || cfg.EmailProvider != EmailProviderSMTP || cfg.SMTPAddr != "smtp.example.test:587" || cfg.SMTPUsername != "smtp-user" || cfg.SMTPPassword != "smtp-password" || cfg.EmailFrom != "Helmr <noreply@example.test>" || cfg.GitHubOAuthClientID != "client-id" || cfg.GitHubOAuthClientSecret != "client-secret" || cfg.RunLeaseTTL != 4*time.Minute || cfg.RunFinalizationTTL != 45*time.Minute {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadControlPlaneDefaultsToSelfHostedDeploymentMode(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("SETUP_TOKEN", "setup-token")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RunLeaseTTL != 5*time.Minute {
		t.Fatalf("Run Lease TTL = %s", cfg.RunLeaseTTL)
	}
	if cfg.RunFinalizationTTL != 30*time.Minute {
		t.Fatalf("Run finalization TTL = %s", cfg.RunFinalizationTTL)
	}
}

func TestLoadControlPlaneRejectsNonPositiveRunLeaseTTL(t *testing.T) {
	t.Setenv("RUN_LEASE_TTL", "0")
	if _, err := LoadControlPlane(); err == nil || !strings.Contains(err.Error(), "RUN_LEASE_TTL") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadControlPlaneRejectsRunLeaseTTLBelowWorkerContract(t *testing.T) {
	t.Setenv("RUN_LEASE_TTL", "29s")
	if _, err := LoadControlPlane(); err == nil || !strings.Contains(err.Error(), "RUN_LEASE_TTL") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadControlPlaneRejectsNonPositiveRunFinalizationTTL(t *testing.T) {
	t.Setenv("RUN_FINALIZATION_TTL", "0")
	if _, err := LoadControlPlane(); err == nil || !strings.Contains(err.Error(), "RUN_FINALIZATION_TTL") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadControlPlaneRejectsFinalizationTTLBelowWorkerContract(t *testing.T) {
	t.Setenv("RUN_FINALIZATION_TTL", "19m")
	if _, err := LoadControlPlane(); err == nil || !strings.Contains(err.Error(), "RUN_FINALIZATION_TTL") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadControlPlaneRequiresManagedRuntimeConfig(t *testing.T) {
	for _, variable := range []string{
		"BUILD_POLICY_PATH",
		"PLATFORM_STORE_URI",
	} {
		t.Run(variable, func(t *testing.T) {
			setControlPlaneRequiredEnv(t)
			t.Setenv("DEPLOYMENT_MODE", "managed-cloud")
			t.Setenv("DATABASE_URL", "postgres://example")
			t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
			t.Setenv("CAS_URI", "s3://helmr-cas")
			t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
			t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
			t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
			t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")
			t.Setenv(variable, "")

			_, err := LoadControlPlane()
			if err == nil || !strings.Contains(err.Error(), variable+" is required") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadControlPlaneRequiresSetupTokenForSelfHosted(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	_, err := LoadControlPlane()
	if err == nil {
		t.Fatal("expected missing setup token error")
	}
	if got, want := err.Error(), "SETUP_TOKEN is required"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}

	t.Setenv("SETUP_TOKEN", " setup-token")
	_, err = LoadControlPlane()
	if err == nil || !strings.Contains(err.Error(), "SETUP_TOKEN must not have surrounding whitespace") {
		t.Fatalf("whitespace error = %v", err)
	}
}

func TestLoadControlPlaneRejectsInvalidDeploymentMode(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("DEPLOYMENT_MODE", "unknown")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("SETUP_TOKEN", "setup-token")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	_, err := LoadControlPlane()
	if err == nil {
		t.Fatal("expected invalid deployment mode error")
	}
	if got, want := err.Error(), "DEPLOYMENT_MODE"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlPlaneRejectsInvalidWorkerSigningKey(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "short")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	_, err := LoadControlPlane()
	if err == nil {
		t.Fatal("expected weak worker signing key error")
	}
	if got, want := err.Error(), "WORKER_TOKEN_SIGNING_KEY "; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlPlaneRejectsInvalidAuthKey(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("AUTH_KEY", "short")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	_, err := LoadControlPlane()
	if err == nil {
		t.Fatal("expected weak auth secret error")
	}
	if got, want := err.Error(), "AUTH_KEY "; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlPlaneAllowsHTTPOnlyForLoopbackPublicURL(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("SETUP_TOKEN", "setup-token")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("PUBLIC_URL", "http://127.0.0.1:8080")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	if _, err := LoadControlPlane(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PUBLIC_URL", "http://helmr.example.test")
	_, err := LoadControlPlane()
	if err == nil {
		t.Fatal("expected public HTTP URL error")
	}
	if got, want := err.Error(), "PUBLIC_URL must use https"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlPlaneDefaultsPublicURL(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("SETUP_TOKEN", "setup-token")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != DefaultPublicURL {
		t.Fatalf("public URL = %q", cfg.PublicURL)
	}
	if cfg.APIOrigin != cfg.PublicURL {
		t.Fatalf("API origin = %q, public URL = %q", cfg.APIOrigin, cfg.PublicURL)
	}
}

func TestLoadControlPlaneRejectsNonOriginPublicAndAPIURLs(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{"PUBLIC_URL", "https://helmr.example.test/console"},
		{"API_ORIGIN", "https://api.helmr.example.test/v1"},
		{"API_ORIGIN", "https://user@api.helmr.example.test"},
	} {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			setControlPlaneRequiredEnv(t)
			t.Setenv("DATABASE_URL", "postgres://example")
			t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
			t.Setenv("CAS_URI", "s3://helmr-cas")
			t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
			t.Setenv("SETUP_TOKEN", "setup-token")
			t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
			t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
			t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")
			t.Setenv(test.name, test.value)
			if _, err := LoadControlPlane(); err == nil || !strings.Contains(err.Error(), "must be an origin") {
				t.Fatalf("LoadControlPlane() error = %v", err)
			}
		})
	}
}

func TestLoadControlPlaneRejectsInvalidMagicLinkDebugURLs(t *testing.T) {
	t.Setenv("MAGIC_LINK_DEBUG_URLS", "sometimes")

	_, err := LoadControlPlane()
	if err == nil {
		t.Fatal("expected invalid magic link debug URLs error")
	}
	if got, want := err.Error(), "MAGIC_LINK_DEBUG_URLS must be a boolean"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlPlaneRequiresCompleteSMTPConfig(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	t.Setenv("SMTP_ADDR", "smtp.example.test:587")
	if _, err := LoadControlPlane(); err == nil || !strings.Contains(err.Error(), "EMAIL_FROM") {
		t.Fatalf("expected email from error, got %v", err)
	}

	t.Setenv("SMTP_ADDR", "")
	t.Setenv("EMAIL_FROM", "noreply@example.test")
	if _, err := LoadControlPlane(); err == nil || !strings.Contains(err.Error(), "EMAIL_PROVIDER") {
		t.Fatalf("expected email provider error, got %v", err)
	}

	t.Setenv("EMAIL_PROVIDER", "smtp")
	if _, err := LoadControlPlane(); err == nil || !strings.Contains(err.Error(), "SMTP_ADDR") {
		t.Fatalf("expected smtp addr error, got %v", err)
	}
}

func TestLoadControlPlaneReadsResendConfig(t *testing.T) {
	setControlPlaneRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("DEPLOYMENT_MODE", "managed-cloud")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("WORKER_TOKEN_SIGNING_KEY", "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=")
	t.Setenv("AUTH_KEY", "BAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQ=")
	t.Setenv("ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("EMAIL_PROVIDER", "resend")
	t.Setenv("RESEND_API_KEY", "re_test")
	t.Setenv("EMAIL_FROM", "Helmr <noreply@example.test>")
	t.Setenv("GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmailProvider != EmailProviderResend || cfg.ResendAPIKey != "re_test" || cfg.EmailFrom != "Helmr <noreply@example.test>" {
		t.Fatalf("config = %+v", cfg)
	}
}

func setControlPlaneRequiredEnv(t *testing.T) {
	t.Helper()
	setControlPlaneTokenCredentialEnv(t)
	t.Setenv("BUILD_POLICY_PATH", "/etc/helmr/build-policy.json")
	t.Setenv("PLATFORM_STORE_URI", "s3://helmr-cas/runtimes")
	t.Setenv("WORKSPACE_FENCING_KEY", "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI=")
}

func setControlPlaneTokenCredentialEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TOKEN_CREDENTIAL_KEY", "AwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwM=")
}

func setWorkerRuntimeEnv(t *testing.T, build bool) {
	t.Helper()
	setWorkerEnrollmentEnv(t)
	t.Setenv("PLATFORM_STORE_URI", "s3://helmr-runtime")
	t.Setenv("WORKER_NETWORK_LINK_POOL", "169.254.64.0/18")
	t.Setenv("WORKER_NETWORK_TRANSLATION_POOL", "100.96.0.0/16")
	t.Setenv("WORKER_NETWORK_RESOLVER_IPV4", "1.1.1.1")
	t.Setenv("WORKER_NETWORK_BLOCKED_IPV4_CIDRS", "[]")
	if build {
		t.Setenv("BUILD_POLICY_PATH", "/etc/helmr/build-policy.json")
		t.Setenv("WORKER_BUILD_CACHE_DIR", "/var/lib/helmr/cache")
		t.Setenv("WORKER_BUILD_SCRATCH_DIR", "/var/lib/helmr/scratch")
		t.Setenv("WORKER_SUBSTRATE_CACHE_MAX_MIB", "8192")
		t.Setenv("WORKER_ARTIFACT_CACHE_MAX_MIB", "4096")
	}
}

func setWorkerEnrollmentEnv(t *testing.T) {
	t.Helper()
	secretFile := t.TempDir() + "/worker-enrollment-token"
	if err := os.WriteFile(secretFile, []byte("hlmr_wgt_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKER_RESOURCE_ID", "host-1")
	t.Setenv("WORKER_ENROLLMENT_TOKEN_FILE", secretFile)
}

func setValidWorkerEnv(t *testing.T, build bool) {
	t.Helper()
	setWorkerRuntimeEnv(t, build)
	t.Setenv("CONTROL_PLANE_URL", "https://api.example.test")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("CHECKPOINT_ENCRYPTION_KEY", "BQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU=")
	t.Setenv("JAILER_UID", "1001")
	t.Setenv("JAILER_GID", "1002")
	if build {
		t.Setenv("WORKER_ROLES", "build")
	} else {
		t.Setenv("WORKER_ROLES", "run")
	}
}

func TestLoadWorkerRequiresBuildStorageConfig(t *testing.T) {
	for _, variable := range []string{
		"PLATFORM_STORE_URI",
		"WORKER_BUILD_CACHE_DIR",
		"WORKER_BUILD_SCRATCH_DIR",
		"WORKER_SUBSTRATE_CACHE_MAX_MIB",
		"WORKER_ARTIFACT_CACHE_MAX_MIB",
	} {
		t.Run(variable, func(t *testing.T) {
			setValidWorkerEnv(t, true)
			t.Setenv(variable, "")
			if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), variable) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadWorkerRequiresPlatformStoreForRunWorkers(t *testing.T) {
	setValidWorkerEnv(t, false)
	t.Setenv("PLATFORM_STORE_URI", "")
	if _, err := LoadWorker(); err == nil ||
		!strings.Contains(err.Error(), "PLATFORM_STORE_URI") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadDatabaseOnlyRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "\npostgres://example ")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-cas")

	cfg, err := LoadDatabase()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "postgres://example" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerReadsVMConfig(t *testing.T) {
	setWorkerRuntimeEnv(t, true)
	t.Setenv("CONTROL_PLANE_URL", " https://api.example.test ")
	t.Setenv("CAS_URI", "\ns3://helmr-cas")
	t.Setenv("CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("WORKER_WORK_DIR", " /var/lib/helmr/scratch/worker ")
	t.Setenv("WORKER_IMAGES_DIR", " /var/lib/helmr/images ")
	t.Setenv("GIT_PATH", " /usr/bin/git ")
	t.Setenv("FIRECRACKER_PATH", " /usr/bin/firecracker ")
	t.Setenv("JAILER_PATH", " /usr/bin/jailer ")
	t.Setenv("JAILER_UID", " 1001 ")
	t.Setenv("JAILER_GID", " 1002 ")
	t.Setenv("JAILER_NUMA_NODE", " 1 ")
	t.Setenv("JAILER_CHROOT_DIR", " /var/lib/helmr/scratch/jailer ")
	t.Setenv("JAILER_CGROUP_VERSION", " 2 ")
	t.Setenv("WORKER_NETWORK_LINK_POOL", " 169.254.128.0/18 ")
	t.Setenv("WORKER_NETWORK_TRANSLATION_POOL", " 100.97.0.0/16 ")
	t.Setenv("WORKER_NETWORK_RESOLVER_IPV4", " 1.0.0.1 ")
	t.Setenv("WORKER_NETWORK_BLOCKED_IPV4_CIDRS", `["10.0.0.0/8","169.254.0.0/16"]`)
	t.Setenv("IP_PATH", " /usr/sbin/ip ")
	t.Setenv("NFT_PATH", " /usr/sbin/nft ")
	t.Setenv("VM_VCPUS", " 4 ")
	t.Setenv("VM_MEMORY_MIB", " 4096 ")
	t.Setenv("VM_SCRATCH_DISK_MIB", " 12288 ")
	t.Setenv("WORKER_CAPACITY_VCPUS", " 8 ")
	t.Setenv("WORKER_CAPACITY_MEMORY_MIB", " 16384 ")
	t.Setenv("WORKER_DISK_RESERVE_MIB", " 2048 ")
	t.Setenv("WORKER_SUBSTRATE_CACHE_MAX_MIB", " 32768 ")
	t.Setenv("WORKER_ARTIFACT_CACHE_MAX_MIB", " 16384 ")
	t.Setenv("WORKER_EXECUTION_SLOTS", " 4 ")
	t.Setenv("WORKER_ROLES", " build,run ")
	t.Setenv("VM_INIT_TIMEOUT", " 45s ")
	t.Setenv("VM_HEALTH_TIMEOUT", " 90s ")
	t.Setenv("VM_HEALTH_ATTEMPT_TIMEOUT", " 7s ")
	t.Setenv("WORKSPACE_MOUNT_STARTUP_TIMEOUT", " 3m ")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CASURI != "s3://helmr-cas" || cfg.WorkDir != "/var/lib/helmr/scratch/worker" || cfg.BuildCacheDir != "/var/lib/helmr/cache" || cfg.BuildScratchDir != "/var/lib/helmr/scratch" || cfg.ImagesDir != "/var/lib/helmr/images" || cfg.GitPath != "/usr/bin/git" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.FirecrackerPath != "/usr/bin/firecracker" || cfg.NetworkLinkPool != "169.254.128.0/18" || cfg.NetworkTranslationPool != "100.97.0.0/16" || cfg.NetworkResolverIPv4 != "1.0.0.1" || cfg.VMVCPUCount != 4 || cfg.VMMemoryMiB != 4096 || cfg.VMScratchDiskMiB != 12288 || cfg.WorkerCapacityVCPUs != 8 || cfg.WorkerCapacityMemoryMiB != 16384 || cfg.WorkerDiskReserveMiB != 2048 || cfg.SubstrateCacheMaxMiB != 32768 || cfg.ArtifactCacheMaxMiB != 16384 || cfg.WorkerExecutionSlots != 4 || cfg.VMInitTimeout != 45*time.Second || cfg.VMHealthTimeout != 90*time.Second || cfg.VMHealthAttemptTimeout != 7*time.Second || cfg.WorkspaceMountStartupTimeout != 3*time.Minute {
		t.Fatalf("config = %+v", cfg)
	}
	if len(cfg.NetworkBlockedIPv4CIDRs) != 2 || cfg.NetworkBlockedIPv4CIDRs[1].String() != "169.254.0.0/16" {
		t.Fatalf("blocked IPv4 CIDRs = %v", cfg.NetworkBlockedIPv4CIDRs)
	}
	if cfg.JailerPath != "/usr/bin/jailer" || cfg.JailerUID != 1001 || cfg.JailerGID != 1002 || cfg.JailerNumaNode != 1 || cfg.JailerChrootDir != "/var/lib/helmr/scratch/jailer" || cfg.CgroupVersion != "2" || cfg.IPPath != "/usr/sbin/ip" || cfg.NFTPath != "/usr/sbin/nft" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.BuildPolicyPath != "/etc/helmr/build-policy.json" || cfg.PlatformStoreURI != "s3://helmr-runtime" {
		t.Fatalf("config = %+v", cfg)
	}
	if !bytes.Equal(cfg.CheckpointKey, make([]byte, 32)) {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerRequiresExplicitCanonicalBlockedIPv4CIDRs(t *testing.T) {
	for _, raw := range []string{"", `null`, `["10.0.0.1/8"]`, `["169.254.0.0/16","10.0.0.0/8"]`} {
		t.Run(raw, func(t *testing.T) {
			setValidWorkerEnv(t, false)
			t.Setenv("WORKER_NETWORK_BLOCKED_IPV4_CIDRS", raw)
			if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), "WORKER_NETWORK_BLOCKED_IPV4_CIDRS") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadWorkerReadsEnrollmentBoundary(t *testing.T) {
	setWorkerRuntimeEnv(t, true)
	t.Setenv("CONTROL_PLANE_URL", "https://controlplane.example.test")
	t.Setenv("CAS_URI", "s3://cas")
	t.Setenv("CHECKPOINT_ENCRYPTION_KEY", "BQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU=")
	t.Setenv("JAILER_UID", "1001")
	t.Setenv("JAILER_GID", "1001")
	t.Setenv("WORKER_ROLES", "build,run")
	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerEnrollmentTokenFile != os.Getenv("WORKER_ENROLLMENT_TOKEN_FILE") {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerDoesNotRequireEnrollmentTokenFileToExistAtStartup(t *testing.T) {
	setValidWorkerEnv(t, false)
	secretFile := t.TempDir() + "/not-yet-materialized"
	t.Setenv("WORKER_ENROLLMENT_TOKEN_FILE", secretFile)
	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerEnrollmentTokenFile != secretFile {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerReadsExplicitRolesAndCapacities(t *testing.T) {
	setWorkerRuntimeEnv(t, false)
	t.Setenv("CONTROL_PLANE_URL", "https://api.example.test")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("CHECKPOINT_ENCRYPTION_KEY", "BQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU=")
	t.Setenv("JAILER_UID", "1001")
	t.Setenv("JAILER_GID", "1002")
	t.Setenv("WORKER_ROLES", "run")
	t.Setenv("WORKER_EXECUTION_SLOTS", "4")
	t.Setenv("WORKER_RUNTIME_STARTS", "2")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if !stringSlicesEqual(cfg.WorkerRoles, []string{"run"}) || cfg.WorkerBuildExecutors != 0 || cfg.WorkerRuntimeStarts != 2 || cfg.PreparedRuntimePoolSize != 2 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerRejectsMultipleBuildExecutors(t *testing.T) {
	setWorkerEnrollmentEnv(t)
	for key, value := range map[string]string{
		"CONTROL_PLANE_URL":                 "https://api.example.test",
		"CAS_URI":                           "s3://helmr-cas",
		"CHECKPOINT_ENCRYPTION_KEY":         "BQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU=",
		"JAILER_UID":                        "1001",
		"JAILER_GID":                        "1002",
		"WORKER_ROLES":                      "build",
		"WORKER_BUILD_EXECUTORS":            "2",
		"WORKER_CAPACITY_VCPUS":             "4",
		"WORKER_CAPACITY_MEMORY_MIB":        "8192",
		"WORKER_SUBSTRATE_CACHE_MAX_MIB":    "4096",
		"WORKER_ARTIFACT_CACHE_MAX_MIB":     "2048",
		"WORKER_NETWORK_BLOCKED_IPV4_CIDRS": "[]",
	} {
		t.Setenv(key, value)
	}
	if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), "zero or one") {
		t.Fatalf("LoadWorker() error = %v", err)
	}
}

func TestLoadWorkerRejectsRuntimePoolBelowRuntimeStarts(t *testing.T) {
	setWorkerEnrollmentEnv(t)
	for key, value := range map[string]string{"CONTROL_PLANE_URL": "https://api.example.test", "CAS_URI": "s3://helmr-cas", "CHECKPOINT_ENCRYPTION_KEY": "BQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU=", "JAILER_UID": "1001", "JAILER_GID": "1002", "WORKER_ROLES": "run", "WORKER_RUNTIME_STARTS": "2", "WORKER_PREPARED_RUNTIME_POOL_SIZE": "1"} {
		t.Setenv(key, value)
	}
	if _, err := LoadWorker(); err == nil {
		t.Fatal("undersized runtime pool accepted")
	}
}

func TestLoadWorkerRejectsEmptyOrUnknownRoles(t *testing.T) {
	for _, roles := range []string{"", ",", "run,other"} {
		t.Run(roles, func(t *testing.T) {
			setWorkerEnrollmentEnv(t)
			t.Setenv("CONTROL_PLANE_URL", "https://api.example.test")
			t.Setenv("CAS_URI", "s3://helmr-cas")
			t.Setenv("CHECKPOINT_ENCRYPTION_KEY", "BQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU=")
			t.Setenv("JAILER_UID", "1001")
			t.Setenv("JAILER_GID", "1002")
			t.Setenv("WORKER_ROLES", roles)
			if _, err := LoadWorker(); err == nil {
				t.Fatal("LoadWorker succeeded")
			}
		})
	}
}

func stringSlicesEqual(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLoadWorkerControlPlaneReadsOnlyControlAuth(t *testing.T) {
	t.Setenv("CONTROL_PLANE_URL", "https://api.example.test")
	t.Setenv("WORKER_INSTANCE_CREDENTIAL_PATH", "/run/helmr/worker-credential.json")

	cfg, err := LoadWorkerControlPlane()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlPlaneURL != "https://api.example.test" || cfg.WorkerInstanceCredentialPath != "/run/helmr/worker-credential.json" || cfg.PollEvery <= 0 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerRejectsInvalidVMNumbers(t *testing.T) {
	setWorkerEnrollmentEnv(t)
	t.Setenv("CONTROL_PLANE_URL", "https://api.example.test")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("JAILER_UID", "1001")
	t.Setenv("JAILER_GID", "1002")
	t.Setenv("VM_MEMORY_MIB", "big")

	_, err := LoadWorker()
	if err == nil {
		t.Fatal("expected invalid memory error")
	}
}

func TestLoadWorkerRejectsHealthAttemptLongerThanHealthTimeout(t *testing.T) {
	setWorkerEnrollmentEnv(t)
	t.Setenv("WORKER_NETWORK_BLOCKED_IPV4_CIDRS", "[]")
	t.Setenv("CONTROL_PLANE_URL", "https://api.example.test")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("JAILER_UID", "1001")
	t.Setenv("JAILER_GID", "1002")
	t.Setenv("WORKER_ROLES", "build,run")
	t.Setenv("VM_HEALTH_TIMEOUT", "5s")
	t.Setenv("VM_HEALTH_ATTEMPT_TIMEOUT", "6s")

	_, err := LoadWorker()
	if err == nil {
		t.Fatal("expected health attempt timeout error")
	}
	if got, want := err.Error(), "VM_HEALTH_ATTEMPT_TIMEOUT"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadWorkerClampsDefaultHealthAttemptToShortHealthTimeout(t *testing.T) {
	setWorkerRuntimeEnv(t, true)
	t.Setenv("CONTROL_PLANE_URL", "https://api.example.test")
	t.Setenv("CAS_URI", "s3://helmr-cas")
	t.Setenv("CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("JAILER_UID", "1001")
	t.Setenv("JAILER_GID", "1002")
	t.Setenv("WORKER_ROLES", "build,run")
	t.Setenv("VM_HEALTH_TIMEOUT", "1s")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VMHealthAttemptTimeout != time.Second {
		t.Fatalf("VMHealthAttemptTimeout = %s, want 1s", cfg.VMHealthAttemptTimeout)
	}
}
