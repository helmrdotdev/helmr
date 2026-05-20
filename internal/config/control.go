package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

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
		DatabaseURL:             os.Getenv("HELMR_DATABASE_URL"),
		RedisURL:                env("HELMR_REDIS_URL", "redis://127.0.0.1:6379/0"),
		CASURI:                  os.Getenv("HELMR_CAS_URI"),
		WorkerTokenSigningKey:   os.Getenv("HELMR_WORKER_TOKEN_SIGNING_KEY"),
		WorkerRegistrationToken: strings.TrimSpace(os.Getenv("HELMR_WORKER_REGISTRATION_TOKEN")),
		AuthSecret:              os.Getenv("HELMR_AUTH_SECRET"),
		SecretEncryptionKey:     os.Getenv("HELMR_SECRET_ENCRYPTION_KEY"),
		PublicURL:               publicURL,
		MagicLinkDebugURLs:      magicLinkDebugURLs,
		SMTPAddr:                strings.TrimSpace(os.Getenv("HELMR_SMTP_ADDR")),
		SMTPUsername:            os.Getenv("HELMR_SMTP_USERNAME"),
		SMTPPassword:            os.Getenv("HELMR_SMTP_PASSWORD"),
		EmailFrom:               strings.TrimSpace(os.Getenv("HELMR_EMAIL_FROM")),
		GitHubAppID:             os.Getenv("HELMR_GITHUB_APP_ID"),
		GitHubAppSlug:           os.Getenv("HELMR_GITHUB_APP_SLUG"),
		GitHubAppPrivateKeyPath: os.Getenv("HELMR_GITHUB_APP_PRIVATE_KEY_PATH"),
		GitHubAppPrivateKeyEnv:  "HELMR_GITHUB_APP_PRIVATE_KEY",
		GitHubWebhookSecret:     os.Getenv("HELMR_GITHUB_APP_WEBHOOK_SECRET"),
		GitHubAppClientID:       os.Getenv("HELMR_GITHUB_APP_CLIENT_ID"),
		GitHubAppClientSecret:   os.Getenv("HELMR_GITHUB_APP_CLIENT_SECRET"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("HELMR_DATABASE_URL is required")
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
	if cfg.SMTPAddr != "" && cfg.EmailFrom == "" {
		return cfg, errors.New("HELMR_EMAIL_FROM is required when HELMR_SMTP_ADDR is set")
	}
	if cfg.EmailFrom != "" && cfg.SMTPAddr == "" {
		return cfg, errors.New("HELMR_SMTP_ADDR is required when HELMR_EMAIL_FROM is set")
	}
	if cfg.GitHubAppID == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_ID is required")
	}
	if cfg.GitHubAppSlug == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_SLUG is required")
	}
	if cfg.GitHubAppPrivateKeyPath == "" && os.Getenv(cfg.GitHubAppPrivateKeyEnv) == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_PRIVATE_KEY_PATH or HELMR_GITHUB_APP_PRIVATE_KEY is required")
	}
	if cfg.GitHubWebhookSecret == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_WEBHOOK_SECRET is required")
	}
	if cfg.GitHubAppClientID == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_CLIENT_ID is required")
	}
	if cfg.GitHubAppClientSecret == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_CLIENT_SECRET is required")
	}
	return cfg, nil
}
