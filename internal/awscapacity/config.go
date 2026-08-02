package awscapacity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

type Config struct {
	Groups            []GroupConfig `json:"groups"`
	ObservationMaxAge time.Duration `json:"-"`
}

type GroupConfig struct {
	WorkerGroupID                string                     `json:"worker_group_id"`
	AutoScalingGroupName         string                     `json:"autoscaling_group_name"`
	TerminationLifecycleHookName string                     `json:"termination_lifecycle_hook_name"`
	AllowsRun                    bool                       `json:"allows_run"`
	AllowsBuild                  bool                       `json:"allows_build"`
	InstanceCapacity             api.OperatorResourceVector `json:"instance_capacity"`
}

func DecodeConfig(raw string, observationMaxAge time.Duration) (Config, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var groups []GroupConfig
	if err := decoder.Decode(&groups); err != nil {
		return Config{}, fmt.Errorf("decode capacity groups: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("capacity groups must contain one JSON value")
	}
	config := Config{Groups: groups, ObservationMaxAge: observationMaxAge}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) Validate() error {
	if config.ObservationMaxAge <= 0 {
		return errors.New("capacity observation max age must be positive")
	}
	if len(config.Groups) == 0 {
		return errors.New("at least one capacity group is required")
	}
	groupIDs := map[string]struct{}{}
	autoScalingGroups := map[string]struct{}{}
	for index, group := range config.Groups {
		if group.WorkerGroupID == "" || group.WorkerGroupID != strings.TrimSpace(group.WorkerGroupID) ||
			group.AutoScalingGroupName == "" || group.AutoScalingGroupName != strings.TrimSpace(group.AutoScalingGroupName) ||
			group.TerminationLifecycleHookName == "" || group.TerminationLifecycleHookName != strings.TrimSpace(group.TerminationLifecycleHookName) {
			return fmt.Errorf("capacity group %d requires worker_group_id, autoscaling_group_name, and termination_lifecycle_hook_name", index)
		}
		if _, duplicate := groupIDs[group.WorkerGroupID]; duplicate {
			return fmt.Errorf("worker group %q is duplicated", group.WorkerGroupID)
		}
		if _, duplicate := autoScalingGroups[group.AutoScalingGroupName]; duplicate {
			return fmt.Errorf("Auto Scaling group %q is duplicated", group.AutoScalingGroupName)
		}
		groupIDs[group.WorkerGroupID] = struct{}{}
		autoScalingGroups[group.AutoScalingGroupName] = struct{}{}
		if !group.AllowsRun && !group.AllowsBuild {
			return fmt.Errorf("capacity group %q must own at least one v0 role", group.WorkerGroupID)
		}
		if err := validateInstanceCapacity(group); err != nil {
			return err
		}
	}
	return nil
}

func validateInstanceCapacity(group GroupConfig) error {
	capacity := group.InstanceCapacity
	if capacity.CPUMillis <= 0 || capacity.MemoryBytes <= 0 || capacity.GuestEphemeralDiskBytes <= 0 {
		return fmt.Errorf("capacity group %q requires positive CPU, memory, and guest disk capacity", group.WorkerGroupID)
	}
	if group.AllowsRun && (capacity.VMSlots <= 0 || capacity.RunConsumers <= 0) {
		return fmt.Errorf("Run capacity group %q requires positive vm_slots and run_consumers", group.WorkerGroupID)
	}
	if group.AllowsBuild && capacity.BuildExecutors <= 0 {
		return fmt.Errorf("Build capacity group %q requires positive build_executors", group.WorkerGroupID)
	}
	return nil
}
