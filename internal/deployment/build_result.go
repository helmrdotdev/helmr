package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/schedule"
	"github.com/helmrdotdev/helmr/internal/secret"
)

func ValidateBuildResult(result api.WorkerDeploymentBuildResult) ([]api.CASObject, error) {
	if strings.TrimSpace(result.BuildManifestDigest) == "" {
		return nil, errors.New("build_manifest_digest is required")
	}
	if strings.TrimSpace(result.DeploymentManifestDigest) == "" {
		return nil, errors.New("deployment_manifest_digest is required")
	}
	if len(result.Tasks) == 0 {
		return nil, errors.New("deployment build must include at least one task")
	}
	objects, casObjects, err := NormalizeBuildCASObjects(result.CASObjects)
	if err != nil {
		return nil, err
	}
	if err := requireBuildObject(objects, result.BuildManifestDigest, api.BuildManifestArtifactMediaType, "build_manifest_digest"); err != nil {
		return nil, err
	}
	if err := requireBuildObject(objects, result.DeploymentManifestDigest, api.DeploymentManifestArtifactMediaType, "deployment_manifest_digest"); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	queueLimits := map[string]*int32{}
	sandboxDefinitions := map[string]workerBuildSandboxDefinition{}
	for _, task := range result.Tasks {
		taskID := strings.TrimSpace(task.TaskID)
		if err := api.ValidateTaskID(taskID); err != nil {
			return nil, err
		}
		if _, ok := seen[taskID]; ok {
			return nil, fmt.Errorf("duplicate task_id %q", taskID)
		}
		seen[taskID] = struct{}{}
		if strings.TrimSpace(task.FilePath) == "" {
			return nil, fmt.Errorf("task %q file_path is required", taskID)
		}
		if strings.TrimSpace(task.ExportName) == "" {
			return nil, fmt.Errorf("task %q export_name is required", taskID)
		}
		if strings.TrimSpace(task.HandlerEntrypoint) == "" {
			return nil, fmt.Errorf("task %q handler_entrypoint is required", taskID)
		}
		if strings.TrimSpace(task.BundleDigest) == "" {
			return nil, fmt.Errorf("task %q bundle_digest is required", taskID)
		}
		if strings.TrimSpace(task.SandboxID) == "" {
			return nil, fmt.Errorf("task %q sandbox_id is required", taskID)
		}
		if strings.TrimSpace(task.SandboxFingerprint) == "" {
			return nil, fmt.Errorf("task %q sandbox_fingerprint is required", taskID)
		}
		if strings.TrimSpace(task.SandboxImageArtifact.Digest) == "" {
			return nil, fmt.Errorf("task %q sandbox_image_artifact.digest is required", taskID)
		}
		if strings.TrimSpace(task.SandboxImageArtifactFormat) == "" {
			return nil, fmt.Errorf("task %q sandbox_image_artifact_format is required", taskID)
		}
		if strings.TrimSpace(task.SandboxImageDigest) == "" {
			return nil, fmt.Errorf("task %q sandbox_image_digest is required", taskID)
		}
		if strings.TrimSpace(task.SandboxImageFormat) == "" {
			return nil, fmt.Errorf("task %q sandbox_image_format is required", taskID)
		}
		if strings.TrimSpace(task.SandboxImageArtifact.Digest) != strings.TrimSpace(task.SandboxImageDigest) {
			return nil, fmt.Errorf("task %q sandbox_image_digest must match sandbox_image_artifact.digest", taskID)
		}
		if strings.TrimSpace(task.WorkspaceMountPath) == "" {
			return nil, fmt.Errorf("task %q workspace_mount_path is required", taskID)
		}
		if strings.TrimSpace(task.FilesystemFormat) == "" {
			return nil, fmt.Errorf("task %q filesystem_format is required", taskID)
		}
		bundleFormatVersion := task.BundleFormatVersion
		if bundleFormatVersion == 0 {
			bundleFormatVersion = api.CurrentBundleFormatVersion
		}
		if bundleFormatVersion != api.CurrentBundleFormatVersion {
			return nil, fmt.Errorf("task %q bundle_format_version %d is not supported; current version is %d", taskID, bundleFormatVersion, api.CurrentBundleFormatVersion)
		}
		if err := requireBuildObject(objects, task.BundleDigest, api.TaskBundleArtifactMediaType, fmt.Sprintf("task %q bundle_digest", taskID)); err != nil {
			return nil, err
		}
		if err := requireBuildObject(objects, task.SandboxImageArtifact.Digest, api.SandboxImageArtifactMediaType, fmt.Sprintf("task %q sandbox_image_artifact.digest", taskID)); err != nil {
			return nil, err
		}
		normalizedImage := objects[strings.TrimSpace(task.SandboxImageArtifact.Digest)]
		if normalizedImage.SizeBytes != task.SandboxImageArtifact.SizeBytes || normalizedImage.MediaType != strings.TrimSpace(task.SandboxImageArtifact.MediaType) {
			return nil, fmt.Errorf("task %q sandbox_image_artifact metadata does not match CAS object", taskID)
		}
		resources := compute.ResourceVector{
			MilliCPU:  task.RequestedMilliCPU,
			MemoryMiB: task.RequestedMemoryMiB,
			DiskMiB:   task.RequestedDiskMiB,
			Slots:     1,
		}
		if err := resources.Validate(true); err != nil {
			return nil, fmt.Errorf("task %q resources: %w", taskID, err)
		}
		if task.RequestedDiskMiB > math.MaxInt32 {
			return nil, fmt.Errorf("task %q requested_disk_mib exceeds max %d", taskID, math.MaxInt32)
		}
		if err := task.Network.Validate(); err != nil {
			return nil, fmt.Errorf("task %q network: %w", taskID, err)
		}
		sandboxID := strings.TrimSpace(task.SandboxID)
		definition := workerBuildSandboxDefinition{
			fingerprint:        strings.TrimSpace(task.SandboxFingerprint),
			imageDigest:        strings.TrimSpace(task.SandboxImageDigest),
			imageFormat:        strings.TrimSpace(task.SandboxImageFormat),
			workspaceMountPath: strings.TrimSpace(task.WorkspaceMountPath),
			filesystemFormat:   strings.TrimSpace(task.FilesystemFormat),
			requestedMilliCPU:  task.RequestedMilliCPU,
			requestedMemoryMiB: task.RequestedMemoryMiB,
			requestedDiskMiB:   task.RequestedDiskMiB,
			network:            task.Network,
		}
		if existing, ok := sandboxDefinitions[sandboxID]; ok && !sameWorkerBuildSandboxDefinition(existing, definition) {
			return nil, fmt.Errorf("sandbox_id %q has conflicting definitions", sandboxID)
		}
		sandboxDefinitions[sandboxID] = definition
		if task.MaxDurationSeconds <= 0 {
			return nil, fmt.Errorf("task %q max_duration_seconds must be positive", taskID)
		}
		if err := api.ValidateQueueName(task.QueueName); err != nil {
			return nil, fmt.Errorf("task %q queue_name: %w", taskID, err)
		}
		if task.ConcurrencyLimit != nil && *task.ConcurrencyLimit <= 0 {
			return nil, fmt.Errorf("task %q concurrency_limit must be positive", taskID)
		}
		if ttl := strings.TrimSpace(task.TTL); ttl != "" {
			if _, err := api.ParsePositiveDuration(ttl, "ttl"); err != nil {
				return nil, fmt.Errorf("task %q ttl: %w", taskID, err)
			}
		}
		if err := validateTaskSchedules(taskID, task.Schedules); err != nil {
			return nil, err
		}
		if err := validateTaskSecrets(taskID, task.Secrets); err != nil {
			return nil, err
		}
		if existing, ok := queueLimits[task.QueueName]; ok && !sameOptionalInt32(existing, task.ConcurrencyLimit) {
			return nil, fmt.Errorf("queue %q has conflicting concurrency_limit values", task.QueueName)
		}
		queueLimits[task.QueueName] = task.ConcurrencyLimit
	}
	if err := validateDeploymentStreams(result.Streams); err != nil {
		return nil, err
	}
	return casObjects, nil
}

