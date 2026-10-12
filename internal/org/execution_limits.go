package org

import (
	"fmt"
	"math"
	"time"
)

// ExecutionLimits are operator-supplied values written with each newly created
// Environment; the token bucket starts full. There are no implicit production
// capacity or rate defaults and no Environment without limits.
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
