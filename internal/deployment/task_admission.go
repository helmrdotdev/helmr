package deployment

import (
	"encoding/json"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type TaskRunAdmission struct {
	HasPayload            bool
	QueueName             string
	QueueConcurrencyLimit *int64
	MaxActiveDurationMS   int64
	QueuedTTLMS           *int64
	RetryPolicy           []byte
}

func ResolveTaskRunAdmission(
	manifestVersion int32,
	declaredID string,
	rawManifest []byte,
	expectedManifestDigest []byte,
	rawQueueConfig []byte,
	queueOverride string,
	queuedTTLOverride *int64,
	retryOverride []byte,
) (TaskRunAdmission, error) {
	manifest, err := ParseTaskManifest(
		manifestVersion,
		rawManifest,
		expectedManifestDigest,
	)
	if err != nil {
		return TaskRunAdmission{}, err
	}
	queueConfig, err := parseQueueConfig(rawQueueConfig)
	if err != nil {
		return TaskRunAdmission{}, err
	}
	validate := func(candidate TaskManifest) error {
		return ValidateBuildPlan(BuildPlan{
			FormatVersion: BuildPlanFormatVersion,
			Definitions: []DefinitionInput{{
				Kind: DefinitionKindTask, DeclaredID: declaredID, Task: &candidate,
			}},
			Queues: queueConfig.Queues,
		})
	}
	if err := validate(manifest); err != nil {
		return TaskRunAdmission{}, fmt.Errorf("validate task admission authority: %w", err)
	}
	if queueOverride != "" {
		manifest.Run.Queue = queueOverride
	}
	if queuedTTLOverride != nil {
		value := *queuedTTLOverride
		manifest.Run.TTLMs = &value
	}
	if len(retryOverride) > 0 {
		retry, err := ParseRetryManifest(retryOverride)
		if err != nil {
			return TaskRunAdmission{}, fmt.Errorf("parse task retry override: %w", err)
		}
		manifest.Run.Retry = retry
	}
	if err := validate(manifest); err != nil {
		return TaskRunAdmission{}, fmt.Errorf("validate task admission selection: %w", err)
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
		return TaskRunAdmission{}, fmt.Errorf("encode task retry authority: %w", err)
	}
	retryPolicy, err = jsoncanon.Transform(retryPolicy)
	if err != nil {
		return TaskRunAdmission{}, fmt.Errorf("canonicalize task retry authority: %w", err)
	}
	var queuedTTL *int64
	if manifest.Run.TTLMs != nil {
		value := *manifest.Run.TTLMs
		queuedTTL = &value
	}
	return TaskRunAdmission{
		HasPayload: manifest.Payload.Kind == SchemaKindStandard,
		QueueName:  manifest.Run.Queue, QueueConcurrencyLimit: queueLimit,
		MaxActiveDurationMS: manifest.Run.MaxDurationMs,
		QueuedTTLMS:         queuedTTL, RetryPolicy: retryPolicy,
	}, nil
}
