package main

import (
	"github.com/helmrdotdev/helmr/internal/org"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAcceptsEmptyBootstrap(t *testing.T) {
	setDevRegionConfig(t)
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigRejectsInvalidDevBoolean(t *testing.T) {
	setDevRegionConfig(t)
	t.Setenv("HELMR_DEV_SEED_DATA", "yes")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "HELMR_DEV_SEED_DATA must be a boolean") {
		t.Fatalf("boolean error = %v", err)
	}
}

func TestDecodeRootKeyRejectsSurroundingWhitespace(t *testing.T) {
	if _, err := decodeRootKey("AUTH_KEY", " AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err == nil {
		t.Fatal("root key with surrounding whitespace was accepted")
	}
}

func TestLoadConfigRejectsSetupTokenWhitespace(t *testing.T) {
	setDevRegionConfig(t)
	t.Setenv("SETUP_TOKEN", " dev-setup-token")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "SETUP_TOKEN must not have surrounding whitespace") {
		t.Fatalf("setup token error = %v", err)
	}
}

func setDevRegionConfig(t *testing.T) {
	t.Helper()
	t.Setenv("ENVIRONMENT_MAX_RESIDENT_COMPUTERS", "10")
	t.Setenv("ENVIRONMENT_MAX_CPU_MILLIS", "16000")
	t.Setenv("ENVIRONMENT_MAX_MEMORY_BYTES", "68719476736")
	t.Setenv("ENVIRONMENT_MAX_RESERVED_STORAGE_BYTES", "1099511627776")
	t.Setenv("ENVIRONMENT_MAX_OUTSTANDING_ADMISSIONS", "100")
	t.Setenv("ENVIRONMENT_MAX_CAUSAL_DEPTH", "8")
	t.Setenv("ENVIRONMENT_ADMISSION_RATE_PER_SECOND", "10")
	t.Setenv("ENVIRONMENT_ADMISSION_BURST", "20")
	t.Setenv("ENVIRONMENT_PREPARATION_TIMEOUT_MS", "600000")
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH", "/etc/helmr/runtime.descriptor.json")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
	t.Setenv("CAS_URI", "s3://helmr-dev-cas")
	t.Setenv("PLATFORM_STORE_URI", "s3://helmr-dev-platform")
}

func TestLoadConfigRequiresS3Stores(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"CAS_URI is required":            {"CAS_URI": ""},
		"PLATFORM_STORE_URI is required": {"PLATFORM_STORE_URI": ""},
		"invalid S3 store URI":           {"CAS_URI": "file:///tmp/helmr-dev-cas"},
		"distinct bucket authority":      {"PLATFORM_STORE_URI": "s3://helmr-dev-cas/platform"},
	} {
		t.Run(name, func(t *testing.T) {
			setDevRegionConfig(t)
			for key, value := range env {
				t.Setenv(key, value)
			}
			if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("store config error = %v", err)
			}
		})
	}
}

func TestMigrationPathsFindsSourceRootWhenCwdDiffers(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	paths, err := migrationPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("expected migration paths")
	}
	for _, path := range paths {
		if filepath.IsAbs(path) {
			continue
		}
		t.Fatalf("expected fallback migration path to be absolute, got %q", path)
	}
}

func devTestExecutionLimits() org.ExecutionLimits {
	return org.ExecutionLimits{MaxResidentComputers: 10, MaxCPUMillis: 16000, MaxMemoryBytes: 1 << 36, MaxReservedStorageBytes: 1 << 40, MaxOutstandingAdmissions: 100, MaxCausalDepth: 8, AdmissionRatePerSecond: 10, AdmissionBurst: 20, PreparationTimeoutMS: 600000}
}
