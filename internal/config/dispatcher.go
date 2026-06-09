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
		SecretEncryptionKeyOld:     envString("HELMR_SECRET_ENCRYPTION_KEY_OLD"),
		PublicURL:                  publicURL,
		EmailProvider:              envLower("HELMR_EMAIL_PROVIDER"),
		ResendAPIKey:               envString("HELMR_RESEND_API_KEY"),
		SMTPAddr:                   envString("HELMR_SMTP_ADDR"),
		SMTPUsername:               envString("HELMR_SMTP_USERNAME"),
		SMTPPassword:               envString("HELMR_SMTP_PASSWORD"),
		EmailFrom:                  envString("HELMR_EMAIL_FROM"),
		ScheduleRepairEvery:        5 * time.Second,
		ScheduleRepairLimit:        100,
		ScheduleTriggerConcurrency: 10,
		ScheduleLease:              5 * time.Minute,
		ScheduleMaxAttempts:        10,
		ScheduleJitter:             30 * time.Second,
	}
	var err error
	if cfg.ScheduleRepairEvery, err = envDuration("HELMR_SCHEDULE_REPAIR_EVERY", cfg.ScheduleRepairEvery); err != nil {
		return cfg, err
	}
	if cfg.ScheduleRepairLimit, err = envInt("HELMR_SCHEDULE_REPAIR_LIMIT", cfg.ScheduleRepairLimit); err != nil {
		return cfg, err
	}
	if cfg.ScheduleTriggerConcurrency, err = envInt("HELMR_SCHEDULE_TRIGGER_CONCURRENCY", cfg.ScheduleTriggerConcurrency); err != nil {
		return cfg, err
	}
	if cfg.ScheduleJitter, err = envDuration("HELMR_SCHEDULE_JITTER", cfg.ScheduleJitter); err != nil {
		return cfg, err
	}
	cfg.ScheduleRepairLookahead = 2*cfg.ScheduleRepairEvery + cfg.ScheduleJitter
	if cfg.ScheduleRepairLookahead, err = envDuration("HELMR_SCHEDULE_REPAIR_LOOKAHEAD", cfg.ScheduleRepairLookahead); err != nil {
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
