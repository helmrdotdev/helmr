package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/schedule"
)

const (
	BuildPlanFormatVersion = 0

	DefinitionKindTask      = DefinitionKind("task")
	DefinitionKindActor     = DefinitionKind("actor")
	DefinitionKindWorkspace = DefinitionKind("workspace")

	SchemaKindNone     = SchemaKind("none")
	SchemaKindStandard = SchemaKind("standard_schema")

	RetryJitterNone = RetryJitter("none")
	RetryJitterFull = RetryJitter("full")

	maxBuildPlanBytes          = 16 << 20
	maxBuildDefinitions        = 10000
	maxBuildQueues             = 1000
	maxBuildImageSteps         = 10000
	maxNetworkDenyCIDRs        = 1024
	maxWorkspaceKeyBytes       = 512
	minRunDurationMs     int64 = 5000
	maxRunDurationMs     int64 = 86400000
	maxQueuedRunTTLMs    int64 = 31536000000
	maxActorIdleMs       int64 = 3600000
	maxRetryDelayMs      int64 = 86400000
)

type DefinitionKind string
type SchemaKind string
type RetryJitter string

type BuildPlan struct {
	FormatVersion int               `json:"formatVersion"`
	Definitions   []DefinitionInput `json:"definitions"`
	Queues        []QueueInput      `json:"queues"`
}

type DefinitionInput struct {
	Kind       DefinitionKind `json:"-"`
	DeclaredID string         `json:"-"`
	Task       *TaskManifest
	Actor      *ActorManifest
	Workspace  *WorkspaceInputManifest
}

type TaskManifest struct {
	Payload  SchemaManifest    `json:"payload"`
	Run      RunManifest       `json:"run"`
	Schedule *ScheduleManifest `json:"schedule,omitempty"`
}

type ActorManifest struct {
	Run           RunManifest `json:"run"`
	IdleTimeoutMs int64       `json:"idleTimeoutMs"`
}

type WorkspaceInputManifest struct {
	ImageBuild imagebuild.Build  `json:"imageBuild"`
	Resources  ResourcesManifest `json:"resources"`
	Network    NetworkManifest   `json:"network"`
}

type WorkspaceManifest struct {
	Image     WorkspaceArtifactManifest `json:"image"`
	Resources ResourcesManifest         `json:"resources"`
	Network   NetworkManifest           `json:"network"`
}

type WorkspaceArtifactManifest struct {
	ArtifactDigest string `json:"artifactDigest"`
	MediaType      string `json:"mediaType"`
}

type SchemaManifest struct {
	Kind SchemaKind `json:"kind"`
}

type RunManifest struct {
	Queue         string        `json:"queue"`
	MaxDurationMs int64         `json:"maxDurationMs"`
	Retry         RetryManifest `json:"retry"`
	TTLMs         *int64        `json:"ttlMs,omitempty"`
}

type RetryManifest struct {
	Enabled     bool          `json:"enabled"`
	MaxAttempts *int64        `json:"maxAttempts,omitempty"`
	Backoff     *RetryBackoff `json:"backoff,omitempty"`
}

type RetryBackoff struct {
	MinMs  int64       `json:"minMs"`
	MaxMs  int64       `json:"maxMs"`
	Factor int64       `json:"factor"`
	Jitter RetryJitter `json:"jitter"`
}

type ScheduleManifest struct {
	Cron      string          `json:"cron"`
	Timezone  string          `json:"timezone"`
	Workspace WorkspaceTarget `json:"workspace"`
}

type WorkspaceTarget struct {
	ID  *string `json:"id,omitempty"`
	Key *string `json:"key,omitempty"`
}

type ResourcesManifest struct {
	MilliCPU  int64 `json:"milliCpu"`
	MemoryMiB int64 `json:"memoryMiB"`
}

type NetworkManifest struct {
	Internet  bool     `json:"internet"`
	DenyCIDRs []string `json:"denyCidrs"`
}

type QueueInput struct {
	Name             string `json:"name"`
	ConcurrencyLimit *int64 `json:"concurrencyLimit,omitempty"`
}

type QueueConfig struct {
	FormatVersion int          `json:"formatVersion"`
	Queues        []QueueInput `json:"queues"`
}

