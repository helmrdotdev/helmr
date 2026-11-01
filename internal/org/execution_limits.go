package org

import (
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
)

// ExecutionLimits are the operator-configured admission and capacity bounds of
// an Environment. There are no implicit production capacity or rate defaults
// and no Environment without limits.
type ExecutionLimits struct {
	MaxResidentComputers     int64
	MaxCPUMillis             int64
	MaxMemoryBytes           int64
	MaxReservedStorageBytes  int64
	MaxOutstandingAdmissions int64
	AdmissionRatePerSecond   int64
	AdmissionBurst           int64
}

func (l ExecutionLimits) Validate() error {
	for _, field := range []struct {
		name  string
		value int64
	}{
		{"resident Computers", l.MaxResidentComputers}, {"CPU", l.MaxCPUMillis}, {"memory", l.MaxMemoryBytes}, {"storage", l.MaxReservedStorageBytes}, {"outstanding admissions", l.MaxOutstandingAdmissions}, {"admission rate", l.AdmissionRatePerSecond}, {"admission burst", l.AdmissionBurst},
	} {
		if field.value <= 0 {
			return fmt.Errorf("execution limit %s must be configured and positive", field.name)
		}
	}
	return nil
}

// For returns the limits that apply to env. Every Environment currently uses
// the configured limits.
func (l ExecutionLimits) For(env uuid.UUID) ExecutionLimits {
	return l
}

// ExecutionBudget is the aggregate CPU, memory and resident Computer bound
// before current usage is subtracted.
func (l ExecutionLimits) ExecutionBudget() db.ExecutionBudget {
	return db.ExecutionBudget{CPU: l.MaxCPUMillis, Memory: l.MaxMemoryBytes, Residents: l.MaxResidentComputers}
}
