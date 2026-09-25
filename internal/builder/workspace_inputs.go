package builder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/sourceid"
)

const maxWorkspaceInputDocumentBytes = 1 << 20

type workspaceImageInput struct {
	DeclaredID string `json:"declaredId"`
	Path       string `json:"path"`
}

// ReadWorkspaceImageInputs builds final-capacity disks inside the pinned Linux
// builder. work is owned by the build invocation until bundle publication ends.
func ReadWorkspaceImageInputs(ctx context.Context, path, work, mkfs, config string) ([]deployment.BundleWorkspaceImage, []ObjectSource, error) {
	return readWorkspaceImageInputs(ctx, path, func(source string) (deployment.BundleWorkspaceImageArtifact, string, error) {
		dir, err := os.MkdirTemp(work, "image-*")
		if err != nil {
			return deployment.BundleWorkspaceImageArtifact{}, "", err
		}
		target := filepath.Join(dir, "disk.filepack")
		artifact, err := buildWorkspaceDisk(ctx, source, target, dir, mkfs, config)
		if err != nil {
			_ = os.RemoveAll(dir)
			return deployment.BundleWorkspaceImageArtifact{}, "", err
		}
		return artifact, target, nil
	})
}

func readWorkspaceImageInputs(
	ctx context.Context,
	path string,
	inspect func(string) (deployment.BundleWorkspaceImageArtifact, string, error),
) ([]deployment.BundleWorkspaceImage, []ObjectSource, error) {
	if path == "" {
		return []deployment.BundleWorkspaceImage{}, []ObjectSource{}, nil
	}
	if ctx == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, nil, errors.New("workspace image input path is invalid")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) == 0 || len(raw) > maxWorkspaceInputDocumentBytes {
		return nil, nil, errors.New("workspace image input document size is invalid")
	}
	var inputs []workspaceImageInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inputs); err != nil {
		return nil, nil, fmt.Errorf("decode workspace image inputs: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("workspace image inputs contain trailing data")
	}
	if inputs == nil || len(inputs) > deployment.MaxDeploymentBundleWorkspaceImages {
		return nil, nil, errors.New("workspace image input count is invalid")
	}
	images := make([]deployment.BundleWorkspaceImage, len(inputs))
	objects := make([]ObjectSource, 0, len(inputs))
	objectDigests := make(map[string]struct{}, len(inputs))
	artifactsByPath := make(map[string]deployment.BundleWorkspaceImageArtifact, len(inputs))
	pathsByInput := make(map[string]string, len(inputs))
	for index, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !sourceid.Valid(input.DeclaredID) || !filepath.IsAbs(input.Path) || filepath.Clean(input.Path) != input.Path {
			return nil, nil, fmt.Errorf("workspace image input %d is invalid", index)
		}
		if index > 0 && inputs[index-1].DeclaredID >= input.DeclaredID {
			return nil, nil, errors.New("workspace image inputs must be unique and sorted")
		}
		artifact, inspected := artifactsByPath[input.Path]
		if !inspected {
			var output string
			artifact, output, err = inspect(input.Path)
			pathsByInput[input.Path] = output
			if err != nil {
				return nil, nil, fmt.Errorf("workspace image %q: %w", input.DeclaredID, err)
			}
			artifactsByPath[input.Path] = artifact
		}
		images[index] = deployment.BundleWorkspaceImage{DeclaredID: input.DeclaredID, Artifact: artifact}
		if _, exists := objectDigests[artifact.Digest]; !exists {
			objects = append(objects, ObjectSource{Digest: artifact.Digest, Path: pathsByInput[input.Path]})
			objectDigests[artifact.Digest] = struct{}{}
		}
	}
	return images, objects, nil
}
