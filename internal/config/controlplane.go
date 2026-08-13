package config

import (
	"errors"
	"fmt"
	"strings"
)

func LoadControlPlane() (ControlPlane, error) {
	bootstrapConfig, err := LoadBootstrap()
	if err != nil {
		return ControlPlane{}, err
	}
	publicURL, err := normalizeOrigin("PUBLIC_URL", env("PUBLIC_URL", DefaultPublicURL))
	if err != nil {
		return ControlPlane{}, err
	}
	apiOriginRaw := envText("API_ORIGIN")
	if apiOriginRaw == "" {
		apiOriginRaw = publicURL
	}
	apiOrigin, err := normalizeOrigin("API_ORIGIN", apiOriginRaw)
	if err != nil {
		return ControlPlane{}, err
	}
	magicLinkDebugURLs, err := envBool("MAGIC_LINK_DEBUG_URLS", false)
	if err != nil {
		return ControlPlane{}, err
	}
	cfg := ControlPlane{
		Addr:                            env("CONTROL_PLANE_ADDR", ":8080"),
		DeploymentMode:                  env("DEPLOYMENT_MODE", DeploymentModeSelfHosted),
		DatabaseURL:                     envText("DATABASE_URL"),
		RedisURL:                        env("REDIS_URL", "redis://127.0.0.1:6379/0"),
		ClickHouseURL:                   envText("CLICKHOUSE_URL"),
		ClickHouseUser:                  envText("CLICKHOUSE_USER"),
		ClickHousePassword:              envSecret("CLICKHOUSE_PASSWORD"),
		CASURI:                          envText("CAS_URI"),
		DeploymentRuntimeDescriptorPath: envText("DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH"),
		PlatformStoreURI:                envText("PLATFORM_STORE_URI"),
		Bootstrap:                       bootstrapConfig,
		CapacityToken:                   envSecret("CAPACITY_TOKEN"),
		SetupToken:                      envSecret("SETUP_TOKEN"),
		PublicURL:                       publicURL,
		APIOrigin:                       apiOrigin,
		MagicLinkDebugURLs:              magicLinkDebugURLs,
		AdminEmails:                     splitNormalizedList(envText("ADMIN_EMAILS")),
		EmailProvider:                   envLower("EMAIL_PROVIDER"),
		ResendAPIKey:                    envSecret("RESEND_API_KEY"),
		SMTPAddr:                        envText("SMTP_ADDR"),
		SMTPUsername:                    envText("SMTP_USERNAME"),
		SMTPPassword:                    envSecret("SMTP_PASSWORD"),
		EmailFrom:                       envText("EMAIL_FROM"),
		GitHubOAuthClientID:             envText("GITHUB_OAUTH_CLIENT_ID"),
		GitHubOAuthClientSecret:         envSecret("GITHUB_OAUTH_CLIENT_SECRET"),
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
		{"TOKEN_CREDENTIAL_KEY", &cfg.TokenCredentialKey},
		{"WORKSPACE_FENCING_KEY", &cfg.WorkspaceFencingKey},
		{"ENCRYPTION_KEY", &cfg.EncryptionKey},
		{"WORKER_TOKEN_SIGNING_KEY", &cfg.WorkerTokenSigningKey},
	} {
		*root.target, err = rootKey(root.name)
		if err != nil {
			return cfg, err
		}
	}
	if err := validateControlPlaneEmailConfig(&cfg); err != nil {
		return cfg, err
	}
	if cfg.GitHubOAuthClientID == "" {
		return cfg, errors.New("GITHUB_OAUTH_CLIENT_ID is required")
	}
	if cfg.GitHubOAuthClientSecret == "" {
		return cfg, errors.New("GITHUB_OAUTH_CLIENT_SECRET is required")
	}
	for _, token := range []struct {
		name  string
		value string
	}{
		{"CAPACITY_TOKEN", cfg.CapacityToken},
		{"SETUP_TOKEN", cfg.SetupToken},
	} {
		if strings.TrimSpace(token.value) != token.value {
			return cfg, fmt.Errorf("%s must not have surrounding whitespace", token.name)
		}
	}
	if cfg.DeploymentMode == DeploymentModeSelfHosted && cfg.SetupToken == "" {
		return cfg, errors.New("SETUP_TOKEN is required when DEPLOYMENT_MODE is self-hosted")
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

func splitNormalizedList(value string) []string {
	if value == "" {
		return nil
	}
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for part := range strings.SplitSeq(value, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		result = append(result, part)
	}
	return result
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
			return errors.New("EMAIL_PROVIDER is required when EMAIL_FROM is set")
		}
		if cfg.ResendAPIKey != "" {
			return errors.New("EMAIL_PROVIDER=resend is required when RESEND_API_KEY is set")
		}
		if cfg.SMTPAddr != "" || cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
			return errors.New("EMAIL_PROVIDER=smtp is required when SMTP config is set")
		}
	case EmailProviderLog:
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
