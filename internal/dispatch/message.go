package dispatch

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
)

type WorkKind string

const (
	WorkKindRun   WorkKind = "run"
	WorkKindBuild WorkKind = "build"
)

type BuildResourceVector struct {
	CPUMillis          int64
	MemoryBytes        int64
	WorkloadDiskBytes  int64
	ScratchBytes       int64
	BuildCacheBytes    int64
	ArtifactCacheBytes int64
	Executors          int32
}

type Message struct {
	WorkKind              WorkKind
	RunID                 string
	DeploymentID          string
	OrgID                 string
	RegionID              string
	ProjectID             string
	EnvironmentID         string
	QueueClass            string
	QueueName             string
	QueueConcurrencyScope string
	QueueConcurrencyLimit int32
	ConcurrencyKey        string
	RunStateVersion       int64
	Requirements          compute.RunRuntimeRequirements
	Priority              int32
	QueueTimestamp        time.Time
	QueuedExpiresAt       time.Time
	EnqueuedAt            time.Time
	Traceparent           string
	BuildAttemptNumber    int32
	LeaseSequence         int64
	BuildResources        BuildResourceVector
}

func (m Message) WorkID() string {
	if m.WorkKind == WorkKindBuild {
		return m.DeploymentID
	}
	return m.RunID
}

func (m Message) ReadyFence() string {
	if m.WorkKind == WorkKindBuild {
		return fmt.Sprintf("build:%d:%d", m.BuildAttemptNumber, m.LeaseSequence)
	}
	return fmt.Sprintf("run:%d", m.RunStateVersion)
}

func QueueNameForRuntime(base string, runtime compute.RuntimeSelector) string {
	names := QueueNamesForRuntime(base, runtime)
	if len(names) == 0 {
		return strings.TrimSpace(base)
	}
	return names[0]
}

func QueueNamesForRuntime(base string, runtime compute.RuntimeSelector) []string {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil
	}
	parts := runtimeQueueParts(runtime)
	names := make([]string, 0, len(parts)+1)
	for i := len(parts); i > 0; i-- {
		names = append(names, base+":rt:"+strings.Join(parts[:i], ":"))
	}
	names = append(names, base)
	return names
}

func runtimeQueueParts(runtime compute.RuntimeSelector) []string {
	ordered := []string{
		strings.TrimSpace(runtime.ID),
		strings.TrimSpace(runtime.Arch),
		strings.TrimSpace(runtime.ABI),
		strings.TrimSpace(runtime.KernelDigest),
		strings.TrimSpace(runtime.InitramfsDigest),
		strings.TrimSpace(runtime.RootfsDigest),
		strings.TrimSpace(runtime.CNIProfile),
	}
	parts := make([]string, 0, len(ordered))
	for _, part := range ordered {
		if part == "" {
			break
		}
		parts = append(parts, part)
	}
	return parts
}

func (m Message) Validate() error {
	var problems []error
	if m.WorkKind != WorkKindRun && m.WorkKind != WorkKindBuild {
		problems = append(problems, errors.New("work kind must be run or build"))
	}
	if m.WorkKind == WorkKindRun && strings.TrimSpace(m.RunID) == "" {
		problems = append(problems, errors.New("run id is required"))
	}
	if m.WorkKind == WorkKindBuild && strings.TrimSpace(m.DeploymentID) == "" {
		problems = append(problems, errors.New("deployment id is required"))
	}
	if strings.TrimSpace(m.OrgID) == "" {
		problems = append(problems, errors.New("org id is required"))
	}
	if strings.TrimSpace(m.RegionID) == "" {
		problems = append(problems, errors.New("region id is required"))
	}
	if strings.TrimSpace(m.ProjectID) == "" {
		problems = append(problems, errors.New("project id is required"))
	}
	if strings.TrimSpace(m.EnvironmentID) == "" {
		problems = append(problems, errors.New("environment id is required"))
	}
	if strings.TrimSpace(m.QueueClass) == "" {
		problems = append(problems, errors.New("queue class is required"))
	}
	if strings.TrimSpace(m.QueueName) == "" {
		problems = append(problems, errors.New("queue name is required"))
	}
	if m.QueueConcurrencyLimit < 0 {
		problems = append(problems, errors.New("queue concurrency limit must be non-negative"))
	}
	if m.WorkKind == WorkKindRun && m.RunStateVersion <= 0 {
		problems = append(problems, errors.New("run state version must be positive"))
	}
	if m.WorkKind == WorkKindRun {
		if err := m.Requirements.Validate(); err != nil {
			problems = append(problems, err)
		}
	}
	if m.WorkKind == WorkKindBuild && (m.BuildAttemptNumber <= 0 || m.LeaseSequence <= 0 ||
		m.BuildResources.CPUMillis <= 0 || m.BuildResources.MemoryBytes <= 0 || m.BuildResources.Executors <= 0 ||
		m.BuildResources.WorkloadDiskBytes < 0 || m.BuildResources.ScratchBytes < 0 ||
		m.BuildResources.BuildCacheBytes < 0 || m.BuildResources.ArtifactCacheBytes < 0) {
		problems = append(problems, errors.New("build fence and frozen resource vector must be valid"))
	}
	return errors.Join(problems...)
}
