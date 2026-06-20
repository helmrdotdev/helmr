package executor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func runTaskRequest(request Request) (*runv0.RunTaskRequest, error) {
	task := request.Run.Bundle.GetTask()
	if task == nil {
		return nil, errors.New("runtime task spec is required")
	}
	taskSessionID := strings.TrimSpace(request.Run.TaskSessionID)
	if taskSessionID == "" {
		return nil, errors.New("runtime task_session_id is required")
	}
	modulePath := strings.TrimSpace(task.ModulePath)
	if modulePath == "" {
		return nil, errors.New("runtime task module_path is required")
	}
	workspacePath := workspaceMountPath(request.Run.Bundle)
	workspaceProto, err := runTaskWorkspaceProto(workspacePath, request.Workspace)
	if err != nil {
		return nil, err
	}
	cwd := workspaceProto.ProjectPath
	secrets, err := runtimeSecrets(task.Secrets, request.Run.Secrets)
	if err != nil {
		return nil, err
	}
	return &runv0.RunTaskRequest{
		TaskId:          request.Run.TaskID,
		ModulePath:      modulePath,
		Cwd:             cwd,
		Secrets:         secrets,
		RunId:           request.Run.RunID,
		PayloadJson:     string(request.Run.Payload),
		Workspace:       workspaceProto,
		AttemptId:       request.Run.AttemptID,
		AttemptNumber:   uint32(request.Run.AttemptNumber),
		RunLeaseId:      request.Run.RunLeaseID,
		SnapshotVersion: uint64(request.Run.SnapshotVersion),
		TaskSessionId:   taskSessionID,
		Trace: &runv0.TraceContext{
			TraceId:     request.Run.Trace.TraceID,
			SpanId:      request.Run.Trace.SpanID,
			Traceparent: request.Run.Trace.Traceparent,
		},
	}, nil
}

func runtimeSourceRoot(source builder.Source) (string, error) {
	if strings.TrimSpace(source.CheckoutRoot) == "" {
		return source.ProjectRoot, nil
	}
	rel, err := filepath.Rel(source.CheckoutRoot, source.ProjectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve source subpath: %w", err)
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", errors.New("source project root must be inside checkout root")
	}
	return source.ProjectRoot, nil
}

func workspaceMountPath(bundle *bundlev0.Bundle) string {
	if bundle != nil && bundle.Sandbox != nil && bundle.Sandbox.Workspace != nil {
		if path := strings.TrimSpace(bundle.Sandbox.Workspace.MountPath); path != "" {
			return path
		}
	}
	return "/workspace"
}

func runTaskWorkspaceProto(mountPath string, artifact workspace.WorkspaceArtifact) (*runv0.RunTaskWorkspace, error) {
	mountPath = strings.TrimSpace(mountPath)
	if mountPath == "" {
		mountPath = "/workspace"
	}
	digest := strings.TrimSpace(artifact.Digest)
	if digest == "" {
		return nil, errors.New("workspace artifact digest is required")
	}
	mediaType := strings.TrimSpace(artifact.MediaType)
	if mediaType != workspace.ArtifactMediaType {
		return nil, fmt.Errorf("unsupported workspace artifact media_type %q", artifact.MediaType)
	}
	encoding := strings.TrimSpace(artifact.Encoding)
	if encoding != workspace.ArtifactEncoding {
		return nil, fmt.Errorf("unsupported workspace artifact encoding %q", artifact.Encoding)
	}
	if artifact.SizeBytes <= 0 {
		return nil, errors.New("workspace artifact size_bytes is required")
	}
	if artifact.SizeBytes > workspace.MaxArtifactArchiveBytes {
		return nil, fmt.Errorf("workspace artifact size_bytes %d exceeds max %d", artifact.SizeBytes, workspace.MaxArtifactArchiveBytes)
	}
	if artifact.EntryCount > workspace.MaxArtifactEntries {
		return nil, fmt.Errorf("workspace artifact entry_count %d exceeds max %d", artifact.EntryCount, workspace.MaxArtifactEntries)
	}
	return &runv0.RunTaskWorkspace{
		Path:        mountPath,
		ProjectPath: mountPath,
		Artifact: &runv0.WorkspaceArtifact{
			Digest:     digest,
			MediaType:  mediaType,
			Encoding:   encoding,
			SizeBytes:  uint64(artifact.SizeBytes),
			EntryCount: uint32(artifact.EntryCount),
		},
		Writable: true,
	}, nil
}

func runtimeSecrets(placements []*bundlev0.SecretPlacement, values api.ResolvedSecrets) ([]*runv0.SecretInject, error) {
	if len(placements) == 0 {
		return nil, nil
	}
	secrets := make([]*runv0.SecretInject, 0, len(placements))
	for _, placement := range placements {
		if placement == nil || strings.TrimSpace(placement.Name) == "" {
			return nil, errors.New("runtime secret placement name is required")
		}
		valueBytes, ok := values[placement.Name]
		if !ok {
			return nil, fmt.Errorf("runtime secret %q is required", placement.Name)
		}
		secrets = append(secrets, &runv0.SecretInject{
			Name:       placement.Name,
			Placement:  runtimePlacement(placement.Placement),
			ValueBytes: append([]byte(nil), valueBytes...),
		})
	}
	return secrets, nil
}

func runtimePlacement(placement *bundlev0.Placement) *runv0.Placement {
	if placement == nil {
		return nil
	}
	switch value := placement.Kind.(type) {
	case *bundlev0.Placement_Env:
		return &runv0.Placement{Kind: &runv0.Placement_Env{Env: &runv0.EnvPlacement{Name: value.Env.GetName()}}}
	case *bundlev0.Placement_File:
		return &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{
			Path:  value.File.GetPath(),
			Mode:  value.File.Mode,
			Owner: value.File.Owner,
		}}}
	case *bundlev0.Placement_Dir:
		return &runv0.Placement{Kind: &runv0.Placement_Dir{Dir: &runv0.DirPlacement{
			Path:  value.Dir.GetPath(),
			Mode:  value.Dir.Mode,
			Owner: value.Dir.Owner,
		}}}
	default:
		return nil
	}
}
