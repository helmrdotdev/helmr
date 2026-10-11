package config

import (
	"fmt"
	"strconv"
)

type EnvironmentExecutionLimits struct {
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

// LoadEnvironmentExecutionLimits reads explicit policy for newly created Environments.
func LoadEnvironmentExecutionLimits() (EnvironmentExecutionLimits, error) {
	var limits EnvironmentExecutionLimits
	for _, field := range []struct {
		name   string
		target *int64
	}{
		{"ENVIRONMENT_MAX_RESIDENT_COMPUTERS", &limits.MaxResidentComputers},
		{"ENVIRONMENT_MAX_CPU_MILLIS", &limits.MaxCPUMillis},
		{"ENVIRONMENT_MAX_MEMORY_BYTES", &limits.MaxMemoryBytes},
		{"ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES", &limits.MaxReservedStorageBytes},
		{"ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS", &limits.MaxOutstandingAdmissions},
		{"ENVIRONMENT_MAX_CAUSAL_DEPTH", &limits.MaxCausalDepth},
		{"ENVIRONMENT_ADMISSION_RATE_PER_SECOND", &limits.AdmissionRatePerSecond},
		{"ENVIRONMENT_ADMISSION_BURST", &limits.AdmissionBurst},
		{"ENVIRONMENT_PREPARATION_TIMEOUT_MS", &limits.PreparationTimeoutMS},
	} {
		value, err := strconv.ParseInt(envText(field.name), 10, 64)
		if err != nil || value <= 0 {
			return EnvironmentExecutionLimits{}, fmt.Errorf("%s must be explicitly configured as a positive integer", field.name)
		}
		*field.target = value
	}
	return limits, nil
}
