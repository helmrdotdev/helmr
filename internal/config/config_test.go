package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadControlReadsRequiredConfig(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_DEPLOYMENT_MODE", "managed-cloud")
	t.Setenv("HELMR_REDIS_URL", "redis://redis.example.test:6379/0")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_WORKER_BOOTSTRAP_TOKEN", " worker-bootstrap-token ")
	t.Setenv("HELMR_SETUP_TOKEN", " setup-token ")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_PUBLIC_URL", "https://helmr.example.test")
	t.Setenv("HELMR_MAGIC_LINK_DEBUG_URLS", "true")
	t.Setenv("HELMR_SMTP_ADDR", "smtp.example.test:587")
	t.Setenv("HELMR_SMTP_USERNAME", "smtp-user")
	t.Setenv("HELMR_SMTP_PASSWORD", "smtp-password")
	t.Setenv("HELMR_EMAIL_FROM", "Helmr <noreply@example.test>")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL == "" || cfg.DeploymentMode != "managed-cloud" || cfg.RedisURL != "redis://redis.example.test:6379/0" || cfg.WorkerTokenSigningKey != "01234567890123456789012345678901" || cfg.WorkerBootstrapToken != "worker-bootstrap-token" || cfg.SetupToken != "setup-token" || cfg.AuthSecret == "" || cfg.SecretEncryptionKey == "" || cfg.PublicURL != "https://helmr.example.test" || !cfg.MagicLinkDebugURLs || cfg.EmailProvider != EmailProviderSMTP || cfg.SMTPAddr != "smtp.example.test:587" || cfg.SMTPUsername != "smtp-user" || cfg.SMTPPassword != "smtp-password" || cfg.EmailFrom != "Helmr <noreply@example.test>" || cfg.GitHubAppID != "123" || cfg.GitHubAppSlug != "helmr-test" || cfg.GitHubAppPrivateKeyPath == "" || cfg.GitHubWebhookSecret != "webhook-secret" || cfg.GitHubAppClientID != "client-id" || cfg.GitHubAppClientSecret != "client-secret" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadControlDefaultsToSelfHostedDeploymentMode(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeploymentMode != DeploymentModeSelfHosted {
		t.Fatalf("deployment mode = %q", cfg.DeploymentMode)
	}
}

func TestLoadControlRequiresSetupTokenForSelfHosted(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected missing setup token error")
	}
	if got, want := err.Error(), "HELMR_SETUP_TOKEN is required"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlRejectsInvalidDeploymentMode(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_DEPLOYMENT_MODE", "unknown")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected invalid deployment mode error")
	}
	if got, want := err.Error(), "HELMR_DEPLOYMENT_MODE"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlAcceptsGitHubPrivateKeyValue(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY", "private-key")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubAppPrivateKeyEnv != "HELMR_GITHUB_APP_PRIVATE_KEY" || cfg.GitHubAppPrivateKeyPath != "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadControlRejectsWeakWorkerSigningKey(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "short")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected weak worker signing key error")
	}
	if got, want := err.Error(), "HELMR_WORKER_TOKEN_SIGNING_KEY:"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlRejectsWeakAuthSecret(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_AUTH_SECRET", "short")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

	_, err := LoadControl()
	if err == nil {
		t.Fatal("expected weak auth secret error")
	}
	if got, want := err.Error(), "HELMR_AUTH_SECRET:"; !strings.HasPrefix(got, want) {
		t.Fatalf("error = %q", got)
	}
}

