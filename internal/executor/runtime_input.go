package executor

import (
	"errors"
	"fmt"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/checkout"
	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func runTaskRequest(request Request) (*runv0.RunTaskRequest, error) {
	task := request.Run.Bundle.GetTask()
	if task == nil {
		return nil, errors.New("runtime task spec is required")
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
	sourceProto, err := runTaskSourceProto(request.Run.Workspace)
	if err != nil {
		return nil, err
	}
	return &runv0.RunTaskRequest{
		TaskId:      request.Run.TaskID,
		ModulePath:  modulePath,
		Cwd:         cwd,
		Secrets:     secrets,
		RunId:       request.Run.RunID,
		PayloadJson: string(request.Run.Payload),
		Source:      sourceProto,
		Workspace:   workspaceProto,
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

func runTaskWorkspaceProto(mountPath string, artifact checkout.WorkspaceArtifact) (*runv0.RunTaskWorkspace, error) {
	mountPath = strings.TrimSpace(mountPath)
	if mountPath == "" {
		mountPath = "/workspace"
	}
	digest := strings.TrimSpace(artifact.Digest)
	if digest == "" {
		return nil, errors.New("workspace artifact digest is required")
	}
	mediaType := strings.TrimSpace(artifact.MediaType)
	if mediaType != checkout.WorkspaceArtifactMediaType {
		return nil, fmt.Errorf("unsupported workspace artifact media_type %q", artifact.MediaType)
	}
	encoding := strings.TrimSpace(artifact.Encoding)
	if encoding != checkout.WorkspaceArtifactEncoding {
		return nil, fmt.Errorf("unsupported workspace artifact encoding %q", artifact.Encoding)
	}
	volumeKind := strings.TrimSpace(artifact.VolumeKind)
	if volumeKind != checkout.WorkspaceVolumeKind {
		return nil, fmt.Errorf("unsupported workspace volume_kind %q", artifact.VolumeKind)
	}
	projectPath := mountPath
	projectSubpath := strings.Trim(strings.TrimSpace(artifact.ProjectSubpath), "/")
	if projectSubpath != "" {
		for _, part := range strings.Split(projectSubpath, "/") {
			if part == "." || part == ".." || part == "" {
				return nil, fmt.Errorf("workspace artifact project_subpath is invalid: %q", artifact.ProjectSubpath)
			}
		}
		projectPath = pathpkg.Join(mountPath, projectSubpath)
	}
	return &runv0.RunTaskWorkspace{
		Path:        mountPath,
		ProjectPath: projectPath,
		Artifact: &runv0.WorkspaceArtifact{
			Digest:    digest,
			MediaType: mediaType,
			Encoding:  encoding,
		},
		VolumeKind: volumeKind,
		Writable:   true,
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
