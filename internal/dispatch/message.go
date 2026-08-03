package dispatch

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type WorkKind string

const (
	WorkKindRun   WorkKind = "run"
	WorkKindBuild WorkKind = "build"
)

type Message struct {
	WorkKind        WorkKind
	RunID           string
	DeploymentID    string
	OrgID           string
	RegionID        string
	ProjectID       string
	EnvironmentID   string
	QueueName       string
	ConcurrencyKey  string
	RunStateVersion int64
	Priority        int32
	QueueOriginAt   time.Time
	QueueScoreAt    time.Time
	EnqueuedAt      time.Time
	LeaseSequence   int64
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
		if m.LeaseSequence < 1 || m.LeaseSequence > 3 {
			problems = append(problems, errors.New("build lease sequence must be within [1,3]"))
		}
	}
	return errors.Join(problems...)
}
