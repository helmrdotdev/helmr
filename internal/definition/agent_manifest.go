package definition

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/conversation"
	"regexp"

	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

type AgentManifest struct {
	ComputerDefinitionID string                 `json:"computerDefinitionId"`
	Setup                bool                   `json:"setup"`
	CloseAfterIdleMs     *int64                 `json:"closeAfterIdleMs,omitempty"`
	MaxTurnDurationMs    *int64                 `json:"maxTurnDurationMs,omitempty"`
	Triggers             map[string]CronTrigger `json:"triggers"`
}
type CronTrigger struct {
	Slack    json.RawMessage `json:"slack,omitempty"`
	Cron     string          `json:"cron"`
	Timezone string          `json:"timezone"`
	Input    json.RawMessage `json:"input"`
}
type SecretBinding = secretbinding.Reference
type SecretBindingEnv = secretbinding.ReferenceEnv
type ComputerRefresh struct {
	EveryMs  int64  `json:"everyMs"`
	MaxAgeMs *int64 `json:"maxAgeMs,omitempty"`
}

type ComputerInputManifest struct {
	ImageBuild   ImageBuild        `json:"imageBuild"`
	Resources    ResourcesManifest `json:"resources"`
	Prepare      bool              `json:"prepare"`
	Refresh      *ComputerRefresh  `json:"refresh,omitempty"`
	Secrets      []SecretBinding   `json:"secrets"`
	BuildSecrets []SecretBinding   `json:"buildSecrets"`
}

func ValidateAgentManifest(m AgentManifest) error {
	if !ValidDeclaredID(m.ComputerDefinitionID) {
		return errors.New("agent computerDefinitionId is invalid")
	}
	for _, duration := range []*int64{m.CloseAfterIdleMs, m.MaxTurnDurationMs} {
		if duration != nil && !positiveSafeInteger(*duration) {
			return errors.New("agent duration must be positive safe integer milliseconds")
		}
	}
	if m.Triggers == nil {
		return errors.New("agent triggers must be an object")
	}
	for id, trigger := range m.Triggers {
		if _, err := SlackChannelReference(trigger.Slack); err != nil {
			return fmt.Errorf("agent trigger %q: %w", id, err)
		}
		if !ValidDeclaredID(id) {
			return errors.New("agent trigger id is invalid")
		}
		if len(trigger.Cron) > 1024 {
			return errors.New("agent cron exceeds 1024 bytes")
		}
		if err := ValidateCron(trigger.Cron); err != nil {
			return fmt.Errorf("agent trigger %q: %w", id, err)
		}
		if err := ValidateTimezone(trigger.Timezone); err != nil {
			return fmt.Errorf("agent trigger %q: %w", id, err)
		}
		if _, err := conversation.Input(trigger.Input); err != nil {
			return fmt.Errorf("agent trigger %q input: %w", id, err)
		}
	}
	return nil
}
func validateComputerSettings(resources ResourcesManifest, refresh *ComputerRefresh, secrets, buildSecrets []SecretBinding) error {
	if err := ValidateResourcesManifest(resources); err != nil {
		return err
	}
	if refresh != nil {
		if !positiveSafeInteger(refresh.EveryMs) || (refresh.MaxAgeMs != nil && !positiveSafeInteger(*refresh.MaxAgeMs)) {
			return errors.New("computer refresh requires positive safe integer milliseconds")
		}
	}
	if err := ValidateSecretBindings(secrets); err != nil {
		return err
	}
	return ValidateSecretBindings(buildSecrets)
}

func ValidateSecretBindings(refs []SecretBinding) error {
	_, err := secretbinding.NormalizeReferences(refs)
	return err
}

// ComputerManifest records the resolved base seed and authored preparation inputs.
// A seed is not an eligible prepared revision; preparation must publish that output.
type ComputerManifest struct {
	Seed         ComputerSeedManifest `json:"seed"`
	Resources    ResourcesManifest    `json:"resources"`
	Prepare      bool                 `json:"prepare"`
	Refresh      *ComputerRefresh     `json:"refresh,omitempty"`
	Secrets      []SecretBinding      `json:"secrets"`
	BuildSecrets []SecretBinding      `json:"buildSecrets"`
}
type ComputerSeedManifest struct {
	Profile        string            `json:"profile"`
	Config         oci.RuntimeConfig `json:"config"`
	ArtifactDigest string            `json:"artifactDigest"`
	MediaType      string            `json:"mediaType"`
}

func ValidateComputerManifest(m ComputerManifest) error {
	if m.Seed.Profile != ComputerSeedProfile || m.Seed.MediaType != ComputerSeedMediaType {
		return errors.New("unsupported Computer seed contract")
	}
	if !computerSeedDigest.MatchString(m.Seed.ArtifactDigest) {
		return errors.New("invalid Computer seed digest")
	}
	return validateComputerSettings(m.Resources, m.Refresh, m.Secrets, m.BuildSecrets)
}

var computerSeedDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
