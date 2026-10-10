package vm

import (
	"math"
	"strings"
	"testing"
)

func TestResourcesValidate(t *testing.T) {
	if err := (Resources{MilliCPU: 1000, MemoryMiB: 512, Slots: 1}).Validate(); err != nil {
		t.Fatalf("zero disk request rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		resources Resources
		want      string
	}{
		"zero cpu":      {Resources{MemoryMiB: 512, Slots: 1}, "milli_cpu must be positive"},
		"zero memory":   {Resources{MilliCPU: 1000, Slots: 1}, "memory_mib must be positive"},
		"negative disk": {Resources{MilliCPU: 1000, MemoryMiB: 512, DiskMiB: -1, Slots: 1}, "disk_mib must not be negative"},
		"zero slots":    {Resources{MilliCPU: 1000, MemoryMiB: 512}, "slots must be positive"},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.resources.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestVCPUCountForMilliCPUIsOverflowSafe(t *testing.T) {
	tests := []struct {
		milliCPU int64
		want     int64
	}{
		{milliCPU: 1, want: 1},
		{milliCPU: 999, want: 1},
		{milliCPU: 1000, want: 1},
		{milliCPU: 1001, want: 2},
		{milliCPU: 2000, want: 2},
		{milliCPU: math.MaxInt64, want: (math.MaxInt64-1)/1000 + 1},
	}
	for _, test := range tests {
		got, err := VCPUCountForMilliCPU(test.milliCPU)
		if err != nil {
			t.Fatalf("VCPUCountForMilliCPU(%d): %v", test.milliCPU, err)
		}
		if got != test.want {
			t.Fatalf("VCPUCountForMilliCPU(%d) = %d, want %d", test.milliCPU, got, test.want)
		}
	}
	for _, invalid := range []int64{0, -1} {
		if _, err := VCPUCountForMilliCPU(invalid); err == nil {
			t.Fatalf("VCPUCountForMilliCPU(%d) succeeded", invalid)
		}
	}
}

func TestReservedCPUMillisUsesPhysicalShapeWithoutOverflow(t *testing.T) {
	for request, want := range map[int64]int64{1: 1000, 500: 1000, 1000: 1000, 1001: 2000, math.MaxInt64 / 1000 * 1000: math.MaxInt64 / 1000 * 1000} {
		got, err := ReservedCPUMillis(request)
		if err != nil || got != want {
			t.Fatalf("request %d: got %d want %d: %v", request, got, want, err)
		}
	}
	for _, request := range []int64{-1, 0, math.MaxInt64/1000*1000 + 1, math.MaxInt64} {
		if _, err := ReservedCPUMillis(request); err == nil {
			t.Fatalf("invalid or overflowing request %d accepted", request)
		}
	}
}
