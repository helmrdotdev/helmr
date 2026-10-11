package org

import (
	"context"
	"fmt"
	"math"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// ExecutionLimits are operator-supplied values copied into each newly created
// Environment. There are no implicit production capacity or rate defaults.
type ExecutionLimits struct {
	MaxResidentComputers     int64
	MaxCPUMillis             int64
	MaxMemoryBytes           int64
	MaxReservedStorageBytes  int64
	MaxOutstandingAdmissions int64
	MaxCausalDepth           int64
	AdmissionRatePerSecond   int64
	AdmissionBurst           int64
	PreparationTimeoutMS     int64
}

func (p ExecutionLimits) Validate() error {
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"resident Computers", p.MaxResidentComputers}, {"CPU", p.MaxCPUMillis}, {"memory", p.MaxMemoryBytes}, {"storage", p.MaxReservedStorageBytes}, {"outstanding admissions", p.MaxOutstandingAdmissions}, {"causal depth", p.MaxCausalDepth}, {"admission rate", p.AdmissionRatePerSecond}, {"admission burst", p.AdmissionBurst}, {"preparation timeout", p.PreparationTimeoutMS},
	} {
		if field.value <= 0 {
			return fmt.Errorf("execution limit %s must be configured and positive", field.name)
		}
	}
	if p.MaxCausalDepth > math.MaxInt32 || p.PreparationTimeoutMS > math.MaxInt64/int64(time.Millisecond) {
		return fmt.Errorf("execution limit exceeds supported range")
	}
	return nil
}

// InitializeEnvironment configures a newly created Environment once, within its
// owner transaction. It never resets an existing policy or admission tokens.
func (p ExecutionLimits) InitializeEnvironment(ctx context.Context, tx pgx.Tx, env uuid.UUID) error {
	if err := p.Validate(); err != nil {
		return err
	}
	updated, err := tx.Exec(ctx, `UPDATE environments SET max_resident_computers=$2,max_cpu_millis=$3,max_memory_bytes=$4,max_reserved_storage_bytes=$5,max_outstanding_admissions=$6,max_causal_depth=$7,admission_rate_per_second=$8,admission_burst=$9,admission_tokens=$9::bigint,admission_refilled_at=clock_timestamp(),preparation_timeout_ms=$10 WHERE id=$1 AND admission_rate_per_second IS NULL`, env, p.MaxResidentComputers, p.MaxCPUMillis, p.MaxMemoryBytes, p.MaxReservedStorageBytes, p.MaxOutstandingAdmissions, p.MaxCausalDepth, p.AdmissionRatePerSecond, p.AdmissionBurst, p.PreparationTimeoutMS)
	if err != nil {
		return err
	}
	if updated.RowsAffected() != 1 {
		return fmt.Errorf("environment execution limits already initialized or Environment missing")
	}
	return nil
}
