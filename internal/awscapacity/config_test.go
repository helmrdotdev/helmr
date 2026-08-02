package awscapacity

import (
	"testing"
	"time"
)

func TestDecodeConfigRejectsNonCanonicalNamesAndTrailingJSON(t *testing.T) {
	valid := `[{"worker_group_id":"run-workers","autoscaling_group_name":"run-asg","termination_lifecycle_hook_name":"run-terminate","allows_run":true,"allows_build":false,"instance_capacity":{"cpu_millis":1000,"memory_bytes":1073741824,"guest_ephemeral_disk_bytes":34359738368,"vm_slots":1,"run_consumers":1}}]`
	for _, raw := range []string{
		`[{"worker_group_id":" run-workers","autoscaling_group_name":"run-asg","termination_lifecycle_hook_name":"run-terminate","allows_run":true,"allows_build":false,"instance_capacity":{"cpu_millis":1000,"memory_bytes":1073741824,"guest_ephemeral_disk_bytes":34359738368,"vm_slots":1,"run_consumers":1}}]`,
		valid + `{}`,
	} {
		if _, err := DecodeConfig(raw, time.Minute); err == nil {
			t.Fatalf("DecodeConfig accepted %q", raw)
		}
	}
}

func TestDecodeConfigRequiresEveryRunConcurrencyDimension(t *testing.T) {
	raw := `[{"worker_group_id":"run-workers","autoscaling_group_name":"run-asg","termination_lifecycle_hook_name":"run-terminate","allows_run":true,"allows_build":false,"instance_capacity":{"cpu_millis":1000,"memory_bytes":1073741824,"guest_ephemeral_disk_bytes":34359738368,"vm_slots":1}}]`
	if _, err := DecodeConfig(raw, time.Minute); err == nil {
		t.Fatal("Run capacity without run_consumers was accepted")
	}
}
