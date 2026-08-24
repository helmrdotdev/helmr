package executor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"golang.org/x/sys/unix"
)

type RuntimeSubstrateResolver interface {
	Resolve(context.Context, string, substrate.Source) (substrate.Result, error)
}

type RuntimeSubstrateRegistrar interface {
	RegisterRuntimeSubstrate(context.Context, workerapi.RuntimeSubstrateRegisterRequest) (workerapi.RuntimeSubstrateRegisterResponse, error)
}

func runtimeSubstrateTopology(ctx context.Context, resolver RuntimeSubstrateResolver, imagePath string, mount workerapi.WorkspaceMount) (vm.RuntimeTopology, error) {
	if resolver == nil {
		return vm.RuntimeTopology{}, nil
	}
	result, err := resolver.Resolve(ctx, imagePath, substrate.Source{
		WorkspaceImageDigest:    mount.WorkspaceImage.Digest,
		WorkspaceImageMediaType: mount.WorkspaceImage.MediaType,
	})
	if err != nil {
		return vm.RuntimeTopology{}, err
	}
	return vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{
		Source: &runtimeSubstrateCacheSource{
			path: result.Path, digest: result.Digest, sizeBytes: result.SizeBytes,
		},
		Digest:    result.Digest,
		Format:    result.Format,
		Contract:  result.Contract,
		SizeBytes: result.SizeBytes,
	}}, nil
}

type runtimeSubstrateCacheSource struct {
	path      string
	digest    string
	sizeBytes int64
}

func (source *runtimeSubstrateCacheSource) MaterializeInto(
	ctx context.Context,
	directory string,
	name string,
	uid int,
	gid int,
) (_ string, retErr error) {
	if source == nil || strings.TrimSpace(source.path) == "" ||
		!strings.HasPrefix(source.digest, "sha256:") || source.sizeBytes <= 0 {
		return "", errors.New("runtime substrate cache source is incomplete")
	}
	if filepath.Base(name) != name || name == "." || name == "" {
		return "", errors.New("runtime substrate projection name is invalid")
	}
	input, err := os.Open(source.path)
	if err != nil {
		return "", fmt.Errorf("open runtime substrate cache source: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, input.Close()) }()
	info, err := input.Stat()
	if err != nil {
		return "", fmt.Errorf("stat runtime substrate cache source: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() != source.sizeBytes {
		return "", errors.New("runtime substrate cache source size changed")
	}
	destination := filepath.Join(directory, name)
	outputFD, err := unix.Open(
		destination,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return "", fmt.Errorf("create runtime substrate arena projection: %w", err)
	}
	output := os.NewFile(uintptr(outputFD), destination)
	keep := false
	defer func() {
		closeErr := output.Close()
		if !keep {
			_ = os.Remove(destination)
		}
		retErr = errors.Join(retErr, closeErr)
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(output, hash), &contextReader{ctx: ctx, reader: input})
	if err != nil {
		return "", fmt.Errorf("copy runtime substrate into arena: %w", err)
	}
	actualDigest := sha256sum.FormatDigest(hash.Sum(nil))
	if written != source.sizeBytes || actualDigest != source.digest {
		return "", errors.New("runtime substrate arena projection does not match source identity")
	}
	if err := output.Sync(); err != nil {
		return "", fmt.Errorf("sync runtime substrate arena projection: %w", err)
	}
	if err := output.Chown(uid, gid); err != nil {
		return "", fmt.Errorf("own runtime substrate arena projection: %w", err)
	}
	if err := output.Chmod(0o440); err != nil {
		return "", fmt.Errorf("seal runtime substrate arena projection: %w", err)
	}
	keep = true
	return destination, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(target []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(target)
}

func runtimeSubstrateDigest(topology vm.RuntimeTopology) string {
	if topology.Substrate == nil {
		return ""
	}
	return topology.Substrate.Digest
}

func runtimeSubstrateID(artifact *workerapi.RuntimeSubstrate) string {
	if artifact == nil {
		return ""
	}
	return artifact.ID
}

func registerRuntimeSubstrate(
	ctx context.Context,
	registrar RuntimeSubstrateRegistrar,
	deploymentDefinitionID string,
	substrate *vm.RuntimeSubstrate,
) (*workerapi.RuntimeSubstrate, error) {
	if substrate == nil {
		return nil, nil
	}
	if registrar == nil {
		return nil, errors.New("runtime substrate registrar is required")
	}
	if strings.TrimSpace(deploymentDefinitionID) == "" {
		return nil, errors.New("runtime substrate deployment_definition_id is required")
	}
	response, err := registrar.RegisterRuntimeSubstrate(
		ctx,
		workerapi.RuntimeSubstrateRegisterRequest{
			DeploymentDefinitionID: strings.TrimSpace(deploymentDefinitionID),
			SubstrateDigest:        strings.TrimSpace(substrate.Digest),
			Format:                 strings.TrimSpace(substrate.Format),
			Contract:               strings.TrimSpace(substrate.Contract),
			SizeBytes:              substrate.SizeBytes,
		},
	)
	if err != nil {
		return nil, err
	}
	registered := response.RuntimeSubstrate
	return &registered, nil
}
