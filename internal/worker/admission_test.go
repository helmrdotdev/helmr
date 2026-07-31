package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
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
		CgroupHealthy: true, ProgramVerifierHealthy: true,
		KVMHealthy: true, FirecrackerHealthy: true,
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
	base := AdmissionCheck{Consumer: "run", State: StateActive, CertifiedAt: now.Add(-time.Minute), CertificationTTL: time.Hour}
	tests := []struct {
		name   string
		mutate func(*HostHealth, *AdmissionCheck)
		want   AdmissionReason
	}{
		{name: "healthy", want: AdmissionAllowed},
		{name: "disk", mutate: func(h *HostHealth, _ *AdmissionCheck) { h.AvailableDiskBytes = 7 << 30 }, want: AdmissionDiskFloor},
		{name: "fd", mutate: func(h *HostHealth, _ *AdmissionCheck) { h.OpenFileDescriptors = 3900 }, want: AdmissionFileDescriptorPressure},
		{name: "cgroup", mutate: func(h *HostHealth, _ *AdmissionCheck) { h.CgroupHealthy = false }, want: AdmissionCgroupUnavailable},
		{name: "program verifier for build", mutate: func(h *HostHealth, c *AdmissionCheck) {
			h.ProgramVerifierHealthy = false
			c.Consumer = "build"
		}, want: AdmissionProgramVerifierUnavailable},
		{name: "program verifier does not own run admission", mutate: func(h *HostHealth, _ *AdmissionCheck) {
			h.ProgramVerifierHealthy = false
		}, want: AdmissionAllowed},
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
		{name: "certification", mutate: func(_ *HostHealth, c *AdmissionCheck) { c.CertifiedAt = now.Add(-2 * time.Hour) }, want: AdmissionCertificationStale},
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

func TestHardAdmissionKeepsVerifierFailureInBuildDomain(t *testing.T) {
	now := time.Now()
	health := healthyHost(now)
	health.ProgramVerifierHealthy = false
	probe := &staticHealthProbe{health: health}
	evaluator, err := NewHardAdmission(HardAdmissionConfig{
		Probe: probe, DiskFloorBytes: 1, FDHeadroom: 1, RuntimeSlotCount: 1,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	check := AdmissionCheck{State: StateActive, CertifiedAt: now, CertificationTTL: time.Hour}
	for _, consumer := range []string{"run", "workspace", "build"} {
		check.Consumer = consumer
		evaluator.Evaluate(context.Background(), check)
	}
	observation := evaluator.Observation()
	if observation.RunPausedReason != "" ||
		observation.RuntimePausedReason != "" ||
		observation.BuildPausedReason != string(AdmissionProgramVerifierUnavailable) {
		t.Fatalf(
			"domain pauses = run:%q runtime:%q build:%q",
			observation.RunPausedReason,
			observation.RuntimePausedReason,
			observation.BuildPausedReason,
		)
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
	check := AdmissionCheck{Consumer: "run", State: StateActive, CertifiedAt: now, CertificationTTL: time.Hour}
	if decision := evaluator.Evaluate(context.Background(), check); !decision.Allowed {
		t.Fatalf("healthy datapath decision = %+v", decision)
	}
	datapathHealthy = false
	if decision := evaluator.Evaluate(context.Background(), check); decision.Allowed || decision.Reason != AdmissionDatapathUnverified {
		t.Fatalf("changed datapath decision = %+v", decision)
	}
	observation := evaluator.Observation()
	if observation.RunPausedReason != string(AdmissionDatapathUnverified) ||
		observation.BuildPausedReason != string(AdmissionDatapathUnverified) ||
		observation.RuntimePausedReason != string(AdmissionDatapathUnverified) {
		t.Fatalf("datapath observation = %+v", observation)
	}
}

func TestHardAdmissionKeepsRuntimeSlotPressureOutOfBuildDomain(t *testing.T) {
	now := time.Now()
	probe := &staticHealthProbe{health: healthyHost(now)}
	evaluator, err := NewHardAdmission(HardAdmissionConfig{Probe: probe, DiskFloorBytes: 1, FDHeadroom: 1, RuntimeSlotCount: 1, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	check := AdmissionCheck{State: StateActive, CertifiedAt: now, CertificationTTL: time.Hour, Recovery: RecoveryEvidence{Quarantined: []string{"slot"}}}
	check.Consumer = "run"
	evaluator.Evaluate(context.Background(), check)
	check.Consumer = "runtime"
	evaluator.Evaluate(context.Background(), check)
	check.Consumer = "build"
	evaluator.Evaluate(context.Background(), check)
	observation := evaluator.Observation()
	if observation.RunPausedReason != "" || observation.RuntimePausedReason == "" || observation.BuildPausedReason != "" {
		t.Fatalf("domain pauses = run:%q runtime:%q build:%q", observation.RunPausedReason, observation.RuntimePausedReason, observation.BuildPausedReason)
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
		Consumer: "run", State: StateActive, CertifiedAt: now, CertificationTTL: time.Hour,
		Snapshot: Snapshot{Active: map[string]int{"workspace": 1}},
	})
	if !decision.Allowed {
		t.Fatalf("run inside mounted workspace rejected: %+v", decision)
	}
}

func TestHardAdmissionPressureObservation(t *testing.T) {
	now := time.Now()
	probe := &staticHealthProbe{health: healthyHost(now)}
	evaluator, err := NewHardAdmission(HardAdmissionConfig{
		Probe: probe, DiskFloorBytes: 1, FDHeadroom: 1, RuntimeSlotCount: 1, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	evaluator.Evaluate(context.Background(), AdmissionCheck{State: StateActive, CertifiedAt: now, CertificationTTL: time.Hour})
	observation := evaluator.Observation()
	if observation.GuestEphemeralDiskPressureBPS != 5000 {
		t.Fatalf("disk pressure = %d, want 5000", observation.GuestEphemeralDiskPressureBPS)
	}
	if len(observation.HealthDetails) == 0 {
		t.Fatal("typed hard admission health details are missing")
	}
}

func TestBuildLeaseValidatesFixedGuestIndependentlyFromHostEnvelope(t *testing.T) {
	capabilities := api.WorkerCapabilities{
		VMMilliCPU: 2000, VMMemoryMiB: 2048,
		GuestEphemeralDiskBytes: 32 << 30, VMGuestEphemeralDiskBytes: 32 << 30, MaxBuildExecutors: 1,
	}
	lease := api.WorkerDeploymentBuildLease{
		RequestedBuildExecutors: 1, RequestedCPUMillis: 3000, RequestedMemoryBytes: 4 << 30,
		RequestedGuestEphemeralDiskBytes: 32 << 30,
	}
	if err := validateBuildLeaseShape(capabilities, lease); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*api.WorkerCapabilities, *api.WorkerDeploymentBuildLease)
	}{
		{name: "cpu", mutate: func(c *api.WorkerCapabilities, _ *api.WorkerDeploymentBuildLease) { c.VMMilliCPU-- }},
		{name: "memory", mutate: func(c *api.WorkerCapabilities, _ *api.WorkerDeploymentBuildLease) { c.VMMemoryMiB-- }},
		{name: "guest disk", mutate: func(c *api.WorkerCapabilities, _ *api.WorkerDeploymentBuildLease) { c.VMGuestEphemeralDiskBytes-- }},
		{name: "executors", mutate: func(_ *api.WorkerCapabilities, l *api.WorkerDeploymentBuildLease) { l.RequestedBuildExecutors = 2 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testCapabilities := capabilities
			testLease := lease
			tt.mutate(&testCapabilities, &testLease)
			if err := validateBuildLeaseShape(testCapabilities, testLease); err == nil {
				t.Fatal("unsupported build shape accepted")
			}
		})
	}
}