func QueueConfigFromPlan(plan BuildPlan) (QueueConfig, error) {
	if err := ValidateBuildPlan(plan); err != nil {
		return QueueConfig{}, err
	}
	config := QueueConfig{
		FormatVersion: BuildPlanFormatVersion,
		Queues:        append([]QueueInput(nil), plan.Queues...),
	}
	for index := range config.Queues {
		if plan.Queues[index].ConcurrencyLimit != nil {
			limit := *plan.Queues[index].ConcurrencyLimit
			config.Queues[index].ConcurrencyLimit = &limit
		}
	}
	return config, nil
}

func CanonicalQueueConfig(config QueueConfig) ([]byte, error) {
	if config.FormatVersion != BuildPlanFormatVersion {
		return nil, fmt.Errorf(
			"queue config formatVersion = %d, want %d",
			config.FormatVersion,
			BuildPlanFormatVersion,
		)
	}
	for index, queue := range config.Queues {
		if err := validateQueueInput(queue); err != nil {
			return nil, fmt.Errorf("queue config queue %d: %w", index, err)
		}
		if index > 0 && bytes.Compare(
			[]byte(config.Queues[index-1].Name),
			[]byte(queue.Name),
		) >= 0 {
			return nil, fmt.Errorf("queue config queues are not in canonical name order at position %d", index)
		}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode queue config: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize queue config: %w", err)
	}
	return canonical, nil
}

func ParseBuildPlan(raw []byte) (BuildPlan, error) {
	if len(raw) == 0 || len(raw) > maxBuildPlanBytes {
		return BuildPlan{}, fmt.Errorf("build plan size is outside [1,%d]", maxBuildPlanBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return BuildPlan{}, fmt.Errorf("canonicalize build plan: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return BuildPlan{}, errors.New("build plan is not RFC 8785 canonical JSON")
	}

	var plan BuildPlan
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return BuildPlan{}, fmt.Errorf("decode build plan: %w", err)
	}
	if err := ensureEOF(decoder, "build plan"); err != nil {
		return BuildPlan{}, err
	}
	if err := ValidateBuildPlan(plan); err != nil {
		return BuildPlan{}, err
	}
	complete, err := CanonicalBuildPlan(plan)
	if err != nil {
		return BuildPlan{}, err
	}
	if !bytes.Equal(raw, complete) {
		return BuildPlan{}, errors.New("build plan does not match the complete canonical v0 shape")
	}
	return plan, nil
}

func CanonicalBuildPlan(plan BuildPlan) ([]byte, error) {
	if err := ValidateBuildPlan(plan); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("encode build plan: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize build plan: %w", err)
	}
	if len(canonical) > maxBuildPlanBytes {
		return nil, fmt.Errorf("build plan size is outside [1,%d]", maxBuildPlanBytes)
	}
	return canonical, nil
}

func ValidateBuildPlan(plan BuildPlan) error {
	if plan.FormatVersion != BuildPlanFormatVersion {
		return fmt.Errorf(
			"build plan formatVersion = %d, want %d",
			plan.FormatVersion,
			BuildPlanFormatVersion,
		)
	}
	if len(plan.Definitions) == 0 {
		return errors.New("build plan definitions must be a non-empty array")
	}
	if len(plan.Definitions) > maxBuildDefinitions {
		return fmt.Errorf("build plan contains more than %d definitions", maxBuildDefinitions)
	}
	if plan.Queues == nil {
		return errors.New("build plan queues must be an array")
	}
	if len(plan.Queues) > maxBuildQueues {
		return fmt.Errorf("build plan contains more than %d queues", maxBuildQueues)
	}

	queues := make(map[string]struct{}, len(plan.Queues))
	for index, queue := range plan.Queues {
		if err := validateQueueInput(queue); err != nil {
			return fmt.Errorf("build plan queue %d: %w", index, err)
		}
		if index > 0 && bytes.Compare([]byte(plan.Queues[index-1].Name), []byte(queue.Name)) >= 0 {
			return fmt.Errorf("build plan queues are not in canonical name order at position %d", index)
		}
		queues[queue.Name] = struct{}{}
	}

	imageSteps := 0
	for index, definition := range plan.Definitions {
		if err := validateDefinitionInput(definition, queues); err != nil {
			return fmt.Errorf("build plan definition %d: %w", index, err)
		}
		if index > 0 && compareDefinitionInputs(plan.Definitions[index-1], definition) >= 0 {
			return fmt.Errorf("build plan definitions are not in canonical order at position %d", index)
		}
		if definition.Workspace != nil {
			imageSteps += imagebuild.StepCount(definition.Workspace.ImageBuild)
			if imageSteps > maxBuildImageSteps {
				return fmt.Errorf("build plan contains more than %d image steps", maxBuildImageSteps)
			}
		}
	}
	return nil
}

