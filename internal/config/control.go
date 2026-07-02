package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func LoadControl() (Control, error) {
	publicURL := env("HELMR_PUBLIC_URL", DefaultPublicURL)
	magicLinkDebugURLs, err := envBool("HELMR_MAGIC_LINK_DEBUG_URLS", false)
	if err != nil {
		return Control{}, err
	}
	cfg := Control{
		Addr:                    env("HELMR_CONTROL_ADDR", ":8080"),
		DeploymentMode:          env("HELMR_DEPLOYMENT_MODE", DeploymentModeSelfHosted),
		CellID:                  env("HELMR_CELL_ID", DefaultCellID),
		DatabaseURL:             envString("HELMR_DATABASE_URL"),
		RedisURL:                env("HELMR_REDIS_URL", "redis://127.0.0.1:6379/0"),
		ClickHouseURL:           envString("HELMR_CLICKHOUSE_URL"),
		ClickHouseUser:          envString("HELMR_CLICKHOUSE_USER"),
		ClickHousePassword:      envString("HELMR_CLICKHOUSE_PASSWORD"),
		CASURI:                  envString("HELMR_CAS_URI"),
		WorkerTokenSigningKey:   envString("HELMR_WORKER_TOKEN_SIGNING_KEY"),
		WorkerBootstrapToken:    envString("HELMR_WORKER_BOOTSTRAP_TOKEN"),
		SetupToken:              envString("HELMR_SETUP_TOKEN"),
		AuthSecret:              envString("HELMR_AUTH_SECRET"),
		SecretEncryptionKey:     envString("HELMR_SECRET_ENCRYPTION_KEY"),
		SecretEncryptionKeyOld:  envString("HELMR_SECRET_ENCRYPTION_KEY_OLD"),
		PublicURL:               publicURL,
		MagicLinkDebugURLs:      magicLinkDebugURLs,
		EmailProvider:           envLower("HELMR_EMAIL_PROVIDER"),
		ResendAPIKey:            envString("HELMR_RESEND_API_KEY"),
		SMTPAddr:                envString("HELMR_SMTP_ADDR"),
		SMTPUsername:            envString("HELMR_SMTP_USERNAME"),
		SMTPPassword:            envString("HELMR_SMTP_PASSWORD"),
		EmailFrom:               envString("HELMR_EMAIL_FROM"),
		GitHubOAuthClientID:     envString("HELMR_GITHUB_OAUTH_CLIENT_ID"),
		GitHubOAuthClientSecret: envString("HELMR_GITHUB_OAUTH_CLIENT_SECRET"),
		ScheduleJitter:          30 * time.Second,
		RuntimePrepareTarget:    0,
		RuntimePrepareLimit:     20,
	}
	if cfg.ScheduleJitter, err = envDuration("HELMR_SCHEDULE_JITTER", cfg.ScheduleJitter); err != nil {
		return cfg, err
	}
	if cfg.RuntimePrepareTarget, err = envInt("HELMR_PREPARED_RUNTIME_WARM_TARGET", cfg.RuntimePrepareTarget); err != nil {
		return cfg, err
	}
	if cfg.RuntimePrepareTarget < 0 {
		return cfg, errors.New("HELMR_PREPARED_RUNTIME_WARM_TARGET must be non-negative")
	}
	if cfg.RuntimePrepareLimit, err = envInt("HELMR_PREPARED_RUNTIME_WARM_LIMIT", cfg.RuntimePrepareLimit); err != nil {
		return cfg, err
	}
	if cfg.RuntimePrepareLimit <= 0 {
		return cfg, errors.New("HELMR_PREPARED_RUNTIME_WARM_LIMIT must be positive")
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("HELMR_DATABASE_URL is required")
	}
	if cfg.DeploymentMode != DeploymentModeSelfHosted && cfg.DeploymentMode != DeploymentModeManagedCloud {
		return cfg, errors.New("HELMR_DEPLOYMENT_MODE must be self-hosted or managed-cloud")
	}
	if cfg.CellID == "" {
		return cfg, errors.New("HELMR_CELL_ID is required")
	}
	if cfg.CASURI == "" {
		return cfg, errors.New("HELMR_CAS_URI is required")
	}
	if cfg.WorkerTokenSigningKey == "" {
		return cfg, errors.New("HELMR_WORKER_TOKEN_SIGNING_KEY is required")
	}
	if err := auth.ValidateWorkerTokenSecret([]byte(cfg.WorkerTokenSigningKey)); err != nil {
		return cfg, fmt.Errorf("HELMR_WORKER_TOKEN_SIGNING_KEY: %w", err)
	}
	if cfg.AuthSecret == "" {
		return cfg, errors.New("HELMR_AUTH_SECRET is required")
	}
	if err := auth.ValidateTokenSecret([]byte(cfg.AuthSecret)); err != nil {
		return cfg, fmt.Errorf("HELMR_AUTH_SECRET: %w", err)
	}
	if cfg.SecretEncryptionKey == "" {
		return cfg, errors.New("HELMR_SECRET_ENCRYPTION_KEY is required")
	}
	if err := validatePublicURL(cfg.PublicURL); err != nil {
		return cfg, err
	}
	if err := validateControlEmailConfig(&cfg); err != nil {
		return cfg, err
	}
	if cfg.GitHubOAuthClientID == "" {
		return cfg, errors.New("HELMR_GITHUB_OAUTH_CLIENT_ID is required")
	}
	if cfg.GitHubOAuthClientSecret == "" {
		return cfg, errors.New("HELMR_GITHUB_OAUTH_CLIENT_SECRET is required")
	}
	if cfg.DeploymentMode == DeploymentModeSelfHosted && cfg.SetupToken == "" {
		return cfg, errors.New("HELMR_SETUP_TOKEN is required when HELMR_DEPLOYMENT_MODE is self-hosted")
	}
	return cfg, nil
}

