package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

const (
	maxTaskCompletionOutputBytes  = 16 << 20
	maxTaskCompletionErrorBytes   = 16 << 10
	maxTaskCompletionMessageBytes = 1024
)

var taskWorkspaceDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type taskCompletionKind string

const (
	taskCompletionSucceeded      taskCompletionKind = "succeeded"
	taskCompletionFailed         taskCompletionKind = "failed"
	taskCompletionPayloadInvalid taskCompletionKind = "payload_invalid"
)

type parsedTaskCompletion struct {
	lease          parsedRunLeaseReceipt
	kind           taskCompletionKind
	output         json.RawMessage
	errorObject    json.RawMessage
	capture        *api.WorkerWorkspaceArtifact
	rollbackBaseID uuid.UUID
	fingerprint    string
}

func parseTaskCompletionRequest(request api.WorkerCompleteTaskRequest) (parsedTaskCompletion, error) {
	lease, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		return parsedTaskCompletion{}, err
	}
	normalized := request
	normalized.Lease.StartDeadlineAt = request.Lease.StartDeadlineAt.UTC()
	normalized.Lease.ExpiresAt = request.Lease.ExpiresAt.UTC()

	parsed := parsedTaskCompletion{lease: lease}
	outcomes := 0
	if request.Outcome.Succeeded != nil {
		outcomes++
		parsed.kind = taskCompletionSucceeded
		if len(request.Outcome.Succeeded.Output) == 0 {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is required")
		}
		if len(request.Outcome.Succeeded.Output) > maxTaskCompletionOutputBytes {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is too large")
		}
		parsed.output, err = canonicalJSON(request.Outcome.Succeeded.Output)
		if err != nil {
			return parsedTaskCompletion{}, fmt.Errorf("outcome.succeeded.output must be an unambiguous JSON value: %w", err)
		}
		if len(parsed.output) > maxTaskCompletionOutputBytes {
			return parsedTaskCompletion{}, errors.New("outcome.succeeded.output is too large")
		}
		normalized.Outcome.Succeeded = &api.WorkerTaskSucceeded{Output: parsed.output}
	}
	if request.Outcome.Failed != nil {
		outcomes++
		parsed.kind = taskCompletionFailed
		parsed.errorObject, normalized.Outcome.Failed, err = normalizeTaskFailure("outcome.failed", request.Outcome.Failed)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
	}
	if request.Outcome.PayloadInvalid != nil {
		outcomes++
		parsed.kind = taskCompletionPayloadInvalid
		parsed.errorObject, normalized.Outcome.PayloadInvalid, err = normalizeTaskFailure("outcome.payload_invalid", request.Outcome.PayloadInvalid)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
	}
	if outcomes != 1 {
		return parsedTaskCompletion{}, errors.New("outcome must contain exactly one variant")
	}

	proofs := 0
	if request.Workspace.Captured != nil {
		proofs++
		artifact := request.Workspace.Captured.Artifact
		if err := validateTaskWorkspaceArtifact(artifact); err != nil {
			return parsedTaskCompletion{}, err
		}
		parsed.capture = &artifact
	}
	if request.Workspace.RolledBack != nil {
		proofs++
		baseID, err := parseCanonicalUUID(
			"workspace.rolled_back.base_workspace_version_id",
			request.Workspace.RolledBack.BaseWorkspaceVersionID,
		)
		if err != nil {
			return parsedTaskCompletion{}, err
		}
		if baseID != lease.baseWorkspaceVersionID {
			return parsedTaskCompletion{}, errors.New("workspace rollback does not match the Lease base Workspace version")
		}
		parsed.rollbackBaseID = baseID
	}
	if proofs != 1 {
		return parsedTaskCompletion{}, errors.New("workspace must contain exactly one proof")
	}
	if parsed.kind == taskCompletionSucceeded && parsed.capture == nil {
		return parsedTaskCompletion{}, errors.New("a successful Task requires a captured Workspace")
	}
	if parsed.kind != taskCompletionSucceeded && parsed.rollbackBaseID == uuid.Nil {
		return parsedTaskCompletion{}, errors.New("a failed Task requires a Workspace rollback")
	}

	parsed.fingerprint, err = terminalRequestFingerprint("task.complete.v0", normalized)
	if err != nil {
		return parsedTaskCompletion{}, fmt.Errorf("fingerprint Task completion: %w", err)
	}
	return parsed, nil
}

func normalizeTaskFailure(label string, failure *api.WorkerTaskFailure) (json.RawMessage, *api.WorkerTaskFailure, error) {
	if !utf8.ValidString(failure.Message) || len(failure.Message) > maxTaskCompletionMessageBytes {
		return nil, nil, fmt.Errorf("%s.message must be valid UTF-8 no larger than %d bytes", label, maxTaskCompletionMessageBytes)
	}
	var details json.RawMessage
	if len(failure.Details) != 0 {
		if len(failure.Details) > maxTaskCompletionErrorBytes {
			return nil, nil, fmt.Errorf("%s.details is too large", label)
		}
		canonical, err := canonicalJSON(failure.Details)
		if err != nil {
			return nil, nil, fmt.Errorf("%s.details must be an unambiguous JSON value: %w", label, err)
		}
		details = canonical
	}
	errorObject, err := json.Marshal(struct {
		Message string          `json:"message"`
		Details json.RawMessage `json:"details,omitempty"`
	}{Message: failure.Message, Details: details})
	if err != nil {
		return nil, nil, fmt.Errorf("encode %s: %w", label, err)
	}
	errorObject, err = canonicalJSON(errorObject)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if len(errorObject) > maxTaskCompletionErrorBytes {
		return nil, nil, fmt.Errorf("%s exceeds %d bytes", label, maxTaskCompletionErrorBytes)
	}
	return errorObject, &api.WorkerTaskFailure{Message: failure.Message, Details: details}, nil
}

func validateTaskWorkspaceArtifact(artifact api.WorkerWorkspaceArtifact) error {
	if !taskWorkspaceDigestPattern.MatchString(artifact.Digest) {
		return errors.New("workspace.captured.artifact.digest must be a SHA-256 digest")
	}
	if artifact.MediaType != workspace.ArtifactMediaType {
		return errors.New("workspace.captured.artifact.media_type is unsupported")
	}
	if artifact.Encoding != workspace.ArtifactEncoding {
		return errors.New("workspace.captured.artifact.encoding is unsupported")
	}
	if artifact.SizeBytes <= 0 || artifact.SizeBytes > workspace.MaxArtifactArchiveBytes {
		return fmt.Errorf("workspace.captured.artifact.size_bytes must be between 1 and %d", workspace.MaxArtifactArchiveBytes)
	}
	if artifact.EntryCount < 0 || artifact.EntryCount > workspace.MaxArtifactEntries {
		return fmt.Errorf("workspace.captured.artifact.entry_count must be between 0 and %d", workspace.MaxArtifactEntries)
	}
	return nil
}

func parseCanonicalUUID(name, value string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value {
		return uuid.Nil, fmt.Errorf("%s must be a canonical UUID", name)
	}
	return parsed, nil
}
