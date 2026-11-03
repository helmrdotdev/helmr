// Package hostrelease downloads the Control Plane's own release to a worker
// host through Control Plane read grants and verifies it before anything is
// installed.
package hostrelease

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/release"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// ErrNoRelease means the Control Plane was built from source and names no
// release, so there is nothing to install or update to.
var ErrNoRelease = errors.New("the Control Plane names no release; install worker artifacts from the deployment's own build")

// maxSignedFileBytes bounds the index and its signature.
const maxSignedFileBytes = 1 << 20

// Source is the Control Plane as a release source.
type Source interface {
	Release(ctx context.Context) (*workerapi.Release, error)
	ReleaseRead(ctx context.Context, digest string) (workerapi.StorageReadGrant, error)
}

// Fetched is a verified release whose host assets are in Dir by name, a new
// directory under the one Fetch was given.
type Fetched struct {
	Index release.Index
	Dir   string
}

// Fetch downloads the Control Plane's release, verifies its signed index and
// writes each host asset into dir after checking it against the index.
func Fetch(ctx context.Context, source Source, client *http.Client, dir string) (Fetched, error) {
	published, err := source.Release(ctx)
	if err != nil {
		return Fetched{}, fmt.Errorf("read the Control Plane's release: %w", err)
	}
	if published == nil {
		return Fetched{}, ErrNoRelease
	}
	index, err := readSmall(ctx, source, client, published.Index)
	if err != nil {
		return Fetched{}, fmt.Errorf("download release index: %w", err)
	}
	signature, err := readSmall(ctx, source, client, published.Signature)
	if err != nil {
		return Fetched{}, fmt.Errorf("download release signature: %w", err)
	}
	verified, err := release.Verify(index, signature, published.Version)
	if err != nil {
		return Fetched{}, err
	}
	dir, err = os.MkdirTemp(dir, "release-"+verified.Version+"-")
	if err != nil {
		return Fetched{}, err
	}
	for _, name := range release.HostAssets {
		asset := verified.Assets[name]
		if err := download(ctx, source, client, workerapi.ReleaseObject{Digest: asset.Digest, SizeBytes: asset.SizeBytes}, filepath.Join(dir, name)); err != nil {
			_ = os.RemoveAll(dir)
			return Fetched{}, fmt.Errorf("download %s: %w", name, err)
		}
	}
	return Fetched{Index: verified, Dir: dir}, nil
}

func readSmall(ctx context.Context, source Source, client *http.Client, object workerapi.ReleaseObject) ([]byte, error) {
	if object.SizeBytes <= 0 || object.SizeBytes > maxSignedFileBytes {
		return nil, fmt.Errorf("release object %s has size %d", object.Digest, object.SizeBytes)
	}
	var buffer bytes.Buffer
	if err := copyObject(ctx, source, client, object, &buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func download(ctx context.Context, source Source, client *http.Client, object workerapi.ReleaseObject, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := copyObject(ctx, source, client, object, file); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

// copyObject writes one release object to out and fails unless its bytes
// match the digest and size the signed index names.
func copyObject(ctx context.Context, source Source, client *http.Client, object workerapi.ReleaseObject, out io.Writer) error {
	grant, err := source.ReleaseRead(ctx, object.Digest)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, grant.Get.Method, grant.Get.URL, nil)
	if err != nil {
		return err
	}
	for key, values := range grant.Get.Header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("storage returned %s", response.Status)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(response.Body, object.SizeBytes+1))
	if err != nil {
		return err
	}
	if written != object.SizeBytes || "sha256:"+hex.EncodeToString(hash.Sum(nil)) != object.Digest {
		return fmt.Errorf("downloaded bytes are not release object %s", object.Digest)
	}
	return nil
}
