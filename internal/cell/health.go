package cell

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
	UpsertCellComponentHealth(context.Context, db.UpsertCellComponentHealthParams) (db.CellComponentHealth, error)
	RefreshCellHealthFromComponents(context.Context, db.RefreshCellHealthFromComponentsParams) (db.CellHealth, error)
}

func RoutingRequiredComponents() []string {
	return []string{ComponentDispatcher}
}

func DevRoutingRequiredComponents() []string {
	return []string{ComponentDevControl}
}

type HealthConfig struct {
	CellID             string
	Component          string
	RequiredComponents []string
	State              db.CellHealthState
	FreshFor           time.Duration
	Details            []byte
}

func ReportHealth(ctx context.Context, store HealthStore, cfg HealthConfig) error {
	if store == nil {
		return errors.New("cell health store is required")
	}
	cellID := strings.TrimSpace(cfg.CellID)
	if cellID == "" {
		return errors.New("cell id is required")
	}
	component := strings.TrimSpace(cfg.Component)
	if component == "" {
		return errors.New("cell health component is required")
	}
	requiredComponents := normalizeRequiredComponents(cfg.RequiredComponents)
	if len(requiredComponents) == 0 {
		return errors.New("cell health required components are required")
	}
	state := cfg.State
	if state == "" {
		state = db.CellHealthStateHealthy
	}
	freshFor := cfg.FreshFor
	if freshFor <= 0 {
		freshFor = defaultRoutingFreshness
	}
	details := cfg.Details
	if len(details) == 0 {
		details = []byte(`{}`)
	}
	if _, err := store.UpsertCellComponentHealth(ctx, db.UpsertCellComponentHealthParams{
		CellID:    cellID,
		Component: component,
		State:     state,
		FreshFor:  pgtype.Interval{Microseconds: freshFor.Microseconds(), Valid: true},
		Details:   details,
	}); err != nil {
		return fmt.Errorf("report cell component health: %w", err)
	}
	if _, err := store.RefreshCellHealthFromComponents(ctx, db.RefreshCellHealthFromComponentsParams{
		CellID:             cellID,
		RequiredComponents: requiredComponents,
	}); err != nil {
		return fmt.Errorf("refresh cell health: %w", err)
	}
	return nil
}

type HealthReporterConfig struct {
	CellID             string
	Component          string
	RequiredComponents []string
	Interval           time.Duration
	ReportTimeout      time.Duration
	FreshFor           time.Duration
	Probe              func(context.Context) (db.CellHealthState, []byte, error)
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
		state := db.CellHealthStateHealthy
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
				state = db.CellHealthStateUnavailable
				details = healthErrorDetails(err)
			}
		}
		cancelProbe()
		reportCtx, cancelReport := context.WithTimeout(ctx, reportTimeout)
		defer cancelReport()
		return ReportHealth(reportCtx, store, HealthConfig{
			CellID:             cfg.CellID,
			Component:          cfg.Component,
			RequiredComponents: cfg.RequiredComponents,
			State:              state,
			FreshFor:           cfg.FreshFor,
			Details:            details,
		})
	}
	if err := report(); err != nil {
		log.Warn("cell health report failed", "cell_id", cfg.CellID, "error", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := report(); err != nil {
				log.Warn("cell health report failed", "cell_id", cfg.CellID, "error", err)
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