func validateDeploymentStreams(streams []api.WorkerDeploymentStream) error {
	seen := map[string]struct{}{}
	for i, item := range streams {
		name := strings.TrimSpace(item.Name)
		if err := api.ValidateStreamName(name); err != nil {
			return fmt.Errorf("deployment stream %d: %w", i, err)
		}
		direction := strings.TrimSpace(item.Direction)
		if direction != "input" && direction != "output" {
			return fmt.Errorf("deployment stream %q direction must be input or output", name)
		}
		key := direction + ":" + name
		if _, ok := seen[key]; ok {
			return fmt.Errorf("deployment has duplicate %s stream %q", direction, name)
		}
		seen[key] = struct{}{}
		if raw := item.SchemaJSON; len(raw) > 0 && !json.Valid(raw) {
			return fmt.Errorf("deployment stream %q schema_json must be valid JSON", name)
		}
	}
	return nil
}

type workerBuildSandboxDefinition struct {
	fingerprint        string
	imageDigest        string
	imageFormat        string
	workspaceMountPath string
	filesystemFormat   string
	requestedMilliCPU  int64
	requestedMemoryMiB int64
	requestedDiskMiB   int64
	network            compute.NetworkPolicy
}

func sameWorkerBuildSandboxDefinition(left workerBuildSandboxDefinition, right workerBuildSandboxDefinition) bool {
	return left.fingerprint == right.fingerprint &&
		left.imageDigest == right.imageDigest &&
		left.imageFormat == right.imageFormat &&
		left.workspaceMountPath == right.workspaceMountPath &&
		left.filesystemFormat == right.filesystemFormat &&
		left.requestedMilliCPU == right.requestedMilliCPU &&
		left.requestedMemoryMiB == right.requestedMemoryMiB &&
		left.requestedDiskMiB == right.requestedDiskMiB &&
		reflect.DeepEqual(left.network, right.network)
}

