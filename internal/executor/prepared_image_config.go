package executor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/oci"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// readPreparedImageConfig moves the mounted-substrate guest's OCI verification
// to the worker. It validates the entire image, including bytes after tar EOF.
func readPreparedImageConfig(ctx context.Context, path string, artifact workerapi.CASObject) (_ *workspacev0.RuntimeImageConfig, retErr error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, input.Close()) }()
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || artifact.SizeBytes <= 0 || info.Size() != artifact.SizeBytes {
		return nil, errors.New("prepared image size does not match descriptor")
	}
	hash := sha256.New()
	body := &io.LimitedReader{R: io.TeeReader(&contextReader{ctx: ctx, reader: input}, hash), N: artifact.SizeBytes}
	config, err := oci.ReadConfig(body)
	if err != nil {
		return nil, fmt.Errorf("inspect prepared image: %w", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return nil, err
	}
	if body.N != 0 {
		return nil, errors.New("prepared image is truncated")
	}
	var extra [1]byte
	if n, err := input.Read(extra[:]); n != 0 || err != io.EOF {
		return nil, errors.New("prepared image exceeds descriptor size")
	}
	if sha256sum.FormatDigest(hash.Sum(nil)) != strings.TrimSpace(artifact.Digest) {
		return nil, errors.New("prepared image digest does not match descriptor")
	}
	return &workspacev0.RuntimeImageConfig{Env: config.Env, WorkingDir: config.WorkingDir, User: config.User, Entrypoint: config.Entrypoint, Cmd: config.Cmd}, nil
}
