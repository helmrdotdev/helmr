//go:build linux

package guestd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/buildkit"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
)

const imageBuildKitReadyTimeout = 30 * time.Second

func handleImageBuild(
	ctx context.Context,
	conn io.ReadWriter,
	bodyLen uint64,
) (returnErr error) {
	if bodyLen > imagebuild.MaxGuestRequestBodyBytes {
		return errors.New("image-build request body exceeds the v0 contract")
	}
	body := &io.LimitedReader{R: conn, N: int64(bodyLen)}
	raw, err := frameio.ReadMessageFrameBounded(body, imagebuild.RequestDocumentMaxBytes)
	if err != nil {
		return fmt.Errorf("read image-build request: %w", err)
	}
	request, err := imagebuild.ParseGuestRequest(raw)
	if err != nil {
		return err
	}
	if body.N < request.SourceArchiveSizeBytes+4 {
		return errors.New("image-build request body is truncated")
	}
	root, err := os.MkdirTemp("/var/lib/helmr/tmp", "image-build-")
	if err != nil {
		return fmt.Errorf("create image-build root: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, os.RemoveAll(root))
	}()
	sourceRoot := filepath.Join(root, "source")
	hash := sha256.New()
	source := &io.LimitedReader{
		R: io.TeeReader(body, hash),
		N: request.SourceArchiveSizeBytes,
	}
	stats, err := archive.ExtractTarWithStats(source, sourceRoot, archive.ExtractOptions{
		MaxBytes:   imagebuild.MaxSourceArchiveBytes,
		MaxEntries: imagebuild.MaxSourceArchiveEntries,
	})
	if err != nil {
		return fmt.Errorf("extract admitted image source: %w", err)
	}
	if source.N != 0 || sha256sum.DigestHash(hash) != request.SourceArchiveDigest ||
		stats.EntryCount != request.SourceArchiveEntries {
		return errors.New("image-build source archive does not match its descriptor")
	}
	paths, err := imageBuildSourcePaths(sourceRoot)
	if err != nil {
		return err
	}
	if !slices.Equal(paths, request.AdmittedPaths) ||
		imagebuild.PathSetDigest(paths) != request.AdmittedPathSetDigest {
		return errors.New("image-build source archive does not match its admitted path set")
	}
	envelopeRaw, err := frameio.ReadMessageFrameBounded(body, imagebuild.CredentialEnvelopeMaxBytes)
	if err != nil {
		return fmt.Errorf("read image-build credential envelope: %w", err)
	}
	defer clear(envelopeRaw)
	envelope, err := imagebuild.ParseCredentialEnvelope(envelopeRaw)
	if err != nil {
		return err
	}
	defer clearRegistryCredentials(envelope.RegistryCredentials)
	if err := imagebuild.MatchCredentialEnvelope(request, envelope); err != nil {
		return err
	}
	if body.N != 0 {
		return errors.New("image-build request contains trailing framed bytes")
	}

	daemon, closeDaemon, err := openImageBuildKit(ctx, root)
	if err != nil {
		return writeImageBuildFailure(conn, nil, err)
	}
	builder, clearProvider, err := buildkit.NewWithRegistryCredentials(
		daemon,
		filepath.Join(root, "output"),
		envelope.RegistryCredentials,
	)
	if err != nil {
		return writeImageBuildFailure(conn, envelope.RegistryCredentials, errors.Join(err, closeDaemon()))
	}
	defer clearProvider()
	artifact, err := builder.BuildImage(ctx, imagebuild.Request{
		RunID:       request.OperationID,
		WorkspaceID: request.AttemptID,
		Build:       request.Plan,
		Source: imagebuild.Source{
			ProjectRoot: sourceRoot,
			SHA:         request.BuildTreeDigest,
		},
		Cache: request.CacheBinding,
	})
	if err != nil {
		buildErr := errors.Join(err, closeDaemon())
		clearProvider()
		return writeImageBuildFailure(conn, envelope.RegistryCredentials, buildErr)
	}
	clearProvider()
	clearRegistryCredentials(envelope.RegistryCredentials)
	if err := closeDaemon(); err != nil {
		return writeImageBuildFailure(conn, nil, err)
	}
	digest, size, err := frameio.HashFile(artifact.ImageTarPath)
	if err != nil {
		return err
	}
	if size < 1 || size > imagebuild.MaxOCIArchiveBytes {
		return writeImageBuildFailure(conn, nil, errors.New("image-build OCI result exceeds the output contract"))
	}
	resultRaw, err := imagebuild.CanonicalGuestResult(imagebuild.GuestResult{
		ExecutionABI: imagebuild.ExecutionABI,
		Outcome:      imagebuild.GuestSucceeded,
		OCIDigest:    digest,
		OCISizeBytes: size,
	})
	if err != nil {
		return err
	}
	if err := frameio.WriteMessageFrame(conn, resultRaw); err != nil {
		return fmt.Errorf("write image-build result: %w", err)
	}
	image, err := os.Open(artifact.ImageTarPath)
	if err != nil {
		return err
	}
	_, copyErr := io.CopyN(conn, image, size)
	closeErr := image.Close()
	if copyErr != nil || closeErr != nil {
		return errors.Join(copyErr, closeErr)
	}
	return nil
}

func openImageBuildKit(
	ctx context.Context,
	root string,
) (buildkitSolver, func() error, error) {
	runRoot := filepath.Join(root, "run")
	stateRoot := filepath.Join(root, "buildkit")
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		return nil, nil, err
	}
	socket := filepath.Join(runRoot, "buildkitd.sock")
	daemonCtx, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(
		daemonCtx,
		"/usr/bin/buildkitd",
		"--addr", "unix://"+socket,
		"--root", stateRoot,
		"--oci-worker=true",
		"--containerd-worker=false",
		"--oci-worker-snapshotter=native",
		"--oci-worker-net=host",
		"--oci-max-parallelism=4",
	)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		cancel()
		return nil, nil, fmt.Errorf("start image-build BuildKit: %w", err)
	}
	closeDaemon := func() error {
		cancel()
		err := command.Wait()
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "signal: killed") {
			return err
		}
		return nil
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, imageBuildKitReadyTimeout)
	defer readyCancel()
	if err := waitForUnixSocket(readyCtx, socket); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("wait for image-build BuildKit socket: %w", err), closeDaemon())
	}
	client, err := bkclient.New(readyCtx, "unix://"+socket)
	if err != nil {
		return nil, nil, errors.Join(err, closeDaemon())
	}
	if err := client.Wait(readyCtx); err != nil {
		return nil, nil, errors.Join(err, client.Close(), closeDaemon())
	}
	closeAll := func() error {
		return errors.Join(client.Close(), closeDaemon())
	}
	return client, closeAll, nil
}

