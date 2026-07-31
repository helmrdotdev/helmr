package firecracker

import (
	"strings"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	if cfg.FirecrackerPath != DefaultFirecrackerPath || cfg.VCPUCount != DefaultVCPUs || cfg.MemoryMiB != DefaultMemoryMiB {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.JailerPath != DefaultJailerPath || cfg.JailerChrootBaseDir == "" || cfg.CgroupVersion != DefaultCgroupVersion {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.GuestPort != DefaultGuestPort || cfg.HealthPort != HealthPort || cfg.StateDir == "" || cfg.HealthTimeout != DefaultHealthTimeout || cfg.HealthAttemptTimeout != DefaultHealthAttemptTimeout {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestConfigDefaultsClampHealthAttemptToShortHealthTimeout(t *testing.T) {
	cfg := (Config{HealthTimeout: time.Second}).WithDefaults()
	if cfg.HealthAttemptTimeout != time.Second {
		t.Fatalf("HealthAttemptTimeout = %s, want 1s", cfg.HealthAttemptTimeout)
	}
}

func TestConfigValidateRejectsHealthAttemptLongerThanHealthTimeout(t *testing.T) {
	cfg := (Config{
		HealthTimeout:        time.Second,
		HealthAttemptTimeout: 2 * time.Second,
	}).WithDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	if got, want := err.Error(), "guest health attempt timeout"; !strings.Contains(got, want) {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestConfigValidateRequiresBootInputs(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	text := err.Error()
	for _, want := range []string{"firecracker jailer uid", "firecracker jailer gid", "guest kernel path", "guest initramfs path", "guest rootfs path", "worker network link pool", "worker network translation pool", "worker network resolver IPv4", "worker network capacity"} {
		if !strings.Contains(text, want) {
			t.Fatalf("error %q does not contain %q", text, want)
		}
	}
}
