package main

import (
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
	indexObject, err := publishReleaseFile(ctx, store, indexPath, "")
	if err != nil {
		return err
	}
	signatureObject, err := publishReleaseFile(ctx, store, signaturePath, "")
	if err != nil {
		return err
	}
	for _, name := range release.HostAssets {
		if _, err := publishReleaseFile(ctx, store, filepath.Join(assets, name), verified.Assets[name].Digest); err != nil {
			return fmt.Errorf("publish %s: %w", name, err)
		}
	}
	return withWorkerDatabase(ctx, func(pool *pgxpool.Pool) error {
		return workergroup.RecordRelease(ctx, pool, verified, indexObject, signatureObject)
	})
}

// publishReleaseFile puts one file in the platform store and returns its
// object. A non-empty want is the digest the signed index lists for it.
func publishReleaseFile(ctx context.Context, store *cass3.ImmutableStore, path, want string) (workergroup.ReleaseObject, error) {
	file, err := os.Open(path)
	if err != nil {
		return workergroup.ReleaseObject{}, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return workergroup.ReleaseObject{}, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if want != "" && digest != want {
		return workergroup.ReleaseObject{}, fmt.Errorf("%s is %s, but the signed index lists %s", path, digest, want)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return workergroup.ReleaseObject{}, err
	}
	if _, err := store.Publish(ctx, cas.Descriptor{Digest: digest, SizeBytes: size, MediaType: releaseObjectMediaType}, file); err != nil {
		return workergroup.ReleaseObject{}, err
	}
	return workergroup.ReleaseObject{Digest: digest, SizeBytes: size}, nil
}
