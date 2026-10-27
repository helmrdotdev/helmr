package config

import (
	"testing"
	"time"
)

func setDeploymentUploadEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY", "24")
	t.Setenv("DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY", "24")
	t.Setenv("DEPLOYMENT_UPLOAD_IDLE_TIMEOUT", "3m")
	t.Setenv("DEPLOYMENT_UPLOAD_MAX_DURATION", "4h")
}

func TestDeploymentUploadConfiguration(t *testing.T) {
	setDeploymentUploadEnv(t)
	limits, err := LoadDeploymentUploadLimits()
	if err != nil || limits.ProcessConcurrency != 24 || limits.OrganizationConcurrency != 24 || limits.IdleTimeout != 3*time.Minute || limits.MaxDuration != 4*time.Hour {
		t.Fatalf("limits=%+v error=%v", limits, err)
	}
	for _, test := range []struct{ key, value string }{
		{"DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY", ""},
		{"DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY", "0"},
		{"DEPLOYMENT_UPLOAD_PROCESS_CONCURRENCY", "9999999999999999999999"},
		{"DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY", "-1"},
		{"DEPLOYMENT_UPLOAD_ORGANIZATION_CONCURRENCY", "25"},
		{"DEPLOYMENT_UPLOAD_IDLE_TIMEOUT", ""},
		{"DEPLOYMENT_UPLOAD_IDLE_TIMEOUT", "1ns"},
		{"DEPLOYMENT_UPLOAD_MAX_DURATION", "0s"},
		{"DEPLOYMENT_UPLOAD_MAX_DURATION", "1m"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			setDeploymentUploadEnv(t)
			t.Setenv(test.key, test.value)
			if _, err := LoadDeploymentUploadLimits(); err == nil {
				t.Fatal("invalid upload configuration accepted")
			}
		})
	}
}

func TestControlPlaneHTTPIdleTimeout(t *testing.T) {
	t.Setenv("CONTROL_PLANE_HTTP_IDLE_TIMEOUT", "210s")
	if value, err := LoadControlPlaneHTTPIdleTimeout(); err != nil || value != 210*time.Second {
		t.Fatalf("idle=%s error=%v", value, err)
	}
	for _, value := range []string{"-1s", "0s", "not-a-duration"} {
		t.Setenv("CONTROL_PLANE_HTTP_IDLE_TIMEOUT", value)
		if _, err := LoadControlPlaneHTTPIdleTimeout(); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}
