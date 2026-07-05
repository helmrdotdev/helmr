package workergroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	ComponentControl    = "control"
	ComponentDispatcher = "dispatcher"
	ComponentDevControl = "dev-control"

	defaultHealthReportInterval = 30 * time.Second
	defaultHealthReportTimeout  = 10 * time.Second
)

type HealthStore interface {
	ReportWorkerGroupHealth(context.Context, db.ReportWorkerGroupHealthParams) (db.WorkerGroup, error)
}

func RoutingRequiredComponents() []string {
	return []string{ComponentDispatcher}
}

func DevRoutingRequiredComponents() []string {
	return []string{ComponentDevControl}
}

type HealthConfig struct {
	WorkerGroupID      string
	Component          string
	RequiredComponents []string
	State              db.WorkerGroupHealthState
	FreshFor           time.Duration
	Details            []byte
}

func ReportHealth(ctx context.Context, store HealthStore, cfg HealthConfig) error {
	if store == nil {
		return errors.New("worker group health store is required")
	}
	workerGroupID := strings.TrimSpace(cfg.WorkerGroupID)
	if workerGroupID == "" {
		return errors.New("worker group id is required")
	}
	component := strings.TrimSpace(cfg.Component)
	if component == "" {
		return errors.New("worker group health component is required")
	}
	requiredComponents := normalizeRequiredComponents(cfg.RequiredComponents)
	if len(requiredComponents) == 0 {
		return errors.New("worker group health required components are required")
	}
	state := cfg.State
	if state == "" {
		state = db.WorkerGroupHealthStateHealthy
	}
	freshFor := cfg.FreshFor
	if freshFor <= 0 {
		freshFor = defaultRoutingFreshness
	}
	details := cfg.Details
	if len(details) == 0 {
		details = []byte(`{}`)
	}
	if _, err := store.ReportWorkerGroupHealth(ctx, db.ReportWorkerGroupHealthParams{
		WorkerGroupID: workerGroupID,
		HealthState:   state,
		FreshFor:      pgtype.Interval{Microseconds: freshFor.Microseconds(), Valid: true},
		HealthDetails: details,
	}); err != nil {
		return fmt.Errorf("report worker group health: %w", err)
	}
	return nil
}

type HealthReporterConfig struct {
	WorkerGroupID      string
	Component          string
	RequiredComponents []string
	Interval           time.Duration
	ReportTimeout      time.Duration
	FreshFor           time.Duration
	Probe              func(context.Context) (db.WorkerGroupHealthState, []byte, error)
	Log                *slog.Logger
}

func RunHealthReporter(ctx context.Context, store HealthStore, cfg HealthReporterConfig) error {
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultHealthReportInterval
	}
	reportTimeout := cfg.ReportTimeout
	if reportTimeout <= 0 {
		reportTimeout = defaultHealthReportTimeout
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	report := func() error {
		probeCtx, cancelProbe := context.WithTimeout(ctx, reportTimeout)
		state := db.WorkerGroupHealthStateHealthy
		details := []byte(`{}`)
		if cfg.Probe != nil {
			probeState, probeDetails, err := cfg.Probe(probeCtx)
			if probeState != "" {
				state = probeState
			}
			if len(probeDetails) != 0 {
				details = probeDetails
			}
			if err != nil {
				state = db.WorkerGroupHealthStateUnavailable
				details = healthErrorDetails(err)
			}
		}
		cancelProbe()
		reportCtx, cancelReport := context.WithTimeout(ctx, reportTimeout)
		defer cancelReport()
		return ReportHealth(reportCtx, store, HealthConfig{
			WorkerGroupID:      cfg.WorkerGroupID,
			Component:          cfg.Component,
			RequiredComponents: cfg.RequiredComponents,
			State:              state,
			FreshFor:           cfg.FreshFor,
			Details:            details,
		})
	}
	if err := report(); err != nil {
		log.Warn("worker group health report failed", "worker_group_id", cfg.WorkerGroupID, "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := report(); err != nil {
				log.Warn("worker group health report failed", "worker_group_id", cfg.WorkerGroupID, "error", err)
			}
		}
	}
}

func healthErrorDetails(err error) []byte {
	if err == nil {
		return []byte(`{}`)
	}
	payload, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		return []byte(`{"error":"health probe failed"}`)
	}
	return payload
}

func normalizeRequiredComponents(components []string) []string {
	seen := make(map[string]struct{}, len(components))
	out := make([]string, 0, len(components))
	for _, component := range components {
		component = strings.TrimSpace(component)
		if component == "" {
			continue
		}
		if _, ok := seen[component]; ok {
			continue
		}
		seen[component] = struct{}{}
		out = append(out, component)
	}
	return out
}
