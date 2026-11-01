// Package executionlimit holds the operator-configured admission and capacity
// bounds of an Environment.
package executionlimit

import (
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
)

// Limits are the operator-configured admission and capacity bounds of an
// Environment. There are no implicit production capacity or rate defaults and
// no Environment without limits.
type Limits struct {
	MaxResidentComputers     int64
	MaxCPUMillis             int64
	MaxMemoryBytes           int64
	MaxReservedStorageBytes  int64
	MaxOutstandingAdmissions int64
	AdmissionRatePerSecond   int64
	AdmissionBurst           int64
}

func (l Limits) Validate() error {
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
func (l Limits) For(env uuid.UUID) Limits {
	return l
}

// ExecutionBudget is the aggregate CPU, memory and resident Computer bound
// before current usage is subtracted.
func (l Limits) ExecutionBudget() db.ExecutionBudget {
	return db.ExecutionBudget{CPU: l.MaxCPUMillis, Memory: l.MaxMemoryBytes, Residents: l.MaxResidentComputers}
}
