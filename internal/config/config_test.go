package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadControlReadsRequiredConfig(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", " postgres://example\n")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_DEPLOYMENT_MODE", " managed-cloud ")
	t.Setenv("HELMR_WORKER_GROUP_ID", " us-east-1-worker-group-2 ")
	t.Setenv("HELMR_REGION_ID", " us-east-1 ")
	t.Setenv("HELMR_DEFAULT_REGION_ID", " us-east-1 ")
	t.Setenv("HELMR_REDIS_URL", "\nredis://redis.example.test:6379/0 ")
	t.Setenv("HELMR_CLICKHOUSE_URL", " https://clickhouse.example.test ")
	t.Setenv("HELMR_CLICKHOUSE_USER", " telemetry ")
	t.Setenv("HELMR_CLICKHOUSE_PASSWORD", " clickhouse-password ")
	t.Setenv("HELMR_CAS_URI", " s3://helmr-cas ")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "\n01234567890123456789012345678901\n")
	t.Setenv("HELMR_WORKER_GROUPS", ` [{"id":"run-workers"}] `)
	t.Setenv("HELMR_SETUP_TOKEN", " setup-token ")
	t.Setenv("HELMR_AUTH_SECRET", " abcdefghijabcdefghijabcdefghij12 ")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", " AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= ")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY_OLD", " AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE= ")
	t.Setenv("HELMR_PUBLIC_URL", " https://helmr.example.test ")
	t.Setenv("HELMR_MAGIC_LINK_DEBUG_URLS", " true ")
	t.Setenv("HELMR_SMTP_ADDR", " smtp.example.test:587 ")
	t.Setenv("HELMR_SMTP_USERNAME", " smtp-user ")
	t.Setenv("HELMR_SMTP_PASSWORD", " smtp-password ")
	t.Setenv("HELMR_EMAIL_FROM", " Helmr <noreply@example.test> ")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", " client-id ")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", " client-secret ")

	cfg, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://example" || cfg.DeploymentMode != "managed-cloud" || cfg.WorkerGroupID != "us-east-1-worker-group-2" || cfg.RegionID != "us-east-1" || cfg.DefaultRegionID != "us-east-1" || cfg.RedisURL != "redis://redis.example.test:6379/0" || cfg.ClickHouseURL != "https://clickhouse.example.test" || cfg.ClickHouseUser != "telemetry" || cfg.ClickHousePassword != "clickhouse-password" || cfg.CASURI != "s3://helmr-cas" || cfg.WorkerTokenSigningKey != "01234567890123456789012345678901" || cfg.WorkerGroupsJSON != `[{"id":"run-workers"}]` || cfg.SetupToken != "setup-token" || cfg.AuthSecret != "abcdefghijabcdefghijabcdefghij12" || cfg.SecretEncryptionKey != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" || cfg.SecretEncryptionKeyOld != "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" || cfg.PublicURL != "https://helmr.example.test" || !cfg.MagicLinkDebugURLs || cfg.EmailProvider != EmailProviderSMTP || cfg.SMTPAddr != "smtp.example.test:587" || cfg.SMTPUsername != "smtp-user" || cfg.SMTPPassword != "smtp-password" || cfg.EmailFrom != "Helmr <noreply@example.test>" || cfg.GitHubOAuthClientID != "client-id" || cfg.GitHubOAuthClientSecret != "client-secret" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadControlDefaultsToSelfHostedDeploymentMode(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeploymentMode != DeploymentModeSelfHosted {
		t.Fatalf("deployment mode = %q", cfg.DeploymentMode)
	}
	if cfg.WorkerGroupID != "us-east-1-worker-group-1" || cfg.RegionID != "us-east-1" || cfg.DefaultRegionID != "us-east-1" {
		t.Fatalf("worker group config = %+v", cfg)
	}
}

