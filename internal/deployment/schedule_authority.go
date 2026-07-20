package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type ScheduleAuthority struct {
	policy *BuildPolicy
}

func NewScheduleAuthority(policy *BuildPolicy) (*ScheduleAuthority, error) {
	if policy == nil {
		return nil, errors.New("deployment build policy is required")
	}
	return &ScheduleAuthority{policy: policy}, nil
}

func (a *ScheduleAuthority) ResolveRuntime(digest string) error {
	if a == nil || a.policy == nil {
		return errors.New("deployment build policy is required")
	}
	_, err := a.policy.ResolveRuntime(digest)
	return err
}

func (a *ScheduleAuthority) ValidateScheduledTask(
	manifestVersion int32,
	declaredID string,
	raw []byte,
	expectedDigest []byte,
	queueConfigRaw []byte,
) error {
	if manifestVersion != BuildPlanFormatVersion {
		return fmt.Errorf(
			"task manifest version = %d, want %d",
			manifestVersion,
			BuildPlanFormatVersion,
		)
	}
	canonical, digest, err := CanonicalManifestAndDigest(raw)
	if err != nil {
		return err
	}
	if !bytes.Equal(digest[:], expectedDigest) {
		return errors.New("task manifest digest does not match its authority")
	}

	var manifest TaskManifest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fmt.Errorf("decode task manifest: %w", err)
	}
	if err := ensureEOF(decoder, "task manifest"); err != nil {
		return err
	}
	completeRaw, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode complete task manifest: %w", err)
	}
	complete, err := jsoncanon.Transform(completeRaw)
	if err != nil {
		return fmt.Errorf("canonicalize complete task manifest: %w", err)
	}
	if !bytes.Equal(canonical, complete) {
		return errors.New("task manifest does not match the complete canonical v0 shape")
	}

	queueConfig, err := parseQueueConfig(queueConfigRaw)
	if err != nil {
		return err
	}
	if err := ValidateBuildPlan(BuildPlan{
		FormatVersion: BuildPlanFormatVersion,
		Definitions: []DefinitionInput{{
			Kind:       DefinitionKindTask,
			DeclaredID: declaredID,
			Task:       &manifest,
		}},
		Queues: queueConfig.Queues,
	}); err != nil {
		return fmt.Errorf("validate scheduled task manifest: %w", err)
	}
	if manifest.Payload.Kind != SchemaKindStandard {
		return errors.New("scheduled task payload kind must be standard_schema")
	}
	return nil
}

func parseQueueConfig(raw []byte) (QueueConfig, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return QueueConfig{}, fmt.Errorf("canonicalize queue config: %w", err)
	}
	var config QueueConfig
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return QueueConfig{}, fmt.Errorf("decode queue config: %w", err)
	}
	if err := ensureEOF(decoder, "queue config"); err != nil {
		return QueueConfig{}, err
	}
	complete, err := CanonicalQueueConfig(config)
	if err != nil {
		return QueueConfig{}, err
	}
	if !bytes.Equal(canonical, complete) {
		return QueueConfig{}, errors.New("queue config does not match the complete canonical v0 shape")
	}
	return config, nil
}
