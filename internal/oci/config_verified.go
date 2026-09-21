package oci

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

// ReadVerifiedConfig verifies the full image digest, including bytes after tar EOF.
func ReadVerifiedConfig(ctx context.Context, path string, digest string, sizeBytes int64) (_ RuntimeConfig, retErr error) {
	input, err := os.Open(path)
	if err != nil {
		return RuntimeConfig{}, err
	}
	defer func() { retErr = errors.Join(retErr, input.Close()) }()
	info, err := input.Stat()
	if err != nil {
		return RuntimeConfig{}, err
	}
	if !info.Mode().IsRegular() || sizeBytes <= 0 || info.Size() != sizeBytes {
		return RuntimeConfig{}, errors.New("OCI image size does not match descriptor")
	}
	hash := sha256.New()
	body := &io.LimitedReader{R: io.TeeReader(&configReader{ctx: ctx, reader: input}, hash), N: sizeBytes}
	config, err := ReadConfig(body)
	if err != nil {
		return RuntimeConfig{}, fmt.Errorf("inspect OCI image: %w", err)
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return RuntimeConfig{}, err
	}
	if body.N != 0 {
		return RuntimeConfig{}, errors.New("OCI image is truncated")
	}
	var extra [1]byte
	if n, err := input.Read(extra[:]); n != 0 || err != io.EOF {
		return RuntimeConfig{}, errors.New("OCI image exceeds descriptor size")
	}
	if sha256sum.FormatDigest(hash.Sum(nil)) != strings.TrimSpace(digest) {
		return RuntimeConfig{}, errors.New("OCI image digest does not match descriptor")
	}
	return config, nil
}

type configReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *configReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
