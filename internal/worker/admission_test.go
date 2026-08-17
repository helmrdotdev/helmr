package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

type staticHealthProbe struct {
	health HostHealth
	err    error
}

func (p *staticHealthProbe) Probe(context.Context) (HostHealth, error) { return p.health, p.err }

func healthyHost(now time.Time) HostHealth {
	return HostHealth{
		ObservedAt: now, AvailableDiskBytes: 20 << 30, DiskCapacityBytes: 40 << 30,
		OpenFileDescriptors: 100, FileDescriptorLimit: 4096,
		CgroupHealthy: true,
		KVMHealthy:    true, FirecrackerHealthy: true,
	}
}

func TestHardAdmissionFailClosedChecks(t *testing.T) {
	now := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
	probe := &staticHealthProbe{health: healthyHost(now)}
	evaluator, err := NewHardAdmission(HardAdmissionConfig{
		Probe: probe, DiskFloorBytes: 8 << 30, FDHeadroom: 256, RuntimeSlotCount: 2, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	base := AdmissionCheck{Consumer: "run", State: StateActive}
	tests := []struct {
		name   string
		mutate func(*HostHealth, *AdmissionCheck)
		want   AdmissionReason
	}{
		{name: "healthy", want: AdmissionAllowed},
		{name: "disk", mutate: func(h *HostHealth, _ *AdmissionCheck) { h.AvailableDiskBytes = 7 << 30 }, want: AdmissionDiskFloor},
		{name: "fd", mutate: func(h *HostHealth, _ *AdmissionCheck) { h.OpenFileDescriptors = 3900 }, want: AdmissionFileDescriptorPressure},
		{name: "cgroup", mutate: func(h *HostHealth, _ *AdmissionCheck) { h.CgroupHealthy = false }, want: AdmissionCgroupUnavailable},
		{name: "kvm", mutate: func(h *HostHealth, _ *AdmissionCheck) { h.KVMHealthy = false }, want: AdmissionKVMUnavailable},
		{name: "firecracker", mutate: func(h *HostHealth, _ *AdmissionCheck) { h.FirecrackerHealthy = false }, want: AdmissionFirecrackerUnavailable},
		{name: "slots", mutate: func(_ *HostHealth, c *AdmissionCheck) {
			c.Consumer = "workspace"
			c.Recovery.Quarantined = []string{"one", "two"}
		}, want: AdmissionRuntimeSlotsQuarantined},
		{name: "partial quarantine plus active slot", mutate: func(_ *HostHealth, c *AdmissionCheck) {
			c.Consumer = "workspace"
			c.Recovery.Quarantined = []string{"one"}
			c.Snapshot = Snapshot{Active: map[string]int{"workspace": 1}}
		}, want: AdmissionRuntimeSlotsQuarantined},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := healthyHost(now)
			check := base
			if tt.mutate != nil {
				tt.mutate(&health, &check)
			}
			probe.health = health
			decision := evaluator.Evaluate(context.Background(), check)
			if decision.Reason != tt.want || decision.Allowed != (tt.want == AdmissionAllowed) {
				t.Fatalf("decision = %+v, want reason %q", decision, tt.want)
			}
		})
	}
}

func TestHardAdmissionFailsClosedWhenDatapathChanges(t *testing.T) {
	now := time.Now()
	datapathHealthy := true
	evaluator, err := NewHardAdmission(HardAdmissionConfig{
		Probe:          &staticHealthProbe{health: healthyHost(now)},
		DiskFloorBytes: 1, FDHeadroom: 1, RuntimeSlotCount: 1,
		Now: func() time.Time { return now },
		DatapathHealth: func() error {
			if datapathHealthy {
				return nil
			}
			return errors.New("binding changed")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	check := AdmissionCheck{Consumer: "run", State: StateActive}
	if decision := evaluator.Evaluate(context.Background(), check); !decision.Allowed {
		t.Fatalf("healthy datapath decision = %+v", decision)
	}
	datapathHealthy = false
	if decision := evaluator.Evaluate(context.Background(), check); decision.Allowed || decision.Reason != AdmissionDatapathUnverified {
		t.Fatalf("changed datapath decision = %+v", decision)
	}
	observation := evaluator.Observation()
	if observation.RunPausedReason != string(AdmissionDatapathUnverified) ||
		observation.RuntimePausedReason != string(AdmissionDatapathUnverified) {
		t.Fatalf("datapath observation = %+v", observation)
	}
}

func TestHardAdmissionKeepsRuntimeSlotPressureInRuntimeDomain(t *testing.T) {
	now := time.Now()
	probe := &staticHealthProbe{health: healthyHost(now)}
	evaluator, err := NewHardAdmission(HardAdmissionConfig{Probe: probe, DiskFloorBytes: 1, FDHeadroom: 1, RuntimeSlotCount: 1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	check := AdmissionCheck{State: StateActive, Recovery: RecoveryEvidence{Quarantined: []string{"slot"}}}
	check.Consumer = "run"
	evaluator.Evaluate(context.Background(), check)
	check.Consumer = "runtime"
	evaluator.Evaluate(context.Background(), check)
	observation := evaluator.Observation()
	if observation.RunPausedReason != "" || observation.RuntimePausedReason == "" {
		t.Fatalf("domain pauses = run:%q runtime:%q", observation.RunPausedReason, observation.RuntimePausedReason)
	}
}

func TestHardAdmissionAllowsRunInsideActiveWorkspaceSlot(t *testing.T) {
	now := time.Now()
	probe := &staticHealthProbe{health: healthyHost(now)}
	evaluator, err := NewHardAdmission(HardAdmissionConfig{
		Probe: probe, DiskFloorBytes: 1, FDHeadroom: 1, RuntimeSlotCount: 1,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := evaluator.Evaluate(context.Background(), AdmissionCheck{
		Consumer: "run", State: StateActive,
		Snapshot: Snapshot{Active: map[string]int{"workspace": 1}},
	})
	if !decision.Allowed {
		t.Fatalf("run inside mounted workspace rejected: %+v", decision)
	}
}

func TestHardAdmissionAllowsOnlyExplicitDrainContinuation(t *testing.T) {
	now := time.Now()
	evaluator, err := NewHardAdmission(HardAdmissionConfig{
		Probe: &staticHealthProbe{health: healthyHost(now)}, DiskFloorBytes: 1,
		FDHeadroom: 1, RuntimeSlotCount: 1, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision := evaluator.Evaluate(context.Background(), AdmissionCheck{
		Consumer: "run", State: StateDraining,
	}); decision.Allowed || decision.Reason != AdmissionReason(StateDraining) {
		t.Fatalf("ordinary draining decision = %+v", decision)
	}
	if decision := evaluator.Evaluate(context.Background(), AdmissionCheck{
		Consumer: "run", State: StateDraining, DrainContinuation: true,
	}); !decision.Allowed {
		t.Fatalf("bound drain continuation rejected: %+v", decision)
	}
}
