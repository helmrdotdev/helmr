package workergroup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/helmrdotdev/helmr/internal/enrollment"
)

type Config struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	Description         string   `json:"description,omitempty"`
	EnrollmentSecretEnv string   `json:"enrollment_secret_env"`
	AllowsRun           bool     `json:"allows_run"`
	AllowsBuild         bool     `json:"allows_build"`
	ObservationTTL      int32    `json:"observation_ttl_seconds"`
	InstanceCapacity    Capacity `json:"instance_capacity"`
}

func DecodeConfig(raw string) ([]Config, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var groups []Config
	if err := decoder.Decode(&groups); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("worker groups must contain one JSON value")
	}
	if len(groups) == 0 {
		return nil, errors.New("at least one worker group is required")
	}
	return groups, nil
}

func (group Config) Prepare(lookupEnv func(string) (string, bool)) (Desired, enrollment.GroupSecret, error) {
	spec, err := Normalize(Spec{
		ID: group.ID, Name: group.Name, Description: group.Description,
		AllowsRun: group.AllowsRun, AllowsBuild: group.AllowsBuild,
	})
	if err != nil {
		return Desired{}, enrollment.GroupSecret{}, err
	}
	if err := group.InstanceCapacity.Validate(spec); err != nil {
		return Desired{}, enrollment.GroupSecret{}, err
	}
	if group.ObservationTTL <= 0 {
		return Desired{}, enrollment.GroupSecret{}, errors.New("observation TTL must be positive")
	}
	if !validEnrollmentSecretEnv(group.EnrollmentSecretEnv) {
		return Desired{}, enrollment.GroupSecret{}, errors.New("enrollment_secret_env must be a HELMR_WORKER_ENROLLMENT_SECRET_* environment variable")
	}
	secret, ok := lookupEnv(group.EnrollmentSecretEnv)
	if !ok {
		return Desired{}, enrollment.GroupSecret{}, fmt.Errorf("%s is required", group.EnrollmentSecretEnv)
	}
	return Desired{
		Spec: spec, Capacity: group.InstanceCapacity, ObservationTTLSeconds: group.ObservationTTL,
	}, enrollment.GroupSecret{GroupID: spec.ID, Secret: secret}, nil
}

func validEnrollmentSecretEnv(name string) bool {
	const prefix = "HELMR_WORKER_ENROLLMENT_SECRET_"
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
		return false
	}
	for _, char := range name[len(prefix):] {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}
