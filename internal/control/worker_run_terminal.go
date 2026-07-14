package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerMessagePayload struct {
	Message string `json:"message"`
}

type runCompletedPayload struct {
	ExitCode int32 `json:"exit_code"`
}

type runFailurePayload struct {
	Detail      any    `json:"detail"`
	FailureKind string `json:"failure_kind"`
}

type taskFailedDetailPayload struct {
	ExitCode int32 `json:"exit_code"`
}

type workerFailureDetailPayload struct {
	LimitSeconds *int32 `json:"limit_seconds,omitempty"`
	Message      string `json:"message"`
}

type runCancelledPayload struct {
	Reason string `json:"reason"`
}

type terminalPayloadError struct {
	kind string
	err  error
}

func terminalPayload(kind string, err error) error {
	return terminalPayloadError{kind: kind, err: err}
}

func releaseFields(result api.WorkerReleaseResult) (db.RunStatus, pgtype.Int4, pgtype.Text, error) {
	switch result.Kind {
	case "completed":
		if result.ExitCode == nil {
			return "", pgtype.Int4{}, pgtype.Text{}, errors.New("result.exit_code is required for completed releases")
		}
		status := db.RunStatusSucceeded
		if *result.ExitCode != 0 {
			status = db.RunStatusFailed
		}
		return status, pgtype.Int4{Int32: *result.ExitCode, Valid: true}, pgtype.Text{}, nil
	case "failed":
		message := "worker execution failed"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, nil
	case "cancelled":
		message := "worker execution cancelled"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return db.RunStatusCancelled, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, nil
	default:
		return "", pgtype.Int4{}, pgtype.Text{}, fmt.Errorf("unsupported release result kind %q", result.Kind)
	}
}

func releaseOutput(result api.WorkerReleaseResult, status db.RunStatus, exitCode pgtype.Int4) []byte {
	if status != db.RunStatusSucceeded || !exitCode.Valid || exitCode.Int32 != 0 || len(result.Output) == 0 {
		return nil
	}
	return append([]byte(nil), result.Output...)
}

type releaseWorkspaceCommitFields struct {
	leaseID            pgtype.UUID
	fencingToken       pgtype.Text
	fencingGeneration  pgtype.Int8
	baseVersionID      pgtype.UUID
	artifactDigest     pgtype.Text
	artifactSizeBytes  pgtype.Int8
	artifactMediaType  pgtype.Text
	artifactEncoding   pgtype.Text
	artifactEntryCount pgtype.Int4
	mountPath          pgtype.Text
}

func releaseWorkspaceFields(workspace *api.WorkerWorkspace) (releaseWorkspaceCommitFields, error) {
	if workspace == nil {
		return releaseWorkspaceCommitFields{}, nil
	}
	leaseID, err := parseRequiredWorkspaceUUID("workspace.write_lease_id", workspace.WriteLeaseID)
	if err != nil {
		return releaseWorkspaceCommitFields{}, err
	}
	fencingToken := strings.TrimSpace(workspace.WriteFencingToken)
	if fencingToken == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.write_fencing_token is required")
	}
	if workspace.FencingGeneration <= 0 {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.fencing_generation must be positive")
	}
	baseVersionID, err := parseOptionalWorkspaceUUID("workspace.base_version_id", workspace.BaseVersionID)
	if err != nil {
		return releaseWorkspaceCommitFields{}, err
	}
	if workspace.Artifact == nil {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact is required")
	}
	artifact := workspace.Artifact
	digest := strings.TrimSpace(artifact.Digest)
	if digest == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.digest is required")
	}
	mediaType := strings.TrimSpace(artifact.MediaType)
	if mediaType == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.media_type is required")
	}
	encoding := strings.TrimSpace(artifact.Encoding)
	if encoding == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.encoding is required")
	}
	if artifact.SizeBytes <= 0 {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.size_bytes must be positive")
	}
	if artifact.EntryCount < 0 {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.entry_count must be non-negative")
	}
	mountPath := strings.TrimSpace(workspace.MountPath)
	if mountPath == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.mount_path is required")
	}
	return releaseWorkspaceCommitFields{
		leaseID:            leaseID,
		fencingToken:       pgvalue.Text(fencingToken),
		fencingGeneration:  pgtype.Int8{Int64: workspace.FencingGeneration, Valid: workspace.FencingGeneration > 0},
		baseVersionID:      baseVersionID,
		artifactDigest:     pgvalue.Text(digest),
		artifactSizeBytes:  pgtype.Int8{Int64: artifact.SizeBytes, Valid: true},
		artifactMediaType:  pgvalue.Text(mediaType),
		artifactEncoding:   pgvalue.Text(encoding),
		artifactEntryCount: pgtype.Int4{Int32: artifact.EntryCount, Valid: true},
		mountPath:          pgvalue.Text(mountPath),
	}, nil
}

func terminalRunEventForFields(status db.RunStatus, exitCode pgtype.Int4, errorMessage pgtype.Text, result api.WorkerReleaseResult) (string, []byte, error) {
	switch status {
	case db.RunStatusSucceeded:
		code := int32(0)
		if exitCode.Valid {
			code = exitCode.Int32
		}
		payload, err := json.Marshal(runCompletedPayload{ExitCode: code})
		return "run.completed", payload, err
	case db.RunStatusFailed:
		if exitCode.Valid {
			payload, err := json.Marshal(runFailurePayload{
				FailureKind: "task_failed",
				Detail:      taskFailedDetailPayload{ExitCode: exitCode.Int32},
			})
			return "run.failed", payload, err
		}
		message := "worker execution failed"
		if errorMessage.Valid && strings.TrimSpace(errorMessage.String) != "" {
			message = errorMessage.String
		}
		if failureKind, ok := trustedWorkerFailureKind(result); ok {
			payload, err := json.Marshal(runFailurePayload{
				FailureKind: failureKind,
				Detail: workerFailureDetailPayload{
					Message:      message,
					LimitSeconds: result.LimitSeconds,
				},
			})
			return "run.failed", payload, err
		}
		payload, err := json.Marshal(runFailurePayload{
			FailureKind: "worker_failed",
			Detail:      workerMessagePayload{Message: message},
		})
		return "run.failed", payload, err
	case db.RunStatusCancelled:
		reason := "worker execution cancelled"
		if errorMessage.Valid && strings.TrimSpace(errorMessage.String) != "" {
			reason = errorMessage.String
		}
		payload, err := json.Marshal(runCancelledPayload{Reason: reason})
		return "run.cancelled", payload, err
	default:
		return "", nil, fmt.Errorf("run status %q is not terminal", status)
	}
}

func trustedWorkerFailureKind(result api.WorkerReleaseResult) (string, bool) {
	if result.FailureKind == nil {
		return "", false
	}
	switch *result.FailureKind {
	case "max_duration", "task_not_found", "duplicate_task_id", "missing_config", "task_parse_failed":
		return *result.FailureKind, true
	default:
		return "", false
	}
}
