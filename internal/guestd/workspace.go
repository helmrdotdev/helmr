package guestd

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"strings"

	runv0 "github.com/helmrdotdev/helmr/internal/gen/helmr/run/v0"
)

func workspaceMountPath(request *runv0.RunTaskRequest) (string, error) {
	mountPath := "/workspace"
	if request.WorkspaceOverlay != nil && strings.TrimSpace(request.WorkspaceOverlay.MountPath) != "" {
		mountPath = request.WorkspaceOverlay.MountPath
	}
	if !strings.HasPrefix(mountPath, "/") {
		return "", fmt.Errorf("workspace mount path must be absolute: %q", mountPath)
	}
	for _, part := range strings.Split(mountPath, "/") {
		if part == ".." {
			return "", fmt.Errorf("workspace mount path must not contain parent components: %q", mountPath)
		}
	}
	clean := pathpkg.Clean(mountPath)
	if clean == "/" {
		return "", errors.New("workspace mount path must not be root")
	}
	if isReservedRuntimePath(clean) {
		return "", fmt.Errorf("workspace mount path %q conflicts with reserved runtime paths", clean)
	}
	return clean, nil
}

func workspaceRootForImage(imageRoot, mountPath string) (string, error) {
	root, err := confinedLayerPath(imageRoot, strings.TrimPrefix(mountPath, "/"))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return root, nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("workspace mount path is not a directory: %s", mountPath)
	}
	return root, nil
}