func validateTaskSchedules(taskID string, schedules []api.WorkerDeploymentTaskSchedule) error {
	seen := map[string]struct{}{}
	for i, item := range schedules {
		scheduleID := strings.TrimSpace(item.ID)
		if scheduleID == "" {
			scheduleID = "primary"
		}
		if err := api.ValidateScheduleID(scheduleID); err != nil {
			return fmt.Errorf("task %q schedule %d: %w", taskID, i, err)
		}
		if _, ok := seen[scheduleID]; ok {
			return fmt.Errorf("task %q has duplicate schedule id %q", taskID, scheduleID)
		}
		seen[scheduleID] = struct{}{}
		timezone := api.NormalizeTimezone(item.Timezone)
		if _, err := schedule.NextCronTime(strings.TrimSpace(item.Cron), timezone, time.Now()); err != nil {
			return fmt.Errorf("task %q schedule %q: %w", taskID, scheduleID, err)
		}
	}
	return nil
}

func validateTaskSecrets(taskID string, secrets []api.SecretDeclaration) error {
	seen := map[string]struct{}{}
	for i, item := range secrets {
		name := strings.TrimSpace(item.Name)
		if err := secret.ValidateName(name); err != nil {
			return fmt.Errorf("task %q secret %d: %w", taskID, i, err)
		}
		if _, ok := seen[name]; ok {
			return fmt.Errorf("task %q has duplicate secret %q", taskID, name)
		}
		seen[name] = struct{}{}
		placements := 0
		for _, value := range []string{item.Env, item.File, item.Dir} {
			if strings.TrimSpace(value) != "" {
				placements++
			}
		}
		if placements != 1 {
			return fmt.Errorf("task %q secret %q must declare exactly one placement", taskID, name)
		}
	}
	return nil
}

func sameOptionalInt32(a *int32, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func NormalizeBuildCASObjects(input []api.CASObject) (map[string]api.CASObject, []api.CASObject, error) {
	objects := make(map[string]api.CASObject, len(input))
	order := make([]string, 0, len(input))
	for _, object := range input {
		digest := strings.TrimSpace(object.Digest)
		if _, err := cas.ObjectKey("", digest); err != nil {
			return nil, nil, fmt.Errorf("deployment build CAS object digest is invalid: %w", err)
		}
		mediaType := strings.TrimSpace(object.MediaType)
		if mediaType == "" {
			return nil, nil, fmt.Errorf("deployment build CAS object %s media_type is required", digest)
		}
		if object.SizeBytes < 0 {
			return nil, nil, fmt.Errorf("deployment build CAS object %s size_bytes must not be negative", digest)
		}
		normalized := api.CASObject{Digest: digest, SizeBytes: object.SizeBytes, MediaType: mediaType}
		if existing, ok := objects[digest]; ok {
			if existing.SizeBytes != normalized.SizeBytes || existing.MediaType != normalized.MediaType {
				return nil, nil, fmt.Errorf("deployment build CAS object %s has conflicting metadata", digest)
			}
			continue
		}
		objects[digest] = normalized
		order = append(order, digest)
	}
	casObjects := make([]api.CASObject, 0, len(order))
	for _, digest := range order {
		casObjects = append(casObjects, objects[digest])
	}
	return objects, casObjects, nil
}

func requireBuildObject(objects map[string]api.CASObject, digest string, mediaType string, field string) error {
	digest = strings.TrimSpace(digest)
	object, ok := objects[digest]
	if !ok {
		return fmt.Errorf("%s must be included in cas_objects", field)
	}
	if strings.TrimSpace(object.MediaType) != mediaType {
		return fmt.Errorf("%s media_type must be %s", field, mediaType)
	}
	if object.SizeBytes < 0 {
		return fmt.Errorf("%s size_bytes must not be negative", field)
	}
	return nil
}
