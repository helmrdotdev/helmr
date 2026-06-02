package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func LoadDispatcher() (Dispatcher, error) {
	publicURL := env("HELMR_PUBLIC_URL", DefaultPublicURL)
	cfg := Dispatcher{
		DatabaseURL:                envString("HELMR_DATABASE_URL"),
		RedisURL:                   env("HELMR_REDIS_URL", "redis://127.0.0.1:6379/0"),
		AsyncBusURI:                envString("HELMR_ASYNC_BUS_URI"),
		AuthSecret:                 envString("HELMR_AUTH_SECRET"),
		SecretEncryptionKey:        envString("HELMR_SECRET_ENCRYPTION_KEY"),
		PublicURL:                  publicURL,
		EmailProvider:              envLower("HELMR_EMAIL_PROVIDER"),
		ResendAPIKey:               envString("HELMR_RESEND_API_KEY"),
		SMTPAddr:                   envString("HELMR_SMTP_ADDR"),
		SMTPUsername:               envString("HELMR_SMTP_USERNAME"),
		SMTPPassword:               envString("HELMR_SMTP_PASSWORD"),
		EmailFrom:                  envString("HELMR_EMAIL_FROM"),
		GitHubAppID:                envString("HELMR_GITHUB_APP_ID"),
		GitHubAppSlug:              envString("HELMR_GITHUB_APP_SLUG"),
		GitHubAppPrivateKeyPath:    envString("HELMR_GITHUB_APP_PRIVATE_KEY_PATH"),
		GitHubAppPrivateKeyEnv:     "HELMR_GITHUB_APP_PRIVATE_KEY",
		ScheduleSweepEvery:         5 * time.Second,
		ScheduleSweepLimit:         100,
		ScheduleTriggerConcurrency: 10,
		ScheduleLease:              5 * time.Minute,
		ScheduleMaxAttempts:        10,
		ScheduleJitter:             30 * time.Second,
	}
	var err error
	if cfg.ScheduleSweepEvery, err = envDuration("HELMR_SCHEDULE_SWEEP_EVERY", cfg.ScheduleSweepEvery); err != nil {
		return cfg, err
	}
	if cfg.ScheduleSweepLimit, err = envInt("HELMR_SCHEDULE_SWEEP_LIMIT", cfg.ScheduleSweepLimit); err != nil {
		return cfg, err
	}
	if cfg.ScheduleTriggerConcurrency, err = envInt("HELMR_SCHEDULE_TRIGGER_CONCURRENCY", cfg.ScheduleTriggerConcurrency); err != nil {
		return cfg, err
	}
	if cfg.ScheduleJitter, err = envDuration("HELMR_SCHEDULE_JITTER", cfg.ScheduleJitter); err != nil {
		return cfg, err
	}
	cfg.ScheduleIndexLookahead = 2*cfg.ScheduleSweepEvery + cfg.ScheduleJitter
	if cfg.ScheduleIndexLookahead, err = envDuration("HELMR_SCHEDULE_INDEX_LOOKAHEAD", cfg.ScheduleIndexLookahead); err != nil {
		return cfg, err
	}
	if cfg.ScheduleLease, err = envDuration("HELMR_SCHEDULE_LEASE", cfg.ScheduleLease); err != nil {
		return cfg, err
	}
	if cfg.ScheduleMaxAttempts, err = envInt("HELMR_SCHEDULE_MAX_ATTEMPTS", cfg.ScheduleMaxAttempts); err != nil {
		return cfg, err
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("HELMR_DATABASE_URL is required")
	}
	if err := validatePublicURL(cfg.PublicURL); err != nil {
		return cfg, err
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
	if cfg.GitHubAppID == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_ID is required")
	}
	if cfg.GitHubAppSlug == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_SLUG is required")
	}
	if cfg.GitHubAppPrivateKeyPath == "" && envString(cfg.GitHubAppPrivateKeyEnv) == "" {
		return cfg, errors.New("HELMR_GITHUB_APP_PRIVATE_KEY_PATH or HELMR_GITHUB_APP_PRIVATE_KEY is required")
	}
	controlEmail := Control{
		EmailProvider: cfg.EmailProvider,
		ResendAPIKey:  cfg.ResendAPIKey,
		SMTPAddr:      cfg.SMTPAddr,
		SMTPUsername:  cfg.SMTPUsername,
		SMTPPassword:  cfg.SMTPPassword,
		EmailFrom:     cfg.EmailFrom,
	}
	if err := validateControlEmailConfig(&controlEmail); err != nil {
		return cfg, err
	}
	cfg.EmailProvider = controlEmail.EmailProvider
	return cfg, nil
}
