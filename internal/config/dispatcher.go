package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func LoadDispatcher() (Dispatcher, error) {
	publicURL := env("HELMR_PUBLIC_URL", DefaultPublicURL)
	cfg := Dispatcher{
		DatabaseURL:   os.Getenv("HELMR_DATABASE_URL"),
		RedisURL:      env("HELMR_REDIS_URL", "redis://127.0.0.1:6379/0"),
		AsyncQueueURI: strings.TrimSpace(os.Getenv("HELMR_ASYNC_QUEUE_URI")),
		AuthSecret:    os.Getenv("HELMR_AUTH_SECRET"),
		PublicURL:     publicURL,
		EmailProvider: strings.ToLower(strings.TrimSpace(os.Getenv("HELMR_EMAIL_PROVIDER"))),
		ResendAPIKey:  os.Getenv("HELMR_RESEND_API_KEY"),
		SMTPAddr:      strings.TrimSpace(os.Getenv("HELMR_SMTP_ADDR")),
		SMTPUsername:  os.Getenv("HELMR_SMTP_USERNAME"),
		SMTPPassword:  os.Getenv("HELMR_SMTP_PASSWORD"),
		EmailFrom:     strings.TrimSpace(os.Getenv("HELMR_EMAIL_FROM")),
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