func TestLoadControlRequiresSetupTokenForSelfHosted(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected missing setup token error")
	}
	if got, want := err.Error(), "HELMR_SETUP_TOKEN is required"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlRejectsInvalidDeploymentMode(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_DEPLOYMENT_MODE", "unknown")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected invalid deployment mode error")
	}
	if got, want := err.Error(), "HELMR_DEPLOYMENT_MODE"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlRejectsWeakWorkerSigningKey(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "short")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected weak worker signing key error")
	}
	if got, want := err.Error(), "HELMR_WORKER_TOKEN_SIGNING_KEY:"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlRejectsWeakAuthSecret(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_AUTH_SECRET", "short")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected weak auth secret error")
	}
	if got, want := err.Error(), "HELMR_AUTH_SECRET:"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlAllowsHTTPOnlyForLoopbackPublicURL(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_PUBLIC_URL", "http://127.0.0.1:8080")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	if _, err := LoadControl(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HELMR_PUBLIC_URL", "http://helmr.example.test")
	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected public HTTP URL error")
	}
	if got, want := err.Error(), "HELMR_PUBLIC_URL must use https"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlDefaultsPublicURL(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != DefaultPublicURL {
		t.Fatalf("public URL = %q", cfg.PublicURL)
	}
}

func TestLoadControlRejectsInvalidMagicLinkDebugURLs(t *testing.T) {
	t.Setenv("HELMR_MAGIC_LINK_DEBUG_URLS", "sometimes")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected invalid magic link debug URLs error")
	}
	if got, want := err.Error(), "HELMR_MAGIC_LINK_DEBUG_URLS must be a boolean"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlRequiresCompleteSMTPConfig(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	t.Setenv("HELMR_SMTP_ADDR", "smtp.example.test:587")
	if _, err := LoadControl(); err == nil || !strings.Contains(err.Error(), "HELMR_EMAIL_FROM") {
		t.Fatalf("expected email from error, got %v", err)
	}

	t.Setenv("HELMR_SMTP_ADDR", "")
	t.Setenv("HELMR_EMAIL_FROM", "noreply@example.test")
	if _, err := LoadControl(); err == nil || !strings.Contains(err.Error(), "HELMR_EMAIL_PROVIDER") {
		t.Fatalf("expected email provider error, got %v", err)
	}

	t.Setenv("HELMR_EMAIL_PROVIDER", "smtp")
	if _, err := LoadControl(); err == nil || !strings.Contains(err.Error(), "HELMR_SMTP_ADDR") {
		t.Fatalf("expected smtp addr error, got %v", err)
	}
}

func TestLoadControlReadsResendConfig(t *testing.T) {
	setControlWorkerGroupEnv(t)
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_DEPLOYMENT_MODE", "managed-cloud")
	t.Setenv("HELMR_WORKER_GROUPS", `[{"id":"run-workers"}]`)
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_EMAIL_PROVIDER", "resend")
	t.Setenv("HELMR_RESEND_API_KEY", "\nre_test\n")
	t.Setenv("HELMR_EMAIL_FROM", "Helmr <noreply@example.test>")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_OAUTH_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmailProvider != EmailProviderResend || cfg.ResendAPIKey != "re_test" || cfg.EmailFrom != "Helmr <noreply@example.test>" {
		t.Fatalf("config = %+v", cfg)
	}
}

func setControlWorkerGroupEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HELMR_WORKER_GROUP_ID", "us-east-1-worker-group-1")
	t.Setenv("HELMR_REGION_ID", "us-east-1")
	t.Setenv("HELMR_DEFAULT_REGION_ID", "us-east-1")
	t.Setenv("HELMR_WORKER_GROUPS", `[{"id":"run-workers"}]`)
}

