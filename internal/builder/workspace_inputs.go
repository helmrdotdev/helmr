package builder

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sourceid"
)

const maxWorkspaceInputDocumentBytes = 1 << 20

type workspaceImageInput struct {
	DeclaredID string `json:"declaredId"`
	Path       string `json:"path"`
}

// ReadWorkspaceImageInputs turns producer-local OCI outputs into the neutral
// finalized-image contract. Paths and BuildKit details never enter the bundle.
func ReadWorkspaceImageInputs(
	ctx context.Context,
	path string,
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
	objects := make([]ObjectSource, len(inputs))
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
		artifact, err := inspectWorkspaceImageInput(input.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("workspace image %q: %w", input.DeclaredID, err)
		}
		images[index] = deployment.BundleWorkspaceImage{DeclaredID: input.DeclaredID, Artifact: artifact}
		objects[index] = ObjectSource{Digest: artifact.Digest, Path: input.Path}
	}
	return images, objects, nil
}

func inspectWorkspaceImageInput(path string) (deployment.BundleWorkspaceImageArtifact, error) {
	file, err := os.Open(path)
	if err != nil {
		return deployment.BundleWorkspaceImageArtifact{}, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > deployment.MaxDeploymentBundleObjectBytes {
		_ = file.Close()
		return deployment.BundleWorkspaceImageArtifact{}, errors.New("OCI output is not a bounded regular file")
	}
	metadata, inspectErr := oci.Inspect(file)
	closeErr := file.Close()
	if err := errors.Join(inspectErr, closeErr); err != nil {
		return deployment.BundleWorkspaceImageArtifact{}, err
	}
	if metadata.ManifestCount != 1 || metadata.Platform == nil ||
		metadata.Platform.OS != deployment.DeploymentBundleTargetOS ||
		metadata.Platform.Architecture != "amd64" {
		return deployment.BundleWorkspaceImageArtifact{}, errors.New("OCI output platform does not match linux/amd64")
	}
	file, err = os.Open(path)
	if err != nil {
		return deployment.BundleWorkspaceImageArtifact{}, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr = file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return deployment.BundleWorkspaceImageArtifact{}, err
	}
	return deployment.BundleWorkspaceImageArtifact{
		Architecture: deployment.ArchitectureX8664,
		Digest:       fmt.Sprintf("sha256:%x", hash.Sum(nil)),
		MediaType:    deployment.WorkspaceImageArtifactMediaType,
		SizeBytes:    info.Size(),
	}, nil
}
