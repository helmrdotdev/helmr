package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/schedule"
)

type ScheduleAuthority struct{}

func NewScheduleAuthority() *ScheduleAuthority {
	return &ScheduleAuthority{}
}

func (a *ScheduleAuthority) ValidateScheduledTask(
	manifestVersion int32,
	declaredID string,
	raw []byte,
	expectedDigest []byte,
	queueConfigRaw []byte,
) error {
	_, err := a.ResolveScheduledTask(
		manifestVersion,
		declaredID,
		raw,
		expectedDigest,
		queueConfigRaw,
	)
	return err
}

func (a *ScheduleAuthority) ResolveScheduledTask(
	manifestVersion int32,
	declaredID string,
	raw []byte,
	expectedDigest []byte,
	queueConfigRaw []byte,
) (schedule.TaskRun, error) {
	manifest, err := ParseTaskManifest(manifestVersion, raw, expectedDigest)
	if err != nil {
		return schedule.TaskRun{}, err
	}

	queueConfig, err := parseQueueConfig(queueConfigRaw)
	if err != nil {
		return schedule.TaskRun{}, err
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
		return schedule.TaskRun{}, fmt.Errorf("validate scheduled task manifest: %w", err)
	}
	if manifest.Payload.Kind != SchemaKindStandard {
		return schedule.TaskRun{}, errors.New("scheduled task payload kind must be standard_schema")
	}
	if manifest.Schedule == nil {
		return schedule.TaskRun{}, errors.New("scheduled task manifest has no Schedule")
	}
	var queueLimit *int64
	for _, queue := range queueConfig.Queues {
		if queue.Name == manifest.Run.Queue {
			if queue.ConcurrencyLimit != nil {
				value := *queue.ConcurrencyLimit
				queueLimit = &value
			}
			break
		}
	}
	retryPolicy, err := json.Marshal(manifest.Run.Retry)
	if err != nil {
		return schedule.TaskRun{}, fmt.Errorf("encode scheduled Task retry authority: %w", err)
	}
	retryPolicy, err = jsoncanon.Transform(retryPolicy)
	if err != nil {
		return schedule.TaskRun{}, fmt.Errorf("canonicalize scheduled Task retry authority: %w", err)
	}
	var queuedTTL *int64
	if manifest.Run.TTLMs != nil {
		value := *manifest.Run.TTLMs
		queuedTTL = &value
	}
	return schedule.TaskRun{
		QueueName:             manifest.Run.Queue,
		QueueConcurrencyLimit: queueLimit,
		QueuedTTLMS:           queuedTTL,
		MaxActiveDurationMS:   manifest.Run.MaxDurationMs,
		RetryPolicy:           retryPolicy,
	}, nil
}

func ParseTaskManifest(
	manifestVersion int32,
	raw []byte,
	expectedDigest []byte,
) (TaskManifest, error) {
	if manifestVersion != BuildPlanFormatVersion {
		return TaskManifest{}, fmt.Errorf(
			"task manifest version = %d, want %d",
			manifestVersion,
			BuildPlanFormatVersion,
		)
	}
	canonical, digest, err := CanonicalManifestAndDigest(raw)
	if err != nil {
		return TaskManifest{}, err
	}
	if !bytes.Equal(digest[:], expectedDigest) {
		return TaskManifest{}, errors.New("task manifest digest does not match its authority")
	}

	var manifest TaskManifest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return TaskManifest{}, fmt.Errorf("decode task manifest: %w", err)
	}
	if err := ensureEOF(decoder, "task manifest"); err != nil {
		return TaskManifest{}, err
	}
	completeRaw, err := json.Marshal(manifest)
	if err != nil {
		return TaskManifest{}, fmt.Errorf("encode complete task manifest: %w", err)
	}
	complete, err := jsoncanon.Transform(completeRaw)
	if err != nil {
		return TaskManifest{}, fmt.Errorf("canonicalize complete task manifest: %w", err)
	}
	if !bytes.Equal(canonical, complete) {
		return TaskManifest{}, errors.New("task manifest does not match the complete canonical v0 shape")
	}
	if manifest.Payload.Kind != SchemaKindNone && manifest.Payload.Kind != SchemaKindStandard {
		return TaskManifest{}, fmt.Errorf("task payload kind %q is unsupported", manifest.Payload.Kind)
	}
	return manifest, nil
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