type buildkitSolver interface {
	Solve(context.Context, *llb.Definition, bkclient.SolveOpt, chan *bkclient.SolveStatus) (*bkclient.SolveResponse, error)
}

func waitForUnixSocket(ctx context.Context, path string) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(path)
		if err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
		}
	}
}

func imageBuildSourcePaths(root string) ([]imagebuild.SourcePath, error) {
	paths := make([]imagebuild.SourcePath, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		kind := imagebuild.SourcePathFile
		switch {
		case entry.Type()&os.ModeSymlink != 0:
			kind = imagebuild.SourcePathSymlink
		case entry.IsDir():
			kind = imagebuild.SourcePathDirectory
		case !entry.Type().IsRegular():
			return fmt.Errorf("image-build source path %q has unsupported type", relative)
		}
		paths = append(paths, imagebuild.SourcePath{
			Path: filepath.ToSlash(relative),
			Kind: kind,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect image-build source paths: %w", err)
	}
	slices.SortFunc(paths, func(left, right imagebuild.SourcePath) int {
		return strings.Compare(left.Path, right.Path)
	})
	return paths, nil
}

func writeImageBuildFailure(
	writer io.Writer,
	credentials []imagebuild.RegistryCredentialValue,
	cause error,
) error {
	message := redactImageBuildError(cause.Error(), credentials)
	reason := imagebuild.GuestFailureImage
	var quotaFailure *buildkit.OutputQuotaFailure
	if errors.As(cause, &quotaFailure) {
		reason = imagebuild.GuestFailureOutputQuota
	}
	if len(message) > 4096 {
		message = message[:4096]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	raw, err := imagebuild.CanonicalGuestResult(imagebuild.GuestResult{
		ExecutionABI:  imagebuild.ExecutionABI,
		Outcome:       imagebuild.GuestFailed,
		FailureReason: reason,
		Error:         message,
	})
	if err != nil {
		return err
	}
	return frameio.WriteMessageFrame(writer, raw)
}

func redactImageBuildError(
	message string,
	credentials []imagebuild.RegistryCredentialValue,
) string {
	for _, credential := range credentials {
		if len(credential.Password) != 0 {
			message = strings.ReplaceAll(message, string(credential.Password), "[REDACTED]")
		}
	}
	return message
}

func clearRegistryCredentials(credentials []imagebuild.RegistryCredentialValue) {
	for index := range credentials {
		clear(credentials[index].Password)
	}
}