func validateControlEmailConfig(cfg *Control) error {
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
			return errors.New("HELMR_EMAIL_PROVIDER is required when HELMR_EMAIL_FROM is set")
		}
		if cfg.ResendAPIKey != "" {
			return errors.New("HELMR_EMAIL_PROVIDER=resend is required when HELMR_RESEND_API_KEY is set")
		}
		if cfg.SMTPAddr != "" || cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
			return errors.New("HELMR_EMAIL_PROVIDER=smtp is required when SMTP config is set")
		}
	case EmailProviderLog:
		if cfg.ResendAPIKey != "" || cfg.SMTPAddr != "" || cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
			return errors.New("HELMR_EMAIL_PROVIDER=log cannot be combined with SMTP or Resend config")
		}
	case EmailProviderSMTP:
		if cfg.SMTPAddr == "" {
			return errors.New("HELMR_SMTP_ADDR is required when HELMR_EMAIL_PROVIDER=smtp")
		}
		if cfg.EmailFrom == "" {
			return errors.New("HELMR_EMAIL_FROM is required when HELMR_EMAIL_PROVIDER=smtp")
		}
		if cfg.ResendAPIKey != "" {
			return errors.New("HELMR_RESEND_API_KEY cannot be combined with HELMR_EMAIL_PROVIDER=smtp")
		}
	case EmailProviderResend:
		if cfg.ResendAPIKey == "" {
			return errors.New("HELMR_RESEND_API_KEY is required when HELMR_EMAIL_PROVIDER=resend")
		}
		if cfg.EmailFrom == "" {
			return errors.New("HELMR_EMAIL_FROM is required when HELMR_EMAIL_PROVIDER=resend")
		}
		if cfg.SMTPAddr != "" || cfg.SMTPUsername != "" || cfg.SMTPPassword != "" {
			return errors.New("SMTP config cannot be combined with HELMR_EMAIL_PROVIDER=resend")
		}
	default:
		return errors.New("HELMR_EMAIL_PROVIDER must be none, log, smtp, or resend")
	}
	return nil
}
