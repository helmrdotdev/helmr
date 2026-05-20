package config

import (
	"errors"
	"os"
)

func LoadDispatcher() (Dispatcher, error) {
	cfg := Dispatcher{
		DatabaseURL: os.Getenv("HELMR_DATABASE_URL"),
		RedisURL:    env("HELMR_REDIS_URL", "redis://127.0.0.1:6379/0"),
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("HELMR_DATABASE_URL is required")
	}
	return cfg, nil
}
