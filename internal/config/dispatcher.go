package config

import (
	"errors"
)

func LoadDispatcher() (Dispatcher, error) {
	var err error
	cfg := Dispatcher{
		DatabaseURL:        envText("DATABASE_URL"),
		ClickHouseURL:      envText("CLICKHOUSE_URL"),
		ClickHouseUser:     envText("CLICKHOUSE_USER"),
		ClickHousePassword: envSecret("CLICKHOUSE_PASSWORD"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.ClickHouseURL == "" {
		return cfg, errors.New("CLICKHOUSE_URL is required")
	}
	cfg.WorkspaceFencingKey, err = rootKey("WORKSPACE_FENCING_KEY")
	if err != nil {
		return cfg, err
	}
	return cfg, nil
}
