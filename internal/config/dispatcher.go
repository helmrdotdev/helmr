package config

import (
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func LoadDispatcher() (Dispatcher, error) {
	const maxInt32 = int(1<<31 - 1)
	var err error
	cfg := Dispatcher{
		DatabaseURL:           envText("DATABASE_URL"),
		RedisURL:              env("REDIS_URL", "redis://127.0.0.1:6379/0"),
		ClickHouseURL:         envText("CLICKHOUSE_URL"),
		ClickHouseUser:        envText("CLICKHOUSE_USER"),
		ClickHousePassword:    envSecret("CLICKHOUSE_PASSWORD"),
		RunReservationTTL:     5 * time.Minute,
		RunLeaseStartDeadline: time.Minute,
		RunLeaseTTL:           5 * time.Minute,
		SchedulePollInterval:  time.Second,
		ScheduleClaimLimit:    100,
		ScheduleConcurrency:   10,
		ScheduleClaimLease:    5 * time.Minute,
	}
	if cfg.SchedulePollInterval, err = envDuration("SCHEDULE_POLL_INTERVAL", cfg.SchedulePollInterval); err != nil {
		return cfg, err
	}
	if cfg.ScheduleClaimLimit, err = envInt("SCHEDULE_CLAIM_LIMIT", cfg.ScheduleClaimLimit); err != nil {
		return cfg, err
	}
	if cfg.ScheduleConcurrency, err = envInt("SCHEDULE_CONCURRENCY", cfg.ScheduleConcurrency); err != nil {
		return cfg, err
	}
	if cfg.ScheduleClaimLease, err = envDuration("SCHEDULE_CLAIM_LEASE", cfg.ScheduleClaimLease); err != nil {
		return cfg, err
	}
	if cfg.RunReservationTTL, err = envDuration("RUN_RESERVATION_TTL", cfg.RunReservationTTL); err != nil {
		return cfg, err
	}
	if cfg.RunLeaseStartDeadline, err = envDuration("RUN_LEASE_START_DEADLINE", cfg.RunLeaseStartDeadline); err != nil {
		return cfg, err
	}
	if cfg.RunLeaseTTL, err = envDuration("RUN_LEASE_TTL", cfg.RunLeaseTTL); err != nil {
		return cfg, err
	}
	if cfg.SchedulePollInterval <= 0 || cfg.ScheduleClaimLimit <= 0 || cfg.ScheduleConcurrency <= 0 || cfg.ScheduleClaimLease <= 0 {
		return cfg, errors.New("schedule polling, claim, concurrency, and lease settings must be positive")
	}
	if cfg.ScheduleClaimLimit > maxInt32 || cfg.ScheduleConcurrency > maxInt32 {
		return cfg, errors.New("schedule claim and concurrency settings must not exceed 2147483647")
	}
	if cfg.RunReservationTTL <= 0 || cfg.RunLeaseStartDeadline <= 0 ||
		cfg.RunLeaseTTL < workerapi.RunLeaseMinTTL || cfg.RunLeaseStartDeadline > cfg.RunLeaseTTL {
		return cfg, errors.New("run preparation and lease settings are invalid")
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