func (input DefinitionInput) MarshalJSON() ([]byte, error) {
	if input.manifestCount() != 1 {
		return nil, errors.New("definition input must contain exactly one manifest")
	}
	switch input.Kind {
	case DefinitionKindTask:
		if input.Task == nil {
			return nil, errors.New("task definition requires a task manifest")
		}
		return json.Marshal(struct {
			Kind       DefinitionKind `json:"kind"`
			DeclaredID string         `json:"declaredId"`
			Manifest   *TaskManifest  `json:"manifest"`
		}{input.Kind, input.DeclaredID, input.Task})
	case DefinitionKindActor:
		if input.Actor == nil {
			return nil, errors.New("actor definition requires an actor manifest")
		}
		return json.Marshal(struct {
			Kind       DefinitionKind `json:"kind"`
			DeclaredID string         `json:"declaredId"`
			Manifest   *ActorManifest `json:"manifest"`
		}{input.Kind, input.DeclaredID, input.Actor})
	case DefinitionKindWorkspace:
		if input.Workspace == nil {
			return nil, errors.New("workspace definition requires a workspace manifest")
		}
		return json.Marshal(struct {
			Kind       DefinitionKind          `json:"kind"`
			DeclaredID string                  `json:"declaredId"`
			Manifest   *WorkspaceInputManifest `json:"manifest"`
		}{input.Kind, input.DeclaredID, input.Workspace})
	default:
		return nil, fmt.Errorf("definition input kind %q is unsupported", input.Kind)
	}
}

