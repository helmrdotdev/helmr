package definition

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/content"
	"regexp"

	"github.com/helmrdotdev/helmr/internal/mcppolicy"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

type AgentManifest struct {
	RuntimeMcp           json.RawMessage       `json:"runtimeMcp,omitempty"`
	TemplateDefinitionID string                `json:"templateDefinitionId"`
	InputKind            string                `json:"inputKind"`
	InputSchema          *InputSchemaDiscovery `json:"inputSchema,omitempty"`
	HasInit              bool                  `json:"hasInit"`
	// CloseAfterIdleMs and HibernateAfterIdleMs are always present; null means never.
	CloseAfterIdleMs     *int64                 `json:"closeAfterIdleMs"`
	HibernateAfterIdleMs *int64                 `json:"hibernateAfterIdleMs"`
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

type TemplateInputManifest struct {
	// DeleteAfterUnusedMs is always present; null means never.
	DeleteAfterUnusedMs *int64            `json:"deleteAfterUnusedMs"`
	ImageBuild          ImageBuild        `json:"imageBuild"`
	Resources           ResourcesManifest `json:"resources"`
	Network             json.RawMessage   `json:"network"`
	PrepareNetwork      json.RawMessage   `json:"prepareNetwork,omitempty"`
	Prepare             bool              `json:"prepare"`
	Refresh             *ComputerRefresh  `json:"refresh,omitempty"`
	Secrets             []SecretBinding   `json:"secrets"`
	PrepareSecrets      []SecretBinding   `json:"prepareSecrets"`
}

func ValidateAgentManifest(m AgentManifest) error {
	if _, err := mcppolicy.Normalize(m.RuntimeMcp, nil); err != nil {
		return fmt.Errorf("%w runtimeMcp: %w", ErrInvalidRuntimeMcpPolicy, err)
	}
	if err := ValidateInputContract(m.InputKind, m.InputSchema); err != nil {
		return err
	}
	if !ValidDeclaredID(m.TemplateDefinitionID) {
		return errors.New("agent templateDefinitionId is invalid")
	}
	for _, duration := range []*int64{m.CloseAfterIdleMs, m.HibernateAfterIdleMs, m.MaxTurnDurationMs} {
		if duration != nil && !positiveSafeInteger(*duration) {
			return errors.New("agent duration must be positive safe integer milliseconds")
		}
	}
	if m.Triggers == nil {
		return errors.New("agent triggers must be an object")
	}
	for id, trigger := range m.Triggers {
		if channel, err := SlackChannelReference(trigger.Slack); err != nil {
			return fmt.Errorf("agent trigger %q: %w", id, err)
		} else if channel != nil && m.InputKind != "text" {
			return errors.New("structured-input agents cannot have a Slack cron destination")
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
		if _, err := content.InvocationFor(m.InputKind, trigger.Input); err != nil {
			return fmt.Errorf("agent trigger %q input: %w", id, err)
		}
	}
	return nil
}
func validateComputerSettings(resources ResourcesManifest, refresh *ComputerRefresh, secrets, prepareSecrets []SecretBinding) error {
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
	return ValidateSecretBindings(prepareSecrets)
}

func ValidateSecretBindings(refs []SecretBinding) error {
	_, err := secretbinding.NormalizeReferences(refs)
	return err
}

// TemplateManifest records the resolved base seed and authored preparation inputs.
// A seed is not an eligible prepared revision; preparation must publish that output.
type TemplateManifest struct {
	DeleteAfterUnusedMs *int64               `json:"deleteAfterUnusedMs"`
	Seed                ComputerSeedManifest `json:"seed"`
	Resources           ResourcesManifest    `json:"resources"`
	Network             json.RawMessage      `json:"network"`
	PrepareNetwork      json.RawMessage      `json:"prepareNetwork,omitempty"`
	Prepare             bool                 `json:"prepare"`
	Refresh             *ComputerRefresh     `json:"refresh,omitempty"`
	Secrets             []SecretBinding      `json:"secrets"`
	PrepareSecrets      []SecretBinding      `json:"prepareSecrets"`
}
type ComputerSeedManifest struct {
	Profile        string            `json:"profile"`
	Config         oci.RuntimeConfig `json:"config"`
	ArtifactDigest string            `json:"artifactDigest"`
	MediaType      string            `json:"mediaType"`
}

func ValidateTemplateManifest(m TemplateManifest) error {
	if err := ValidateUnusedPolicy(m.DeleteAfterUnusedMs); err != nil {
		return err
	}
	if m.Seed.Profile != ComputerSeedProfile || m.Seed.MediaType != ComputerSeedMediaType {
		return errors.New("unsupported Computer seed contract")
	}
	if !computerSeedDigest.MatchString(m.Seed.ArtifactDigest) {
		return errors.New("invalid Computer seed digest")
	}
	if _, _, err := ResolveTemplatePolicies(m.Network, m.PrepareNetwork, m.Prepare, m.Secrets, m.PrepareSecrets); err != nil {
		return err
	}
	return validateComputerSettings(m.Resources, m.Refresh, m.Secrets, m.PrepareSecrets)
}

var computerSeedDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ValidateUnusedPolicy(duration *int64) error {
	if duration != nil && !positiveSafeInteger(*duration) {
		return errors.New("invalid Computer unused retention")
	}
	return nil
}
