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
	CPUMillis         int64
	MemoryBytes       int64
	WorkloadDiskBytes int64
	ScratchBytes      int64
	Executors         int32
}

const (
	buildArchitectureX8664 = "x86_64"
)

type Message struct {
	WorkKind          WorkKind
	RunID             string
	DeploymentID      string
	OrgID             string
	RegionID          string
	ProjectID         string
	EnvironmentID     string
	QueueName         string
	ConcurrencyKey    string
	RunStateVersion   int64
	Priority          int32
	QueueOriginAt     time.Time
	QueueScoreAt      time.Time
	EnqueuedAt        time.Time
	LeaseSequence     int64
	BuildArchitecture string
	BuildResources    BuildResourceVector
}

func (m Message) WorkID() string {
	if m.WorkKind == WorkKindBuild {
		return m.DeploymentID
	}
	return m.RunID
}

func (m Message) ReadyFence() string {
	if m.WorkKind == WorkKindBuild {
		return fmt.Sprintf("build:%d", m.LeaseSequence)
	}
	return fmt.Sprintf("run:%d", m.RunStateVersion)
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
	if strings.TrimSpace(m.QueueName) == "" {
		problems = append(problems, errors.New("queue name is required"))
	}
	if m.WorkKind == WorkKindRun && m.RunStateVersion <= 0 {
		problems = append(problems, errors.New("run state version must be positive"))
	}
	if m.QueueOriginAt.IsZero() || m.QueueScoreAt.IsZero() {
		problems = append(problems, errors.New("queue origin and score are required"))
	}
	if m.WorkKind == WorkKindBuild {
		envelope := compute.BuildEnvelopeResources()
		if !validBuildArchitecture(m.BuildArchitecture) {
			problems = append(problems, errors.New("build architecture must be x86_64"))
		}
		if m.LeaseSequence < 1 || m.LeaseSequence > 3 ||
			m.BuildResources.CPUMillis != envelope.MilliCPU ||
			m.BuildResources.MemoryBytes != envelope.MemoryMiB<<20 ||
			m.BuildResources.WorkloadDiskBytes != 0 ||
			m.BuildResources.ScratchBytes != envelope.DiskMiB<<20 ||
			m.BuildResources.Executors != 1 {
			problems = append(problems, errors.New("build fence and fixed resource vector must be valid"))
		}
	}
	return errors.Join(problems...)
}

func validBuildArchitecture(architecture string) bool {
	return architecture == buildArchitectureX8664
}
