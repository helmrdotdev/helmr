package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/auth"
)

func LoadDispatcher() (Dispatcher, error) {
	var err error
	publicURL := env("HELMR_PUBLIC_URL", DefaultPublicURL)
	cfg := Dispatcher{
		FleetMetricsNamespace:      env("HELMR_FLEET_METRICS_NAMESPACE", "Helmr/WorkerFleet"),
		DatabaseURL:                envString("HELMR_DATABASE_URL"),
		RedisURL:                   env("HELMR_REDIS_URL", "redis://127.0.0.1:6379/0"),
		WorkerGroupID:              envString("HELMR_WORKER_GROUP_ID"),
		ClickHouseURL:              envString("HELMR_CLICKHOUSE_URL"),
		ClickHouseUser:             envString("HELMR_CLICKHOUSE_USER"),
		ClickHousePassword:         envString("HELMR_CLICKHOUSE_PASSWORD"),
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
		RuntimePrepareTarget:       0,
		RuntimePrepareLimit:        20,
		RuntimePrepareEvery:        5 * time.Second,
	}
	workerFleetsJSON := envString("HELMR_WORKER_FLEETS")
	if workerFleetsJSON != "" {
		if cfg.WorkerFleets, err = parseWorkerFleets(workerFleetsJSON); err != nil {
			return cfg, fmt.Errorf("HELMR_WORKER_FLEETS: %w", err)
		}
	}
	if len(cfg.WorkerFleets) > 0 && strings.TrimSpace(cfg.FleetMetricsNamespace) == "" {
		return cfg, errors.New("HELMR_FLEET_METRICS_NAMESPACE must not be empty")
	}
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
	if cfg.RuntimePrepareEvery, err = envDuration("HELMR_PREPARED_RUNTIME_WARM_EVERY", cfg.RuntimePrepareEvery); err != nil {
		return cfg, err
	}
	if cfg.RuntimePrepareEvery <= 0 {
		return cfg, errors.New("HELMR_PREPARED_RUNTIME_WARM_EVERY must be positive")
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("HELMR_DATABASE_URL is required")
	}
	if cfg.WorkerGroupID == "" {
		return cfg, errors.New("HELMR_WORKER_GROUP_ID is required")
	}
	if cfg.ClickHouseURL == "" {
		return cfg, errors.New("HELMR_CLICKHOUSE_URL is required")
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

type workerFleetJSON struct {
	GroupID           string   `json:"group_id"`
	Role              string   `json:"role"`
	AutoscalingGroup  string   `json:"autoscaling_group"`
	CompatibilityKeys []string `json:"compatibility_keys"`
	InstanceCapacity  struct {
		MilliCPU           uint64 `json:"milli_cpu"`
		MemoryBytes        uint64 `json:"memory_bytes"`
		WorkloadDiskBytes  uint64 `json:"workload_disk_bytes"`
		ScratchBytes       uint64 `json:"scratch_bytes"`
		BuildCacheBytes    uint64 `json:"build_cache_bytes"`
		ArtifactCacheBytes uint64 `json:"artifact_cache_bytes"`
		VMSlots            uint64 `json:"vm_slots"`
		BuildExecutors     uint64 `json:"build_executors"`
	} `json:"instance_capacity"`
	MinWorkers                int    `json:"min_workers"`
	WarmWorkers               int    `json:"warm_workers"`
	MaxWorkers                int    `json:"max_workers"`
	MaxScaleOutPerCycle       int    `json:"max_scale_out_per_cycle"`
	MaxPendingWorkers         int    `json:"max_pending_workers"`
	MaxPackingItems           int    `json:"max_packing_items"`
	QueuedRunScratchBytes     uint64 `json:"queued_run_scratch_bytes"`
	ControllerIntervalSeconds int    `json:"controller_interval_seconds"`
	ScaleOutCooldownSeconds   int    `json:"scale_out_cooldown_seconds"`
	ScaleInCooldownSeconds    int    `json:"scale_in_cooldown_seconds"`
	ScaleInHysteresisSeconds  int    `json:"scale_in_hysteresis_seconds"`
	StaleWorkerTimeoutSeconds int    `json:"stale_worker_timeout_seconds"`
	ReadinessTimeoutSeconds   int    `json:"readiness_timeout_seconds"`
	DrainTimeoutSeconds       int    `json:"drain_timeout_seconds"`
	EmergencyStop             bool   `json:"emergency_stop"`
	MetricIntervalSeconds     int    `json:"metric_interval_seconds"`
}

func parseWorkerFleets(raw string) ([]WorkerFleet, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var values []workerFleetJSON
	if err := decoder.Decode(&values); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if len(values) == 0 {
		return nil, errors.New("must contain at least one fleet or be unset")
	}
	if len(values) > 32 {
		return nil, errors.New("must contain at most 32 fleets")
	}
	result := make([]WorkerFleet, 0, len(values))
	groups, asgs, roles := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	for index, value := range values {
		fleet, err := validateWorkerFleet(value)
		if err != nil {
			return nil, fmt.Errorf("fleet[%d]: %w", index, err)
		}
		if _, exists := groups[fleet.GroupID]; exists {
			return nil, fmt.Errorf("fleet[%d]: duplicate group_id %q", index, fleet.GroupID)
		}
		if _, exists := asgs[fleet.ASGName]; exists {
			return nil, fmt.Errorf("fleet[%d]: duplicate autoscaling_group %q", index, fleet.ASGName)
		}
		if _, exists := roles[fleet.Role]; exists {
			return nil, fmt.Errorf("fleet[%d]: duplicate role %q", index, fleet.Role)
		}
		groups[fleet.GroupID], asgs[fleet.ASGName] = struct{}{}, struct{}{}
		roles[fleet.Role] = struct{}{}
		result = append(result, fleet)
	}
	return result, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("contains trailing JSON value")
}

func validateWorkerFleet(value workerFleetJSON) (WorkerFleet, error) {
	value.GroupID = strings.TrimSpace(value.GroupID)
	value.Role = strings.TrimSpace(value.Role)
	value.AutoscalingGroup = strings.TrimSpace(value.AutoscalingGroup)
	if value.GroupID == "" || value.AutoscalingGroup == "" {
		return WorkerFleet{}, errors.New("group_id and autoscaling_group are required")
	}
	if value.Role != "run" && value.Role != "build" {
		return WorkerFleet{}, errors.New("role must be exactly run or build")
	}
	if value.MinWorkers < 0 || value.WarmWorkers < 0 || value.MaxWorkers <= 0 || value.MinWorkers > value.MaxWorkers || value.WarmWorkers > value.MaxWorkers || value.MaxWorkers > 10_000 {
		return WorkerFleet{}, errors.New("worker bounds must satisfy 0 <= min,warm <= max <= 10000")
	}
	if value.MaxScaleOutPerCycle <= 0 || value.MaxScaleOutPerCycle > value.MaxWorkers || value.MaxPendingWorkers < 0 || value.MaxPendingWorkers > value.MaxWorkers || value.MaxPackingItems <= 0 || value.MaxPackingItems > 1_000_000 {
		return WorkerFleet{}, errors.New("scale-out step and pending limit must be bounded by max_workers")
	}
	capacity := value.InstanceCapacity
	if capacity.MilliCPU == 0 || capacity.MemoryBytes == 0 || capacity.WorkloadDiskBytes == 0 || capacity.ScratchBytes == 0 {
		return WorkerFleet{}, errors.New("certified cpu, memory, workload disk, and scratch capacity must be positive")
	}
	if value.Role == "run" && capacity.VMSlots == 0 {
		return WorkerFleet{}, errors.New("run fleet vm_slots must be positive")
	}
	if value.Role == "run" && (value.QueuedRunScratchBytes == 0 || value.QueuedRunScratchBytes > capacity.ScratchBytes) {
		return WorkerFleet{}, errors.New("run fleet queued_run_scratch_bytes must be positive and fit instance scratch capacity")
	}
	if value.Role == "build" && value.QueuedRunScratchBytes != 0 {
		return WorkerFleet{}, errors.New("build fleet queued_run_scratch_bytes must be zero")
	}
	if value.Role == "build" && (capacity.BuildExecutors == 0 || capacity.BuildCacheBytes == 0 || capacity.ArtifactCacheBytes == 0) {
		return WorkerFleet{}, errors.New("build fleet executors and cache capacities must be positive")
	}
	durations := []struct {
		name    string
		seconds int
		out     *time.Duration
	}{
		{"scale_out_cooldown_seconds", value.ScaleOutCooldownSeconds, new(time.Duration)},
		{"scale_in_cooldown_seconds", value.ScaleInCooldownSeconds, new(time.Duration)},
		{"scale_in_hysteresis_seconds", value.ScaleInHysteresisSeconds, new(time.Duration)},
		{"stale_worker_timeout_seconds", value.StaleWorkerTimeoutSeconds, new(time.Duration)},
		{"readiness_timeout_seconds", value.ReadinessTimeoutSeconds, new(time.Duration)},
		{"drain_timeout_seconds", value.DrainTimeoutSeconds, new(time.Duration)},
		{"controller_interval_seconds", value.ControllerIntervalSeconds, new(time.Duration)},
		{"metric_interval_seconds", value.MetricIntervalSeconds, new(time.Duration)},
	}
	for _, item := range durations {
		if item.seconds <= 0 || item.seconds > int((30*24*time.Hour)/time.Second) {
			return WorkerFleet{}, fmt.Errorf("%s must be between 1 and 2592000", item.name)
		}
		*item.out = time.Duration(item.seconds) * time.Second
	}
	if *durations[6].out > *durations[4].out || *durations[7].out > *durations[4].out {
		return WorkerFleet{}, errors.New("controller and metric intervals must not exceed readiness_timeout_seconds")
	}
	keys := make([]string, 0, len(value.CompatibilityKeys))
	if len(value.CompatibilityKeys) == 0 {
		return WorkerFleet{}, errors.New("compatibility_keys must not be empty")
	}
	seen := map[string]struct{}{}
	for _, key := range value.CompatibilityKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			return WorkerFleet{}, errors.New("compatibility_keys must not contain empty values")
		}
		if _, exists := seen[key]; exists {
			return WorkerFleet{}, fmt.Errorf("duplicate compatibility key %q", key)
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return WorkerFleet{
		GroupID: value.GroupID, Role: value.Role, ASGName: value.AutoscalingGroup, CompatibilityKeys: keys,
		MilliCPU: capacity.MilliCPU, MemoryBytes: capacity.MemoryBytes, WorkloadDiskBytes: capacity.WorkloadDiskBytes,
		ScratchBytes: capacity.ScratchBytes, BuildCacheBytes: capacity.BuildCacheBytes, ArtifactCacheBytes: capacity.ArtifactCacheBytes,
		VMSlots: capacity.VMSlots, BuildExecutors: capacity.BuildExecutors, MinWorkers: value.MinWorkers, WarmWorkers: value.WarmWorkers,
		QueuedRunScratchBytes: value.QueuedRunScratchBytes,
		MaxWorkers:            value.MaxWorkers, MaxScaleOutPerCycle: value.MaxScaleOutPerCycle, MaxPending: value.MaxPendingWorkers, MaxPackingItems: value.MaxPackingItems,
		ScaleOutCooldown: *durations[0].out, ScaleInCooldown: *durations[1].out, ScaleInHysteresis: *durations[2].out,
		StaleWorkerTimeout: *durations[3].out, ReadinessTimeout: *durations[4].out, DrainTimeout: *durations[5].out,
		EmergencyStop:      value.EmergencyStop,
		ControllerInterval: *durations[6].out, MetricsInterval: *durations[7].out,
	}, nil
}
