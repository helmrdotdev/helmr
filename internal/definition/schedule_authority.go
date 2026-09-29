package definition

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

type ScheduleAuthority struct{}

func NewScheduleAuthority() *ScheduleAuthority {
	return &ScheduleAuthority{}
}

func (a *ScheduleAuthority) ResolveScheduledTask(
	manifestVersion int32,
	declaredID string,
	raw []byte,
	expectedDigest []byte,
	queueConfigRaw []byte,
) (ScheduledTaskAdmission, error) {
	manifest, err := ParseTaskManifest(manifestVersion, raw, expectedDigest)
	if err != nil {
		return ScheduledTaskAdmission{}, err
	}

	queueConfig, err := parseQueueConfig(queueConfigRaw)
	if err != nil {
		return ScheduledTaskAdmission{}, err
	}
	if err := ValidateBuildPlan(BuildPlan{
		FormatVersion: BuildPlanFormatVersion,
		Definitions: []Input{{
			Kind:       KindTask,
			DeclaredID: declaredID,
			Task:       &manifest,
		}},
		Queues: queueConfig.Queues,
	}); err != nil {
		return ScheduledTaskAdmission{}, fmt.Errorf("validate scheduled task manifest: %w", err)
	}
	if manifest.Payload.Kind != SchemaKindStandard {
		return ScheduledTaskAdmission{}, errors.New("scheduled task payload kind must be standard_schema")
	}
	if manifest.Schedule == nil {
		return ScheduledTaskAdmission{}, errors.New("scheduled task manifest has no schedule")
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
		return ScheduledTaskAdmission{}, fmt.Errorf("encode scheduled task retry authority: %w", err)
	}
	retryPolicy, err = jsoncanon.Transform(retryPolicy)
	if err != nil {
		return ScheduledTaskAdmission{}, fmt.Errorf("canonicalize scheduled task retry authority: %w", err)
	}
	var queuedTTL *int64
	if manifest.Run.TTLMs != nil {
		value := *manifest.Run.TTLMs
		queuedTTL = &value
	}
	secretPlacements, err := secretbinding.Normalize(secretbinding.Placements(manifest.Schedule.Computer.Secrets))
	if err != nil {
		return ScheduledTaskAdmission{}, fmt.Errorf("normalize scheduled Computer Secrets: %w", err)
	}
	return ScheduledTaskAdmission{
		QueueName:             manifest.Run.Queue,
		QueueConcurrencyLimit: queueLimit,
		QueuedTTLMS:           queuedTTL,
		MaxActiveDurationMS:   manifest.Run.MaxDurationMs,
		RetryPolicy:           retryPolicy,
		SandboxDeclaredID:     manifest.Schedule.Computer.SandboxDeclaredID,
		SecretPlacements:      secretPlacements,
	}, nil
}

func ParseTaskManifest(
	manifestVersion int32,
	raw []byte,
	expectedDigest []byte,
) (TaskManifest, error) {
	if manifestVersion != DeploymentPlanFormatVersion {
		return TaskManifest{}, fmt.Errorf(
			"task manifest version = %d, want %d",
			manifestVersion,
			DeploymentPlanFormatVersion,
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
