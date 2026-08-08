package config

import (
	"errors"
)

func LoadDispatcher() (Dispatcher, error) {
	var err error
	cfg := Dispatcher{
		DatabaseURL:        envText("DATABASE_URL"),
		RedisURL:           env("REDIS_URL", "redis://127.0.0.1:6379/0"),
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
