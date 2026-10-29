package config

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"strings"
	"time"
)

func LoadControlPlane() (ControlPlane, error) {
	policy, err := auditpolicy.Parse(env("AUDIT_RETENTION_SECONDS", ""))
	if err != nil {
		return ControlPlane{}, err
	}
	bootstrapConfig, err := LoadBootstrap()
	if err != nil {
		return ControlPlane{}, err
	}
	appOrigin, err := normalizeOrigin("APP_ORIGIN", env("APP_ORIGIN", DefaultAppOrigin))
	if err != nil {
		return ControlPlane{}, err
	}
	httpIdleTimeout, err := LoadControlPlaneHTTPIdleTimeout()
	if err != nil {
		return ControlPlane{}, err
	}
	cfg := ControlPlane{
		AuditPolicy:                     policy,
		SlackTransport:                  env("SLACK_TRANSPORT", "http"),
		OutboundProxyURL:                envText("OUTBOUND_PROXY_URL"),
		Addr:                            env("CONTROL_PLANE_ADDR", ":8080"),
		HTTPIdleTimeout:                 httpIdleTimeout,
		DeploymentMode:                  env("DEPLOYMENT_MODE", DeploymentModeSelfHosted),
		DatabaseURL:                     envText("DATABASE_URL"),
		ClickHouseURL:                   envText("CLICKHOUSE_URL"),
		ClickHouseUser:                  envText("CLICKHOUSE_USER"),
		ClickHousePassword:              envSecret("CLICKHOUSE_PASSWORD"),
		CASURI:                          envText("CAS_URI"),
		DeploymentRuntimeDescriptorPath: envText("DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH"),
		PlatformStoreURI:                envText("PLATFORM_STORE_URI"),
		Bootstrap:                       bootstrapConfig,
		CapacityToken:                   envSecret("CAPACITY_TOKEN"),
		AppOrigin:                       appOrigin,
		EmailProvider:                   envLower("EMAIL_PROVIDER"),
		ResendAPIKey:                    envSecret("RESEND_API_KEY"),
		SMTPAddr:                        envText("SMTP_ADDR"),
		SMTPUsername:                    envText("SMTP_USERNAME"),
		SMTPPassword:                    envSecret("SMTP_PASSWORD"),
		EmailFrom:                       envText("EMAIL_FROM"),
		GitHubOAuthClientID:             envText("GITHUB_OAUTH_CLIENT_ID"),
		GitHubOAuthClientSecret:         envSecret("GITHUB_OAUTH_CLIENT_SECRET"),
	}
	cfg.NetworkPrivateBusinessCIDRs, err = parseCanonicalBlockedIPv4Prefixes(env("CONTROL_PLANE_NETWORK_PRIVATE_BUSINESS_CIDRS", "[]"))
	if err != nil {
		return cfg, fmt.Errorf("CONTROL_PLANE_NETWORK_PRIVATE_BUSINESS_CIDRS: %w", err)
	}
	cfg.NetworkBlockedIPv4CIDRs, err = parseCanonicalBlockedIPv4Prefixes(env("CONTROL_PLANE_NETWORK_BLOCKED_IPV4_CIDRS", "[]"))
	if err != nil {
		return cfg, fmt.Errorf("CONTROL_PLANE_NETWORK_BLOCKED_IPV4_CIDRS: %w", err)
	}
	if err = networkpolicy.ValidateInstallation(networkpolicy.Installation{PrivateBusinessCIDRs: cfg.NetworkPrivateBusinessCIDRs, BlockedIPv4CIDRs: cfg.NetworkBlockedIPv4CIDRs}); err != nil {
		return cfg, err
	}
	if err := validatePlatformConnections(cfg.SlackTransport, cfg.OutboundProxyURL); err != nil {
		return cfg, err
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.DeploymentMode != DeploymentModeSelfHosted && cfg.DeploymentMode != DeploymentModeManagedCloud {
		return cfg, errors.New("DEPLOYMENT_MODE must be self-hosted or managed-cloud")
	}
	if cfg.ClickHouseURL == "" {
		return cfg, errors.New("CLICKHOUSE_URL is required")
	}
	if cfg.CASURI == "" {
		return cfg, errors.New("CAS_URI is required")
	}
	if cfg.DeploymentRuntimeDescriptorPath == "" {
		return cfg, errors.New("DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH is required")
	}
	if cfg.PlatformStoreURI == "" {
		return cfg, errors.New("PLATFORM_STORE_URI is required")
	}
	for _, root := range []struct {
		name   string
		target *[]byte
	}{
		{"AUTH_KEY", &cfg.AuthKey},
		{"COMPUTER_FENCING_KEY", &cfg.ComputerFencingKey},
		{"ENCRYPTION_KEY", &cfg.EncryptionKey},
		{"WORKER_HOST_CREDENTIAL_SIGNING_KEY", &cfg.WorkerHostCredentialSigningKey},
	} {
		*root.target, err = rootKey(root.name)
		if err != nil {
			return cfg, err
		}
	}
	if err := validateControlPlaneEmailConfig(&cfg); err != nil {
		return cfg, err
	}
	if (cfg.GitHubOAuthClientID == "") != (cfg.GitHubOAuthClientSecret == "") {
		return cfg, errors.New("GITHUB_OAUTH_CLIENT_ID and GITHUB_OAUTH_CLIENT_SECRET must be configured together")
	}
	for _, token := range []struct {
		name  string
		value string
	}{
		{"CAPACITY_TOKEN", cfg.CapacityToken},
	} {
		if strings.TrimSpace(token.value) != token.value {
			return cfg, fmt.Errorf("%s must not have surrounding whitespace", token.name)
		}
	}
	if cfg.DeploymentMode == DeploymentModeManagedCloud {
		cfg.OrganizationCreationAdminOnly, err = envBool("ORGANIZATION_CREATION_ADMIN_ONLY", false)
		if err != nil {
			return cfg, err
		}
		cfg.ComputerKMSKeyARN = envText("COMPUTER_KMS_KEY_ARN")
		if cfg.ComputerKMSKeyARN == "" {
			return cfg, errors.New("COMPUTER_KMS_KEY_ARN is required when DEPLOYMENT_MODE is managed-cloud")
		}
	} else {
		cfg.ComputerWrappingKeyID = envText("COMPUTER_WRAPPING_KEY_ID")
		if cfg.ComputerWrappingKeyID == "" {
			return cfg, errors.New("COMPUTER_WRAPPING_KEY_ID is required when DEPLOYMENT_MODE is self-hosted")
		}
		cfg.ComputerWrappingKey, err = rootKey("COMPUTER_WRAPPING_KEY")
		if err != nil {
			return cfg, err
		}
	}
	cfg.EnvironmentExecutionLimits, err = LoadEnvironmentExecutionLimits()
	if err != nil {
		return cfg, err
	}

	cfg.DeploymentUploadLimits, err = LoadDeploymentUploadLimits()
	if err != nil {
		return cfg, err
	}
	cfg.DiagnosticAdmission, err = LoadDiagnosticAdmission()
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}

func LoadBootstrap() (Bootstrap, error) {
	enabled, err := envBool("BOOTSTRAP_ENABLED", false)
	if err != nil {
		return Bootstrap{}, err
	}
	if !enabled {
		return Bootstrap{}, nil
	}
	cfg := Bootstrap{
		Enabled:           true,
		RegionID:          env("BOOTSTRAP_REGION_ID", "default"),
		RegionDisplayName: envText("BOOTSTRAP_REGION_DISPLAY_NAME"),
		RegionLocation:    envText("BOOTSTRAP_REGION_LOCATION"),
		WorkerGroupName:   env("BOOTSTRAP_WORKER_GROUP_NAME", "default"),
		WorkerToken:       envSecret("BOOTSTRAP_WORKER_TOKEN"),
	}
	return cfg, nil
}

func validateControlPlaneEmailConfig(cfg *ControlPlane) error {
	if cfg.EmailProvider == "" {
		switch {
		case cfg.ResendAPIKey != "":
			cfg.EmailProvider = EmailProviderResend
		case cfg.SMTPAddr != "":
			cfg.EmailProvider = EmailProviderSMTP
		default:
			cfg.EmailProvider = EmailProviderNone
		}
	}
	switch cfg.EmailProvider {
	case EmailProviderNone:
		if cfg.EmailFrom != "" {
			return errors.New("EMAIL_FROM requires EMAIL_PROVIDER=log, smtp, or resend")
		}
		if cfg.ResendAPIKey != "" {
			return errors.New("EMAIL_PROVIDER=resend is required when RESEND_API_KEY is set")
		}
		if cfg.SMTPAddr != "" || cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
			return errors.New("EMAIL_PROVIDER=smtp is required when SMTP config is set")
		}
	case EmailProviderLog:
		if cfg.DeploymentMode == DeploymentModeManagedCloud {
			return errors.New("EMAIL_PROVIDER=log is for local development and is not allowed when DEPLOYMENT_MODE is managed-cloud")
		}
		if cfg.ResendAPIKey != "" || cfg.SMTPAddr != "" || cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
			return errors.New("EMAIL_PROVIDER=log cannot be combined with SMTP or Resend config")
		}
	case EmailProviderSMTP:
		if cfg.SMTPAddr == "" {
			return errors.New("SMTP_ADDR is required when EMAIL_PROVIDER=smtp")
		}
		if cfg.EmailFrom == "" {
			return errors.New("EMAIL_FROM is required when EMAIL_PROVIDER=smtp")
		}
		if cfg.ResendAPIKey != "" {
			return errors.New("RESEND_API_KEY cannot be combined with EMAIL_PROVIDER=smtp")
		}
	case EmailProviderResend:
		if cfg.ResendAPIKey == "" {
			return errors.New("RESEND_API_KEY is required when EMAIL_PROVIDER=resend")
		}
		if cfg.EmailFrom == "" {
			return errors.New("EMAIL_FROM is required when EMAIL_PROVIDER=resend")
		}
		if cfg.SMTPAddr != "" || cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
			return errors.New("SMTP config cannot be combined with EMAIL_PROVIDER=resend")
		}
	default:
		return errors.New("EMAIL_PROVIDER must be none, log, smtp, or resend")
	}
	return nil
}

// HTTP keepalive is distinct from body progress and admitted-upload lifetime.
// Reverse proxies should close idle pooled connections before their backend.
func LoadControlPlaneHTTPIdleTimeout() (time.Duration, error) {
	duration, err := envDuration("CONTROL_PLANE_HTTP_IDLE_TIMEOUT", 2*time.Minute)
	if err != nil {
		return 0, err
	}
	if duration < time.Millisecond {
		return 0, errors.New("CONTROL_PLANE_HTTP_IDLE_TIMEOUT must be at least 1ms")
	}
	return duration, nil
}
