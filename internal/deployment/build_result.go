package deployment

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
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
		if err := requireBuildObject(objects, task.BundleDigest, api.TaskBundleArtifactMediaType, fmt.Sprintf("task %q bundle_digest", taskID)); err != nil {
			return nil, err
		}
		resources := compute.ResourceVector{
			MilliCPU:  task.RequestedMilliCPU,
			MemoryMiB: task.RequestedMemoryMiB,
			Slots:     1,
		}
		if err := resources.Validate(true); err != nil {
			return nil, fmt.Errorf("task %q resources: %w", taskID, err)
		}
		if task.MaxDurationSeconds <= 0 {
			return nil, fmt.Errorf("task %q max_duration_seconds must be positive", taskID)
		}
		if err := validatePayloadSchemaJSON(task.PayloadSchema); err != nil {
			return nil, fmt.Errorf("task %q payload_schema: %w", taskID, err)
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
		if existing, ok := queueLimits[task.QueueName]; ok && !sameOptionalInt32(existing, task.ConcurrencyLimit) {
			return nil, fmt.Errorf("queue %q has conflicting concurrency_limit values", task.QueueName)
		}
		queueLimits[task.QueueName] = task.ConcurrencyLimit
	}
	return casObjects, nil
}

func sameOptionalInt32(a *int32, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func validatePayloadSchemaJSON(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return errors.New("must be valid JSON")
	}
	switch value.(type) {
	case bool, map[string]any:
		return nil
	default:
		return errors.New("must be a JSON Schema object or boolean")
	}
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
