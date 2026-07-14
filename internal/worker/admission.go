package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"golang.org/x/sys/unix"
)

type AdmissionReason string

const (
	AdmissionAllowed                 AdmissionReason = ""
	AdmissionDiskFloor               AdmissionReason = "disk_floor"
	AdmissionFileDescriptorPressure  AdmissionReason = "file_descriptor_pressure"
	AdmissionCgroupUnavailable       AdmissionReason = "cgroup_unavailable"
	AdmissionKVMUnavailable          AdmissionReason = "kvm_unavailable"
	AdmissionFirecrackerUnavailable  AdmissionReason = "firecracker_unavailable"
	AdmissionRuntimeSlotsQuarantined AdmissionReason = "runtime_slots_quarantined"
	AdmissionCertificationStale      AdmissionReason = "certification_stale"
	AdmissionProbeFailed             AdmissionReason = "host_probe_failed"
)

type HostHealth struct {
	ObservedAt          time.Time `json:"observed_at"`
	AvailableDiskBytes  int64     `json:"available_disk_bytes"`
	DiskCapacityBytes   int64     `json:"disk_capacity_bytes"`
	OpenFileDescriptors uint64    `json:"open_file_descriptors"`
	FileDescriptorLimit uint64    `json:"file_descriptor_limit"`
	CgroupHealthy       bool      `json:"cgroup_healthy"`
	KVMHealthy          bool      `json:"kvm_healthy"`
	FirecrackerHealthy  bool      `json:"firecracker_healthy"`
}

type HostHealthProbe interface {
	Probe(context.Context) (HostHealth, error)
}

type AdmissionCheck struct {
	Consumer         string
	State            State
	Snapshot         Snapshot
	Recovery         RecoveryEvidence
	CertifiedAt      time.Time
	CertificationTTL time.Duration
}

type AdmissionDecision struct {
	Allowed bool            `json:"allowed"`
	Reason  AdmissionReason `json:"reason,omitempty"`
	Health  HostHealth      `json:"health"`
}

type AdmissionEvaluator interface {
	Evaluate(context.Context, AdmissionCheck) AdmissionDecision
	Observation() api.WorkerObservation
}

type HardAdmissionConfig struct {
	Probe            HostHealthProbe
	DiskFloorBytes   int64
	FDHeadroom       uint64
	RuntimeSlotCount int32
	Now              func() time.Time
}

type HardAdmission struct {
	cfg  HardAdmissionConfig
	mu   sync.RWMutex
	last map[string]AdmissionDecision
}

