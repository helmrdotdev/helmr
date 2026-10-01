package vm

import (
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