func (input *DefinitionInput) UnmarshalJSON(raw []byte) error {
	var header struct {
		Kind DefinitionKind `json:"kind"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}

	*input = DefinitionInput{Kind: header.Kind}
	switch header.Kind {
	case DefinitionKindTask:
		var wire struct {
			Kind       DefinitionKind `json:"kind"`
			DeclaredID string         `json:"declaredId"`
			Manifest   *TaskManifest  `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		input.DeclaredID = wire.DeclaredID
		input.Task = wire.Manifest
	case DefinitionKindActor:
		var wire struct {
			Kind       DefinitionKind `json:"kind"`
			DeclaredID string         `json:"declaredId"`
			Manifest   *ActorManifest `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		input.DeclaredID = wire.DeclaredID
		input.Actor = wire.Manifest
	case DefinitionKindWorkspace:
		var wire struct {
			Kind       DefinitionKind          `json:"kind"`
			DeclaredID string                  `json:"declaredId"`
			Manifest   *WorkspaceInputManifest `json:"manifest"`
		}
		if err := decodeClosedDefinition(raw, &wire); err != nil {
			return err
		}
		input.DeclaredID = wire.DeclaredID
		input.Workspace = wire.Manifest
	default:
		return fmt.Errorf("definition input kind %q is unsupported", header.Kind)
	}
	return nil
}

func decodeClosedDefinition(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return ensureEOF(decoder, "definition input")
}

func (input DefinitionInput) manifestCount() int {
	count := 0
	for _, present := range []bool{
		input.Task != nil,
		input.Actor != nil,
		input.Workspace != nil,
	} {
		if present {
			count++
		}
	}
	return count
}

func validateDefinitionInput(input DefinitionInput, queues map[string]struct{}) error {
	if !declaredIDPattern.MatchString(input.DeclaredID) {
		return fmt.Errorf("declaredId %q is outside the exact ASCII ID domain", input.DeclaredID)
	}
	if input.manifestCount() != 1 {
		return errors.New("definition input must contain exactly one manifest")
	}
	switch input.Kind {
	case DefinitionKindTask:
		if input.Task == nil {
			return errors.New("task definition requires a task manifest")
		}
		if input.Task.Payload.Kind != SchemaKindNone && input.Task.Payload.Kind != SchemaKindStandard {
			return fmt.Errorf("task payload kind %q is unsupported", input.Task.Payload.Kind)
		}
		if err := validateRunManifest(input.Task.Run, queues); err != nil {
			return fmt.Errorf("task run: %w", err)
		}
		if input.Task.Schedule != nil {
			if input.Task.Payload.Kind != SchemaKindStandard {
				return errors.New("scheduled task payload kind must be standard_schema")
			}
			if err := validateScheduleManifest(*input.Task.Schedule); err != nil {
				return fmt.Errorf("task schedule: %w", err)
			}
		}
	case DefinitionKindActor:
		if input.Actor == nil {
			return errors.New("actor definition requires an actor manifest")
		}
		if err := validateRunManifest(input.Actor.Run, queues); err != nil {
			return fmt.Errorf("actor run: %w", err)
		}
		if input.Actor.IdleTimeoutMs < 1 || input.Actor.IdleTimeoutMs > maxActorIdleMs {
			return fmt.Errorf("actor idleTimeoutMs must be in [1,%d]", maxActorIdleMs)
		}
	case DefinitionKindWorkspace:
		if input.Workspace == nil {
			return errors.New("workspace definition requires a workspace manifest")
		}
		if err := imagebuild.Validate(
			input.Workspace.ImageBuild,
			string(ArchitectureX8664),
		); err != nil {
			return fmt.Errorf("workspace imageBuild: %w", err)
		}
		if err := validateResourcesManifest(input.Workspace.Resources); err != nil {
			return fmt.Errorf("workspace resources: %w", err)
		}
		if err := validateNetworkManifest(input.Workspace.Network); err != nil {
			return fmt.Errorf("workspace network: %w", err)
		}
	default:
		return fmt.Errorf("definition input kind %q is unsupported", input.Kind)
	}
	return nil
}

func validateRunManifest(run RunManifest, queues map[string]struct{}) error {
	if err := api.ValidateQueueName(run.Queue); err != nil {
		return err
	}
	if _, ok := queues[run.Queue]; !ok {
		return fmt.Errorf("queue %q is not declared by the build plan", run.Queue)
	}
	if run.MaxDurationMs < minRunDurationMs || run.MaxDurationMs > maxRunDurationMs {
		return fmt.Errorf(
			"maxDurationMs is outside [%d,%d]",
			minRunDurationMs,
			maxRunDurationMs,
		)
	}
	if run.TTLMs != nil && (*run.TTLMs < 1 || *run.TTLMs > maxQueuedRunTTLMs) {
		return fmt.Errorf("ttlMs must be in [1,%d]", maxQueuedRunTTLMs)
	}
	return validateRetryManifest(run.Retry)
}

func validateRetryManifest(retry RetryManifest) error {
	if !retry.Enabled {
		if retry.MaxAttempts != nil || retry.Backoff != nil {
			return errors.New("disabled retry must contain only enabled")
		}
		return nil
	}
	if retry.MaxAttempts == nil || *retry.MaxAttempts < 1 || *retry.MaxAttempts > 10 {
		return errors.New("enabled retry maxAttempts must be in [1,10]")
	}
	if retry.Backoff == nil {
		return errors.New("enabled retry requires backoff")
	}
	if retry.Backoff.MinMs < 1 || retry.Backoff.MinMs > maxRetryDelayMs {
		return fmt.Errorf("retry backoff minMs must be in [1,%d]", maxRetryDelayMs)
	}
	if retry.Backoff.MaxMs < 1 || retry.Backoff.MaxMs > maxRetryDelayMs {
		return fmt.Errorf("retry backoff maxMs must be in [1,%d]", maxRetryDelayMs)
	}
	if retry.Backoff.MinMs > retry.Backoff.MaxMs {
		return errors.New("retry backoff minMs must not exceed maxMs")
	}
	if retry.Backoff.Factor < 1 || retry.Backoff.Factor > 100 {
		return errors.New("retry backoff factor must be an integer in [1,100]")
	}
	if retry.Backoff.Jitter != RetryJitterNone && retry.Backoff.Jitter != RetryJitterFull {
		return fmt.Errorf("retry backoff jitter %q is unsupported", retry.Backoff.Jitter)
	}
	return nil
}

func ParseRetryManifest(raw []byte) (RetryManifest, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return RetryManifest{}, fmt.Errorf("canonicalize retry manifest: %w", err)
	}
	var manifest RetryManifest
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return RetryManifest{}, fmt.Errorf("decode retry manifest: %w", err)
	}
	if err := ensureEOF(decoder, "retry manifest"); err != nil {
		return RetryManifest{}, err
	}
	if err := validateRetryManifest(manifest); err != nil {
		return RetryManifest{}, err
	}
	completeRaw, err := json.Marshal(manifest)
	if err != nil {
		return RetryManifest{}, fmt.Errorf("encode retry manifest: %w", err)
	}
	complete, err := jsoncanon.Transform(completeRaw)
	if err != nil {
		return RetryManifest{}, fmt.Errorf("canonicalize complete retry manifest: %w", err)
	}
	if !bytes.Equal(canonical, complete) {
		return RetryManifest{}, errors.New("retry manifest does not match the complete canonical v0 shape")
	}
	return manifest, nil
}

func validateScheduleManifest(manifest ScheduleManifest) error {
	if len(manifest.Cron) == 0 || len(manifest.Cron) > 1024 {
		return errors.New("cron must be 1-1024 bytes")
	}
	if len(manifest.Timezone) == 0 || len(manifest.Timezone) > 255 {
		return errors.New("timezone must be 1-255 bytes")
	}
	if err := schedule.ValidateCron(manifest.Cron); err != nil {
		return err
	}
	if err := schedule.ValidateTimezone(manifest.Timezone); err != nil {
		return err
	}
	return validateWorkspaceTarget(manifest.Workspace)
}

func validateWorkspaceTarget(target WorkspaceTarget) error {
	if (target.ID == nil) == (target.Key == nil) {
		return errors.New("workspace target must contain exactly one of id or key")
	}
	if target.ID != nil {
		if strings.TrimSpace(*target.ID) != *target.ID {
			return errors.New("workspace target id must not contain surrounding whitespace")
		}
		if err := publicid.ValidateFor(publicid.Workspace, *target.ID); err != nil {
			return fmt.Errorf("workspace target id: %w", err)
		}
		return nil
	}
	key := *target.Key
	if key == "" ||
		!utf8.ValidString(key) ||
		len(key) > maxWorkspaceKeyBytes ||
		strings.ContainsRune(key, '\x00') ||
		strings.TrimSpace(key) != key {
		return errors.New("workspace target key is outside the exact Workspace key domain")
	}
	return nil
}

func validateResourcesManifest(resources ResourcesManifest) error {
	if !positiveSafeInteger(resources.MilliCPU) {
		return errors.New("milliCpu must be a positive JavaScript-safe integer")
	}
	if !positiveSafeInteger(resources.MemoryMiB) {
		return errors.New("memoryMiB must be a positive JavaScript-safe integer")
	}
	return nil
}

func validateNetworkManifest(network NetworkManifest) error {
	if network.DenyCIDRs == nil {
		return errors.New("denyCidrs must be an array")
	}
	if !network.Internet && len(network.DenyCIDRs) > 0 {
		return errors.New("denyCidrs must be empty when internet is disabled")
	}
	if len(network.DenyCIDRs) > maxNetworkDenyCIDRs {
		return fmt.Errorf("denyCidrs contains more than %d prefixes", maxNetworkDenyCIDRs)
	}
	for index, raw := range network.DenyCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Masked() != prefix || prefix.String() != raw {
			return fmt.Errorf("denyCidrs[%d] is not a canonical masked IP prefix", index)
		}
		if index > 0 && bytes.Compare(
			[]byte(network.DenyCIDRs[index-1]),
			[]byte(raw),
		) >= 0 {
			return fmt.Errorf("denyCidrs is not in canonical order at position %d", index)
		}
	}
	return nil
}

func validateQueueInput(queue QueueInput) error {
	if err := api.ValidateQueueName(queue.Name); err != nil {
		return err
	}
	if queue.ConcurrencyLimit != nil && !positiveSafeInteger(*queue.ConcurrencyLimit) {
		return errors.New("concurrencyLimit must be a positive JavaScript-safe integer")
	}
	return nil
}

func positiveSafeInteger(value int64) bool {
	return value > 0 && value <= maxJSONSafeInteger
}

func compareDefinitionInputs(left, right DefinitionInput) int {
	leftKind := definitionKindOrder(left.Kind)
	rightKind := definitionKindOrder(right.Kind)
	if leftKind < rightKind {
		return -1
	}
	if leftKind > rightKind {
		return 1
	}
	return bytes.Compare([]byte(left.DeclaredID), []byte(right.DeclaredID))
}

func definitionKindOrder(kind DefinitionKind) int {
	switch kind {
	case DefinitionKindTask:
		return 0
	case DefinitionKindActor:
		return 1
	case DefinitionKindWorkspace:
		return 2
	default:
		return 3
	}
}
