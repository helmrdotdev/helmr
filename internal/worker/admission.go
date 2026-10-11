package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sys/unix"
)

type AdmissionReason string

const (
	AdmissionAllowed                  AdmissionReason = ""
	AdmissionDiskFloor                AdmissionReason = "disk_floor"
	AdmissionFileDescriptorPressure   AdmissionReason = "file_descriptor_pressure"
	AdmissionCgroupUnavailable        AdmissionReason = "cgroup_unavailable"
	AdmissionKVMUnavailable           AdmissionReason = "kvm_unavailable"
	AdmissionFirecrackerUnavailable   AdmissionReason = "firecracker_unavailable"
	AdmissionProbeFailed              AdmissionReason = "host_probe_failed"
	AdmissionDatapathUnverified       AdmissionReason = "datapath_unverified"
	AdmissionCheckpointKeyUnavailable AdmissionReason = "checkpoint_key_unavailable"
)

type HostHealth struct {
	ObservedAt          time.Time `json:"observed_at"`
	AvailableDiskBytes  int64     `json:"available_disk_bytes"`
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
	Consumer          string
	Status            Status
	Snapshot          Snapshot
	Recovery          RecoveryEvidence
	DrainContinuation bool
}

type AdmissionDecision struct {
	Allowed bool            `json:"allowed"`
	Reason  AdmissionReason `json:"reason,omitempty"`
	Health  HostHealth      `json:"health"`
}

type AdmissionEvaluator interface {
	Evaluate(context.Context, AdmissionCheck) AdmissionDecision
	Observation() workerapi.Observation
}

type HardAdmissionConfig struct {
	Probe          HostHealthProbe
	DiskFloorBytes int64
	FDHeadroom     uint64
	DatapathHealth func() error
	Now            func() time.Time
}

type HardAdmission struct {
	cfg                      HardAdmissionConfig
	mu                       sync.RWMutex
	last                     AdmissionDecision
	checkpointKeyUnavailable bool
}

// The configured key is immutable for this worker's lifetime. A health probe
// cannot repair a mismatch. Reinitialization clears this local latch but does
// not prove compatibility with retained checkpoints.
// Only new physical starts consult this gate. Cleanup and running work continue.
func (a *HardAdmission) PauseForCheckpointKeyMismatch() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.checkpointKeyUnavailable = true
	a.last.Allowed = false
	a.last.Reason = AdmissionCheckpointKeyUnavailable
}

func (a *HardAdmission) CheckpointKeyUnavailable() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.checkpointKeyUnavailable
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
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &HardAdmission{cfg: cfg}, nil
}

func (a *HardAdmission) Evaluate(ctx context.Context, check AdmissionCheck) AdmissionDecision {
	health, err := a.cfg.Probe.Probe(ctx)
	var datapathErr error
	if a.cfg.DatapathHealth != nil {
		datapathErr = a.cfg.DatapathHealth()
	}
	decision := AdmissionDecision{Allowed: false, Health: health}
	switch {
	case err != nil:
		decision.Reason = AdmissionProbeFailed
	case datapathErr != nil:
		decision.Reason = AdmissionDatapathUnverified
	case check.Status != StatusActive && !(check.Status == StatusDraining && check.DrainContinuation):
		decision.Reason = AdmissionReason(check.Status)
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
	default:
		decision.Allowed = true
	}
	a.mu.Lock()
	if a.checkpointKeyUnavailable {
		decision.Allowed = false
		decision.Reason = AdmissionCheckpointKeyUnavailable
	}
	a.last = decision
	a.mu.Unlock()
	return decision
}

func (a *HardAdmission) Observation() workerapi.Observation {
	a.mu.RLock()
	decision := a.last
	a.mu.RUnlock()
	observation := workerapi.Observation{}
	var datapathErr error
	if a.cfg.DatapathHealth != nil {
		datapathErr = a.cfg.DatapathHealth()
	}
	if datapathErr != nil {
		reason := string(AdmissionDatapathUnverified)
		observation.RunPausedReason = reason
		observation.VMPausedReason = reason
		return observation
	}
	if !decision.Allowed && decision.Reason != "" {
		observation.RunPausedReason, observation.VMPausedReason = string(decision.Reason), string(decision.Reason)
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
	if p.CgroupVersion == "2" {
		if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
			health.CgroupHealthy = true
		}
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

// ConsumerAdmission keeps claim admission separate from the physical effects of
// an already delivered allocation. Discovery and retained cleanup need neither.
type ConsumerAdmission interface {
	AdmitClaim(context.Context) error
	AdmitAllocatedStart(context.Context) error
}

var errClaimAdmissionPaused = errors.New("worker claim admission paused")

type consumerAdmission struct {
	supervisor *Supervisor
	spec       ConsumerSpec
	recovery   RecoveryEvidence
}

func (a consumerAdmission) AdmitClaim(ctx context.Context) error {
	s := a.supervisor
	state := s.state.Load().(Status)
	if s.cfg.AdmissionEvaluator == nil || (a.spec.BypassAdmissionDuringDrain && state == StatusDraining) {
		return nil
	}
	decision := s.cfg.AdmissionEvaluator.Evaluate(ctx, AdmissionCheck{
		Consumer: a.spec.Name, Status: state, Snapshot: s.registry.snapshot(),
		Recovery: a.recovery, DrainContinuation: a.spec.ContinueDuringDrain && state == StatusDraining,
	})
	if !decision.Allowed {
		return errClaimAdmissionPaused
	}
	return nil
}

// The physical owner invokes this only after validating the exact live delivery.
// Host drain preserves that admitted start; every hard health fence still applies.
func (a consumerAdmission) AdmitAllocatedStart(ctx context.Context) error {
	return a.supervisor.admitInstanceStart(ctx, true)
}