func TestLoadDatabaseOnlyRequiresDatabaseURL(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "\npostgres://example ")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")

	cfg, err := LoadDatabase()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "postgres://example" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerReadsVMConfig(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", " https://api.example.test ")
	t.Setenv("HELMR_CAS_URI", "\ns3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", " run-workers ")
	t.Setenv("HELMR_WORKER_PROVIDER_REGION", " us-east-1 ")
	t.Setenv("HELMR_WORKER_LABELS", "pool=standard,dedicated_key=tenant-a")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", " AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= ")
	t.Setenv("HELMR_WORKER_WORK_DIR", " /var/lib/helmr ")
	t.Setenv("HELMR_WORKER_IMAGES_DIR", " /var/lib/helmr/images ")
	t.Setenv("HELMR_GIT_PATH", " /usr/bin/git ")
	t.Setenv("HELMR_WORKER_BUILDKIT_ADDR", " unix:///run/helmr/buildkit/buildkitd.sock ")
	t.Setenv("HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE", " helmr-ci ")
	t.Setenv("HELMR_WORKER_FIRECRACKER_PATH", " /usr/bin/firecracker ")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_PATH", " /usr/bin/jailer ")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", " 1001 ")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", " 1002 ")
	t.Setenv("HELMR_WORKER_FIRECRACKER_NUMA_NODE", " 1 ")
	t.Setenv("HELMR_WORKER_FIRECRACKER_CHROOT_DIR", " /var/lib/helmr/jailer ")
	t.Setenv("HELMR_WORKER_FIRECRACKER_CGROUP_VERSION", " 2 ")
	t.Setenv("HELMR_WORKER_CNI_NETWORK", " helmr-ci ")
	t.Setenv("HELMR_WORKER_CNI_PROFILE", " helmr-ci/v2 ")
	t.Setenv("HELMR_WORKER_CNI_CONF_DIR", " /etc/helmr/cni ")
	t.Setenv("HELMR_WORKER_CNI_BIN_DIR", " /opt/helmr/cni/bin ")
	t.Setenv("HELMR_WORKER_CNI_CACHE_DIR", " /var/lib/helmr/cni ")
	t.Setenv("HELMR_WORKER_IP_PATH", " /usr/sbin/ip ")
	t.Setenv("HELMR_WORKER_NFT_PATH", " /usr/sbin/nft ")
	t.Setenv("HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS", "10.0.0.0/8,169.254.0.0/16")
	t.Setenv("HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS", "fc00::/7 fe80::/10")
	t.Setenv("HELMR_VM_VCPUS", " 4 ")
	t.Setenv("HELMR_VM_MEMORY_MIB", " 4096 ")
	t.Setenv("HELMR_VM_SCRATCH_DISK_MIB", " 12288 ")
	t.Setenv("HELMR_WORKER_CAPACITY_VCPUS", " 8 ")
	t.Setenv("HELMR_WORKER_CAPACITY_MEMORY_MIB", " 16384 ")
	t.Setenv("HELMR_WORKER_DISK_RESERVE_MIB", " 2048 ")
	t.Setenv("HELMR_WORKER_SUBSTRATE_CACHE_MAX_MIB", " 32768 ")
	t.Setenv("HELMR_WORKER_ARTIFACT_CACHE_MAX_MIB", " 16384 ")
	t.Setenv("HELMR_WORKER_EXECUTION_SLOTS", " 4 ")
	t.Setenv("HELMR_WORKER_ROLES", " build,run ")
	t.Setenv("HELMR_WORKER_CERTIFICATION_TTL", " 12h ")
	t.Setenv("HELMR_VM_HEALTH_TIMEOUT", " 90s ")
	t.Setenv("HELMR_VM_HEALTH_ATTEMPT_TIMEOUT", " 7s ")
	t.Setenv("HELMR_WORKSPACE_MOUNT_STARTUP_TIMEOUT", " 3m ")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CASURI != "s3://helmr-cas" || cfg.WorkDir != "/var/lib/helmr" || cfg.ImagesDir != "/var/lib/helmr/images" || cfg.GitPath != "/usr/bin/git" || cfg.BuildKitAddr != "unix:///run/helmr/buildkit/buildkitd.sock" || cfg.BuildKitCacheNS != "helmr-ci" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.FirecrackerPath != "/usr/bin/firecracker" || cfg.CNINetworkName != "helmr-ci" || cfg.CNIConfDir != "/etc/helmr/cni" || cfg.CNIBinDir != "/opt/helmr/cni/bin" || cfg.CNICacheDir != "/var/lib/helmr/cni" || cfg.VMVCPUCount != 4 || cfg.VMMemoryMiB != 4096 || cfg.VMScratchDiskMiB != 12288 || cfg.WorkerCapacityVCPUs != 8 || cfg.WorkerCapacityMemoryMiB != 16384 || cfg.WorkerDiskReserveMiB != 2048 || cfg.SubstrateCacheMaxMiB != 32768 || cfg.ArtifactCacheMaxMiB != 16384 || cfg.WorkerExecutionSlots != 4 || cfg.WorkerCertificationTTL != 12*time.Hour || cfg.VMHealthTimeout != 90*time.Second || cfg.VMHealthAttemptTimeout != 7*time.Second || cfg.WorkspaceMountStartupTimeout != 3*time.Minute {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.JailerPath != "/usr/bin/jailer" || cfg.JailerUID != 1001 || cfg.JailerGID != 1002 || cfg.JailerNumaNode != 1 || cfg.JailerChrootDir != "/var/lib/helmr/jailer" || cfg.CgroupVersion != "2" || cfg.CNIProfile != "helmr-ci/v2" || cfg.IPPath != "/usr/sbin/ip" || cfg.NFTPath != "/usr/sbin/nft" {
		t.Fatalf("config = %+v", cfg)
	}
	if !stringSlicesEqual(cfg.NetworkBlockedIPv4CIDRs, []string{"10.0.0.0/8", "169.254.0.0/16"}) || !stringSlicesEqual(cfg.NetworkBlockedIPv6CIDRs, []string{"fc00::/7", "fe80::/10"}) {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.WorkerGroupID != "run-workers" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.WorkerProviderRegion != "us-east-1" || cfg.WorkerLabels["pool"] != "standard" || cfg.WorkerLabels["dedicated_key"] != "tenant-a" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.CheckpointKey == "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerAllowsEmptyNetworkBlockedCIDRs(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_WORKER_PROVIDER_REGION", "us-east-1")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "checkpoint-key")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_WORKER_ROLES", "build,run")
	t.Setenv("HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS", "none")
	t.Setenv("HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS", "none")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NetworkBlockedIPv4CIDRs == nil || len(cfg.NetworkBlockedIPv4CIDRs) != 0 || cfg.NetworkBlockedIPv6CIDRs == nil || len(cfg.NetworkBlockedIPv6CIDRs) != 0 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerRequiresGroup(t *testing.T) {
	if _, err := LoadWorker(); err == nil || !strings.Contains(err.Error(), "HELMR_WORKER_GROUP_ID") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadWorkerReadsEnrollmentBoundary(t *testing.T) {
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_CONTROL_URL", "https://control.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://cas")
	t.Setenv("HELMR_WORKER_PROVIDER_REGION", "us-east-1")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "checkpoint-key")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1001")
	t.Setenv("HELMR_WORKER_ROLES", "build,run")
	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkerGroupID != "run-workers" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerReadsExplicitRolesAndCapacities(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_WORKER_PROVIDER_REGION", "us-east-1")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "checkpoint-key")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_WORKER_ROLES", "run")
	t.Setenv("HELMR_WORKER_EXECUTION_SLOTS", "4")
	t.Setenv("HELMR_WORKER_RUNTIME_STARTS", "2")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if !stringSlicesEqual(cfg.WorkerRoles, []string{"run"}) || cfg.WorkerBuildExecutors != 0 || cfg.WorkerRuntimeStarts != 2 || cfg.PreparedRuntimePoolSize != 2 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerRejectsRuntimePoolBelowRuntimeStarts(t *testing.T) {
	for key, value := range map[string]string{"HELMR_CONTROL_URL": "https://api.example.test", "HELMR_CAS_URI": "s3://helmr-cas", "HELMR_WORKER_GROUP_ID": "run-workers", "HELMR_WORKER_PROVIDER_REGION": "us-east-1", "HELMR_CHECKPOINT_ENCRYPTION_KEY": "checkpoint-key", "HELMR_WORKER_FIRECRACKER_JAILER_UID": "1001", "HELMR_WORKER_FIRECRACKER_JAILER_GID": "1002", "HELMR_WORKER_ROLES": "run", "HELMR_WORKER_RUNTIME_STARTS": "2", "HELMR_WORKER_PREPARED_RUNTIME_POOL_SIZE": "1"} {
		t.Setenv(key, value)
	}
	if _, err := LoadWorker(); err == nil {
		t.Fatal("undersized runtime pool accepted")
	}
}

func TestLoadWorkerRejectsEmptyOrUnknownRoles(t *testing.T) {
	for _, roles := range []string{"", ",", "run,other"} {
		t.Run(roles, func(t *testing.T) {
			t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
			t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
			t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
			t.Setenv("HELMR_WORKER_PROVIDER_REGION", "us-east-1")
			t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "checkpoint-key")
			t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
			t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
			t.Setenv("HELMR_WORKER_ROLES", roles)
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

func TestLoadWorkerControlReadsOnlyControlAuth(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_WORKER_INSTANCE_CREDENTIAL_PATH", "/run/helmr/worker-credential.json")

	cfg, err := LoadWorkerControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlURL != "https://api.example.test" || cfg.WorkerInstanceCredentialPath != "/run/helmr/worker-credential.json" || cfg.PollEvery <= 0 {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerDoesNotReadGenericBuildKitHost(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_WORKER_PROVIDER_REGION", "us-east-1")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_WORKER_ROLES", "build,run")
	t.Setenv("BUILDKIT_HOST", "tcp://buildkit.example.test:1234")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BuildKitAddr != "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerRejectsInvalidVMNumbers(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_VM_MEMORY_MIB", "big")

	_, err := LoadWorker()
	if err == nil {
		t.Fatal("expected invalid memory error")
	}
}

func TestLoadWorkerRejectsHealthAttemptLongerThanHealthTimeout(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_WORKER_ROLES", "build,run")
	t.Setenv("HELMR_VM_HEALTH_TIMEOUT", "5s")
	t.Setenv("HELMR_VM_HEALTH_ATTEMPT_TIMEOUT", "6s")

	_, err := LoadWorker()
	if err == nil {
		t.Fatal("expected health attempt timeout error")
	}
	if got, want := err.Error(), "HELMR_VM_HEALTH_ATTEMPT_TIMEOUT"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadWorkerClampsDefaultHealthAttemptToShortHealthTimeout(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_WORKER_PROVIDER_REGION", "us-east-1")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_WORKER_ROLES", "build,run")
	t.Setenv("HELMR_VM_HEALTH_TIMEOUT", "1s")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VMHealthAttemptTimeout != time.Second {
		t.Fatalf("VMHealthAttemptTimeout = %s, want 1s", cfg.VMHealthAttemptTimeout)
	}
}

func TestLoadWorkerRequiresProviderRegion(t *testing.T) {
	t.Setenv("HELMR_DEPLOYMENT_MODE", DeploymentModeManagedCloud)
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_WORKER_ROLES", "build,run")

	_, err := LoadWorker()
	if err == nil {
		t.Fatal("expected provider region error")
	}
	if got, want := err.Error(), "HELMR_WORKER_PROVIDER_REGION"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadWorkerRejectsInvalidLabels(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_GROUP_ID", "run-workers")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_WORKER_LABELS", "pool")

	_, err := LoadWorker()
	if err == nil {
		t.Fatal("expected invalid label error")
	}
	if got, want := err.Error(), "HELMR_WORKER_LABELS"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestWorkerFleetConfigIsStrict(t *testing.T) {
	raw := `[{
		"group_id":"run-workers","role":"run","autoscaling_group":"helmr-run",
		"compatibility_keys":["run-workers"],
		"instance_capacity":{"milli_cpu":8000,"memory_bytes":17179869184,"workload_disk_bytes":1000000000,
		"scratch_bytes":2000000000,"build_cache_bytes":0,"artifact_cache_bytes":0,"vm_slots":4,"build_executors":0},
		"min_workers":0,"warm_workers":0,"max_workers":4,"max_scale_out_per_cycle":2,"queued_run_scratch_bytes":500000000,
		"max_pending_workers":2,"max_packing_items":10000,"scale_out_cooldown_seconds":10,
		"scale_in_cooldown_seconds":60,"scale_in_hysteresis_seconds":300,"stale_worker_timeout_seconds":120,
		"readiness_timeout_seconds":600,"drain_timeout_seconds":900,
		"controller_interval_seconds":5,"metric_interval_seconds":60
	}]`
	fleets, err := parseWorkerFleets(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(fleets) != 1 || fleets[0].CompatibilityKeys[0] != "run-workers" || fleets[0].MaxWorkers != 4 {
		t.Fatalf("fleets = %#v", fleets)
	}
	if _, err := parseWorkerFleets(strings.Replace(raw, `"max_pending_workers":2`, `"max_pending_workers":0`, 1)); err != nil {
		t.Fatalf("zero max_pending_workers: %v", err)
	}
	for _, invalid := range []string{
		strings.Replace(raw, `"max_workers":4`, `"max_workers":0`, 1),
		strings.Replace(raw, `"role":"run"`, `"role":"both"`, 1),
		strings.Replace(raw, `"group_id":"run-workers"`, `"group_id":"run-workers","unknown":true`, 1),
	} {
		if _, err := parseWorkerFleets(invalid); err == nil {
			t.Fatalf("parseWorkerFleets(%s) succeeded", invalid)
		}
	}
}