func TestLoadControlAllowsHTTPOnlyForLoopbackPublicURL(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_PUBLIC_URL", "http://127.0.0.1:8080")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

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
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_SETUP_TOKEN", "setup-token")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

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
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

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
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_DEPLOYMENT_MODE", "managed-cloud")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_TOKEN_SIGNING_KEY", "01234567890123456789012345678901")
	t.Setenv("HELMR_AUTH_SECRET", "abcdefghijabcdefghijabcdefghij12")
	t.Setenv("HELMR_SECRET_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_EMAIL_PROVIDER", "resend")
	t.Setenv("HELMR_RESEND_API_KEY", "re_test")
	t.Setenv("HELMR_EMAIL_FROM", "Helmr <noreply@example.test>")
	t.Setenv("HELMR_GITHUB_APP_ID", "123")
	t.Setenv("HELMR_GITHUB_APP_SLUG", "helmr-test")
	t.Setenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH", "/run/secrets/github-app.pem")
	t.Setenv("HELMR_GITHUB_APP_WEBHOOK_SECRET", "webhook-secret")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_ID", "client-id")
	t.Setenv("HELMR_GITHUB_APP_CLIENT_SECRET", "client-secret")

	cfg, err := LoadControl()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmailProvider != EmailProviderResend || cfg.ResendAPIKey != "re_test" || cfg.EmailFrom != "Helmr <noreply@example.test>" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadDatabaseOnlyRequiresDatabaseURL(t *testing.T) {
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
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
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_WORKER_BOOTSTRAP_TOKEN", "bootstrap-token")
	t.Setenv("HELMR_WORKER_BOOTSTRAP_TOKEN_PATH", "/run/helmr/bootstrap-token")
	t.Setenv("HELMR_WORKER_RESOURCE_ID", "worker-instance-1")
	t.Setenv("HELMR_WORKER_REGION", "us-east-1")
	t.Setenv("HELMR_WORKER_LABELS", "pool=standard,dedicated_key=tenant-a")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_WORK_DIR", "/var/lib/helmr")
	t.Setenv("HELMR_WORKER_IMAGES_DIR", "/var/lib/helmr/images")
	t.Setenv("HELMR_GIT_PATH", "/usr/bin/git")
	t.Setenv("HELMR_WORKER_BUILDKIT_ADDR", "unix:///run/helmr/buildkit/buildkitd.sock")
	t.Setenv("HELMR_WORKER_BUILDKIT_CACHE_NAMESPACE", "helmr-ci")
	t.Setenv("HELMR_WORKER_FIRECRACKER_PATH", "/usr/bin/firecracker")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_PATH", "/usr/bin/jailer")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_WORKER_FIRECRACKER_NUMA_NODE", "1")
	t.Setenv("HELMR_WORKER_FIRECRACKER_CHROOT_DIR", "/var/lib/helmr/jailer")
	t.Setenv("HELMR_WORKER_FIRECRACKER_CGROUP_VERSION", "2")
	t.Setenv("HELMR_WORKER_CNI_NETWORK", "helmr-ci")
	t.Setenv("HELMR_WORKER_CNI_PROFILE", "helmr-ci/v2")
	t.Setenv("HELMR_WORKER_CNI_CONF_DIR", "/etc/helmr/cni")
	t.Setenv("HELMR_WORKER_CNI_BIN_DIR", "/opt/helmr/cni/bin")
	t.Setenv("HELMR_WORKER_CNI_CACHE_DIR", "/var/lib/helmr/cni")
	t.Setenv("HELMR_WORKER_IP_PATH", "/usr/sbin/ip")
	t.Setenv("HELMR_WORKER_NFT_PATH", "/usr/sbin/nft")
	t.Setenv("HELMR_WORKER_NETWORK_BLOCKED_IPV4_CIDRS", "10.0.0.0/8,169.254.0.0/16")
	t.Setenv("HELMR_WORKER_NETWORK_BLOCKED_IPV6_CIDRS", "fc00::/7 fe80::/10")
	t.Setenv("HELMR_VM_VCPUS", "4")
	t.Setenv("HELMR_VM_MEMORY_MIB", "4096")
	t.Setenv("HELMR_VM_SCRATCH_DISK_MIB", "12288")
	t.Setenv("HELMR_VM_HEALTH_TIMEOUT", "90s")

	cfg, err := LoadWorker()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CASURI != "s3://helmr-cas" || cfg.WorkDir != "/var/lib/helmr" || cfg.ImagesDir != "/var/lib/helmr/images" || cfg.GitPath != "/usr/bin/git" || cfg.BuildKitAddr != "unix:///run/helmr/buildkit/buildkitd.sock" || cfg.BuildKitCacheNS != "helmr-ci" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.FirecrackerPath != "/usr/bin/firecracker" || cfg.CNINetworkName != "helmr-ci" || cfg.CNIConfDir != "/etc/helmr/cni" || cfg.CNIBinDir != "/opt/helmr/cni/bin" || cfg.CNICacheDir != "/var/lib/helmr/cni" || cfg.VMVCPUCount != 4 || cfg.VMMemoryMiB != 4096 || cfg.VMScratchDiskMiB != 12288 || cfg.VMHealthTimeout != 90*time.Second {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.JailerPath != "/usr/bin/jailer" || cfg.JailerUID != 1001 || cfg.JailerGID != 1002 || cfg.JailerNumaNode != 1 || cfg.JailerChrootDir != "/var/lib/helmr/jailer" || cfg.CgroupVersion != "2" || cfg.CNIProfile != "helmr-ci/v2" || cfg.IPPath != "/usr/sbin/ip" || cfg.NFTPath != "/usr/sbin/nft" {
		t.Fatalf("config = %+v", cfg)
	}
	if !stringSlicesEqual(cfg.NetworkBlockedIPv4CIDRs, []string{"10.0.0.0/8", "169.254.0.0/16"}) || !stringSlicesEqual(cfg.NetworkBlockedIPv6CIDRs, []string{"fc00::/7", "fe80::/10"}) {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.WorkerResourceID != "worker-instance-1" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.WorkerBootstrapToken != "bootstrap-token" || cfg.WorkerBootstrapTokenPath != "/run/helmr/bootstrap-token" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.WorkerRegion != "us-east-1" || cfg.WorkerLabels["pool"] != "standard" || cfg.WorkerLabels["dedicated_key"] != "tenant-a" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.CheckpointKey == "" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadWorkerAllowsEmptyNetworkBlockedCIDRs(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "checkpoint-key")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
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
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
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
	t.Setenv("HELMR_CHECKPOINT_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_UID", "1001")
	t.Setenv("HELMR_WORKER_FIRECRACKER_JAILER_GID", "1002")
	t.Setenv("HELMR_VM_MEMORY_MIB", "big")

	_, err := LoadWorker()
	if err == nil {
		t.Fatal("expected invalid memory error")
	}
}

func TestLoadWorkerRejectsInvalidLabels(t *testing.T) {
	t.Setenv("HELMR_CONTROL_URL", "https://api.example.test")
	t.Setenv("HELMR_CAS_URI", "s3://helmr-cas")
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
