package config

import (
	"fmt"
	"strconv"
	"time"
)

type DeploymentUploadLimits struct {
	ProcessConcurrency      int
	OrganizationConcurrency int
	IdleTimeout             time.Duration
	MaxDuration             time.Duration
}

func LoadDeploymentUploadLimits() (DeploymentUploadLimits, error) {
	var limits DeploymentUploadLimits
	for _, field := range []struct {
		name   string
		target *int
	}{
		{"DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY", &limits.ProcessConcurrency},
		{"DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY", &limits.OrganizationConcurrency},
	} {
		value, err := strconv.Atoi(envText(field.name))
		if err != nil || value <= 0 {
			return limits, fmt.Errorf("%s must be explicitly configured as a positive integer", field.name)
		}
		*field.target = value
	}
	for _, field := range []struct {
		name   string
		target *time.Duration
	}{
		{"DEPLOYMENT_UPLOAD_IDLE_TIMEOUT", &limits.IdleTimeout},
		{"DEPLOYMENT_UPLOAD_MAX_DURATION", &limits.MaxDuration},
	} {
		value, err := time.ParseDuration(envText(field.name))
		if err != nil || value < time.Millisecond {
			return limits, fmt.Errorf("%s must be explicitly configured as a duration of at least 1ms", field.name)
		}
		*field.target = value
	}
	if limits.OrganizationConcurrency > limits.ProcessConcurrency {
		return limits, fmt.Errorf("DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY must not exceed DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY")
	}
	if limits.IdleTimeout > limits.MaxDuration {
		return limits, fmt.Errorf("DEPLOYMENT_UPLOAD_IDLE_TIMEOUT must not exceed DEPLOYMENT_UPLOAD_MAX_DURATION")
	}
	return limits, nil
}
