package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type ActorRunAdmission struct {
	QueueName             string
	QueueConcurrencyLimit *int64
	MaxActiveDurationMS   int64
	QueuedTTLMS           *int64
	RetryPolicy           []byte
}

func ResolveActorRunAdmission(
	manifestVersion int32,
	declaredID string,
	rawManifest []byte,
	expectedManifestDigest []byte,
	rawQueueConfig []byte,
	queueOverride string,
) (ActorRunAdmission, error) {
	if manifestVersion != BuildPlanFormatVersion {
		return ActorRunAdmission{}, fmt.Errorf(
			"actor manifest version = %d, want %d",
			manifestVersion,
			BuildPlanFormatVersion,
		)
	}
	canonical, digest, err := CanonicalManifestAndDigest(rawManifest)
	if err != nil {
		return ActorRunAdmission{}, err
	}
	if !bytes.Equal(digest[:], expectedManifestDigest) {
		return ActorRunAdmission{}, errors.New("actor manifest digest does not match its authority")
	}
	var manifest ActorManifest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return ActorRunAdmission{}, fmt.Errorf("decode actor manifest: %w", err)
	}
	if err := ensureEOF(decoder, "actor manifest"); err != nil {
		return ActorRunAdmission{}, err
	}
	completeRaw, err := json.Marshal(manifest)
	if err != nil {
		return ActorRunAdmission{}, fmt.Errorf("encode complete actor manifest: %w", err)
	}
	complete, err := jsoncanon.Transform(completeRaw)
	if err != nil {
		return ActorRunAdmission{}, fmt.Errorf("canonicalize complete actor manifest: %w", err)
	}
	if !bytes.Equal(canonical, complete) {
		return ActorRunAdmission{}, errors.New("actor manifest does not match the complete canonical v0 shape")
	}
	queueConfig, err := parseQueueConfig(rawQueueConfig)
	if err != nil {
		return ActorRunAdmission{}, err
	}
	validate := func(candidate ActorManifest) error {
		return ValidateBuildPlan(BuildPlan{
			FormatVersion: BuildPlanFormatVersion,
			Definitions: []DefinitionInput{{
				Kind: DefinitionKindActor, DeclaredID: declaredID, Actor: &candidate,
			}},
			Queues: queueConfig.Queues,
		})
	}
	if err := validate(manifest); err != nil {
		return ActorRunAdmission{}, fmt.Errorf("validate actor admission authority: %w", err)
	}
	if queueOverride != "" {
		manifest.Run.Queue = queueOverride
	}
	if err := validate(manifest); err != nil {
		return ActorRunAdmission{}, fmt.Errorf("validate actor admission selection: %w", err)
	}
	var queueLimit *int64
	for _, queue := range queueConfig.Queues {
		if queue.Name != manifest.Run.Queue {
			continue
		}
		if queue.ConcurrencyLimit != nil {
			value := *queue.ConcurrencyLimit
			queueLimit = &value
		}
		break
	}
	retryPolicy, err := json.Marshal(manifest.Run.Retry)
	if err != nil {
		return ActorRunAdmission{}, fmt.Errorf("encode actor retry authority: %w", err)
	}
	retryPolicy, err = jsoncanon.Transform(retryPolicy)
	if err != nil {
		return ActorRunAdmission{}, fmt.Errorf("canonicalize actor retry authority: %w", err)
	}
	var queuedTTL *int64
	if manifest.Run.TTLMs != nil {
		value := *manifest.Run.TTLMs
		queuedTTL = &value
	}
	return ActorRunAdmission{
		QueueName: manifest.Run.Queue, QueueConcurrencyLimit: queueLimit,
		MaxActiveDurationMS: manifest.Run.MaxDurationMs,
		QueuedTTLMS:         queuedTTL, RetryPolicy: retryPolicy,
	}, nil
}