func NewHardAdmission(cfg HardAdmissionConfig) (*HardAdmission, error) {
	if cfg.Probe == nil {
		return nil, errors.New("admission host health probe is required")
	}
	if cfg.DiskFloorBytes <= 0 {
		return nil, errors.New("admission disk floor must be positive")
	}
	if cfg.FDHeadroom == 0 {
		return nil, errors.New("admission file descriptor headroom must be positive")
	}
	if cfg.RuntimeSlotCount <= 0 {
		return nil, errors.New("admission runtime slot count must be positive")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &HardAdmission{cfg: cfg, last: map[string]AdmissionDecision{}}, nil
}

func (a *HardAdmission) Evaluate(ctx context.Context, check AdmissionCheck) AdmissionDecision {
	health, err := a.cfg.Probe.Probe(ctx)
	decision := AdmissionDecision{Allowed: false, Health: health}
	switch {
	case err != nil:
		decision.Reason = AdmissionProbeFailed
	case check.State != StateActive:
		decision.Reason = AdmissionReason(check.State)
	case check.CertifiedAt.IsZero(), check.CertificationTTL <= 0,
		a.cfg.Now().After(check.CertifiedAt.Add(check.CertificationTTL)):
		decision.Reason = AdmissionCertificationStale
	case health.AvailableDiskBytes < a.cfg.DiskFloorBytes:
		decision.Reason = AdmissionDiskFloor
	case health.FileDescriptorLimit <= health.OpenFileDescriptors ||
		health.FileDescriptorLimit-health.OpenFileDescriptors < a.cfg.FDHeadroom:
		decision.Reason = AdmissionFileDescriptorPressure
	case !health.CgroupHealthy:
		decision.Reason = AdmissionCgroupUnavailable
	case !health.KVMHealthy:
		decision.Reason = AdmissionKVMUnavailable
	case !health.FirecrackerHealthy:
		decision.Reason = AdmissionFirecrackerUnavailable
	case runtimeSlotConsumer(check.Consumer) &&
		int32(len(check.Recovery.Quarantined)+check.Snapshot.Active["workspace"]) >= a.cfg.RuntimeSlotCount:
		decision.Reason = AdmissionRuntimeSlotsQuarantined
	default:
		decision.Allowed = true
	}
	a.mu.Lock()
	a.last[check.Consumer] = decision
	a.mu.Unlock()
	return decision
}

func runtimeSlotConsumer(consumer string) bool {
	return consumer == "workspace" || consumer == "runtime"
}

func (a *HardAdmission) Observation() api.WorkerObservation {
	a.mu.RLock()
	decisions := make(map[string]AdmissionDecision, len(a.last))
	maps.Copy(decisions, a.last)
	a.mu.RUnlock()
	observation := api.WorkerObservation{}
	decision := decisions["run"]
	if decision.Health.DiskCapacityBytes == 0 {
		decision = decisions["runtime"]
	}
	if decision.Health.DiskCapacityBytes == 0 {
		decision = decisions["build"]
	}
	if decision.Health.DiskCapacityBytes == 0 {
		for _, current := range decisions {
			decision = current
			break
		}
	}
	if decision.Health.DiskCapacityBytes > 0 {
		used := decision.Health.DiskCapacityBytes - decision.Health.AvailableDiskBytes
		used = min(max(used, 0), decision.Health.DiskCapacityBytes)
		observation.WorkloadDiskPressureBPS = int32(used * 10_000 / decision.Health.DiskCapacityBytes)
		observation.ScratchPressureBPS = observation.WorkloadDiskPressureBPS
	}
	details, _ := json.Marshal(decisions)
	observation.HealthDetails = details
	for domain, current := range decisions {
		if current.Allowed || current.Reason == "" {
			continue
		}
		reason := string(current.Reason)
		if current.Reason != AdmissionRuntimeSlotsQuarantined {
			observation.RunPausedReason, observation.BuildPausedReason, observation.RuntimePausedReason = reason, reason, reason
			break
		}
		if domain == "run" {
			observation.RunPausedReason = reason
		}
		if domain == "runtime" {
			observation.RuntimePausedReason = reason
		}
	}
	return observation
}

type SystemHostHealthProbe struct {
	WorkDir         string
	CgroupVersion   string
	FirecrackerPath string
	Now             func() time.Time
}

func (p SystemHostHealthProbe) Probe(context.Context) (HostHealth, error) {
	if p.Now == nil {
		p.Now = time.Now
	}
	health := HostHealth{ObservedAt: p.Now().UTC()}
	if err := os.MkdirAll(p.WorkDir, 0o755); err != nil {
		return health, fmt.Errorf("prepare work directory: %w", err)
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(p.WorkDir, &stat); err != nil {
		return health, fmt.Errorf("inspect worker filesystem: %w", err)
	}
	health.AvailableDiskBytes = int64(stat.Bavail) * int64(stat.Bsize)
	health.DiskCapacityBytes = int64(stat.Blocks) * int64(stat.Bsize)
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return health, fmt.Errorf("inspect open file descriptors: %w", err)
	}
	health.OpenFileDescriptors = uint64(len(entries))
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		return health, fmt.Errorf("inspect file descriptor limit: %w", err)
	}
	health.FileDescriptorLimit = limit.Cur
	cgroupPath := "/sys/fs/cgroup/cgroup.controllers"
	if p.CgroupVersion != "2" {
		cgroupPath = "/sys/fs/cgroup"
	}
	if info, statErr := os.Stat(cgroupPath); statErr == nil {
		health.CgroupHealthy = p.CgroupVersion == "2" || info.IsDir()
	}
	if file, openErr := os.OpenFile("/dev/kvm", os.O_RDWR, 0); openErr == nil {
		health.KVMHealthy = true
		_ = file.Close()
	}
	path, lookupErr := exec.LookPath(p.FirecrackerPath)
	if lookupErr == nil {
		if info, statErr := os.Stat(filepath.Clean(path)); statErr == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			health.FirecrackerHealthy = true
		}
	}
	return health, nil
}
