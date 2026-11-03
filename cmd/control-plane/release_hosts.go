package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/release"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgxpool"
)

// releaseObjectMediaType is how the platform store keeps a release's files;
// hosts address them by digest only.
const releaseObjectMediaType = "application/octet-stream"

// runReleaseHostsCommand publishes a release for the deployment's worker
// hosts: it verifies the signed index, puts the index, its signature and every
// host asset the index lists in the platform store, and records the release so
// hosts install and update from it through Control Plane read grants.
func runReleaseHostsCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("release publish-hosts", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var storeURI, version, indexPath, signaturePath, assets string
	flags.StringVar(&storeURI, "store", "", "Platform Artifact store URI")
	flags.StringVar(&version, "version", "", "release version, for example v1.2.3")
	flags.StringVar(&indexPath, "index", "", "release-index.json")
	flags.StringVar(&signaturePath, "signature", "", "release-index.sigstore.json")
	flags.StringVar(&assets, "assets", "", "directory holding the release's host assets by name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || storeURI == "" || version == "" || indexPath == "" || signaturePath == "" || assets == "" {
		return errors.New("release publish-hosts requires --store, --version, --index, --signature and --assets")
	}
	index, err := os.ReadFile(indexPath)
	if err != nil {
		return err
	}
	signature, err := os.ReadFile(signaturePath)
	if err != nil {
		return err
	}
	verified, err := release.Verify(index, signature, version)
	if err != nil {
		return err
	}
	store, err := cass3.NewImmutable(ctx, storeURI)
	if err != nil {
		return fmt.Errorf("configure platform artifact store: %w", err)
	}
	stage, err := os.MkdirTemp("", "helmr-release-hosts-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	// The index and signature published are exactly the bytes verified above.
	indexObject, err := publishReleaseBytes(ctx, store, stage, "index", index)
	if err != nil {
		return err
	}
	signatureObject, err := publishReleaseBytes(ctx, store, stage, "signature", signature)
	if err != nil {
		return err
	}
	for _, name := range release.HostAssets {
		asset := verified.Assets[name]
		if err := publishReleaseAsset(ctx, store, stage, filepath.Join(assets, name), asset); err != nil {
			return fmt.Errorf("publish %s: %w", name, err)
		}
	}
	return withWorkerDatabase(ctx, func(pool *pgxpool.Pool) error {
		return workergroup.RecordRelease(ctx, pool, verified, indexObject, signatureObject)
	})
}

func publishReleaseBytes(ctx context.Context, store *cass3.ImmutableStore, stage, name string, data []byte) (workergroup.ReleaseObject, error) {
	sum := sha256.Sum256(data)
	object := release.Asset{Digest: "sha256:" + hex.EncodeToString(sum[:]), SizeBytes: int64(len(data))}
	if err := publishStaged(ctx, store, stage, name, bytes.NewReader(data), object); err != nil {
		return workergroup.ReleaseObject{}, err
	}
	return workergroup.ReleaseObject(object), nil
}

func publishReleaseAsset(ctx context.Context, store *cass3.ImmutableStore, stage, path string, signed release.Asset) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return publishStaged(ctx, store, stage, filepath.Base(path), file, signed)
}

// publishStaged copies source into a private file, checks it against the
// signed descriptor, seals it read-only and publishes that copy, so what
// reaches the store is exactly what was checked.
func publishStaged(ctx context.Context, store *cass3.ImmutableStore, stage, name string, source io.Reader, signed release.Asset) error {
	path := filepath.Join(stage, name)
	staged, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(staged, hash), io.LimitReader(source, signed.SizeBytes+1))
	if closeErr := staged.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if digest != signed.Digest || size != signed.SizeBytes {
		return fmt.Errorf("%s is %s (%d bytes), but the signed index lists %s (%d bytes)", name, digest, size, signed.Digest, signed.SizeBytes)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		return err
	}
	sealed, err := os.Open(path)
	if err != nil {
		return err
	}
	defer sealed.Close()
	_, err = store.Publish(ctx, cas.Descriptor{Digest: digest, SizeBytes: size, MediaType: releaseObjectMediaType}, sealed)
	return err
}
