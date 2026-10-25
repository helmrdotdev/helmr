package config

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
)

func LoadDispatcher() (Dispatcher, error) {
	policy, err := auditpolicy.Parse(env("AUDIT_RETENTION_SECONDS", ""))
	if err != nil {
		return Dispatcher{}, err
	}
	appOrigin, err := normalizeOrigin("APP_ORIGIN", env("APP_ORIGIN", DefaultAppOrigin))
	if err != nil {
		return Dispatcher{}, err
	}
	cfg := Dispatcher{
		AuditPolicy:        policy,
		SlackTransport:     env("SLACK_TRANSPORT", "http"),
		OutboundProxyURL:   envText("OUTBOUND_PROXY_URL"),
		AppOrigin:          appOrigin,
		ControlPlaneURL:    envText("CONTROL_PLANE_URL"),
		DatabaseURL:        envText("DATABASE_URL"),
		ClickHouseURL:      envText("CLICKHOUSE_URL"),
		ClickHouseUser:     envText("CLICKHOUSE_USER"),
		ClickHousePassword: envSecret("CLICKHOUSE_PASSWORD"),
	}
	if cfg.ControlPlaneURL == "" {
		return cfg, errors.New("CONTROL_PLANE_URL is required")
	}
	if err := validatePlatformConnections(cfg.SlackTransport, cfg.OutboundProxyURL); err != nil {
		return cfg, err
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.ClickHouseURL == "" {
		return cfg, errors.New("CLICKHOUSE_URL is required")
	}
	cfg.ComputerFencingKey, err = rootKey("COMPUTER_FENCING_KEY")
	if err != nil {
		return cfg, err
	}
	cfg.EncryptionKey, err = rootKey("ENCRYPTION_KEY")
	if err != nil {
		return cfg, err
	}
	cfg.DiagnosticExporter, err = LoadDiagnosticExporter()
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}
