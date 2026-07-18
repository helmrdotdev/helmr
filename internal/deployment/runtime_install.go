package deployment

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
)

const RuntimePolicyMediaType = "application/vnd.helmr.runtime-policy.v0+json"

func RunRuntimeInstall(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("runtime install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var storeURI, digest, output string
	flags.StringVar(&storeURI, "store", "", "managed runtime store URI")
	flags.StringVar(&digest, "digest", "", "runtime policy digest")
	flags.StringVar(&output, "output", "", "installed runtime policy path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("runtime install accepts no positional arguments")
	}
	if storeURI == "" || digest == "" || output == "" {
		return errors.New("runtime install requires --store, --digest, and --output")
	}
	if os.Geteuid() != 0 {
		return errors.New("runtime install must run as root")
	}
	catalog, err := LoadRuntimeCatalog()
	if err != nil {
		return fmt.Errorf("authenticate runtime catalog: %w", err)
	}
	store, err := cas.NewImmutableS3(ctx, storeURI)
	if err != nil {
		return fmt.Errorf("configure runtime store: %w", err)
	}
	return installRuntimePolicy(ctx, store, digest, output, catalog, 0, 0)
}

func installRuntimePolicy(
	ctx context.Context,
	store cas.Reader,
	digest,
	output string,
	catalog *RuntimeCatalog,
	ownerUID,
	ownerGID int,
) error {
	if ctx == nil {
		return errors.New("runtime policy install context is nil")
	}
	if store == nil {
		return errors.New("runtime policy store is required")
	}
	if _, err := RuntimeDigestBytes(digest); err != nil {
		return fmt.Errorf("runtime policy digest: %w", err)
	}
	if output == "" || !filepath.IsAbs(output) || filepath.Clean(output) != output {
		return errors.New("runtime policy output path must be canonical and absolute")
	}
	parent := filepath.Dir(output)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect runtime policy directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime policy parent is not a directory")
	}

	object, err := store.Stat(ctx, digest)
	if err != nil {
		return fmt.Errorf("stat runtime policy object: %w", err)
	}
	if object.Digest != digest ||
		object.MediaType != RuntimePolicyMediaType ||
		object.SizeBytes < 1 ||
		object.SizeBytes > maxRuntimePolicyBytes {
		return errors.New("runtime policy object does not match its descriptor")
	}
	body, err := store.Get(ctx, digest)
	if err != nil {
		return fmt.Errorf("open runtime policy object: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(body, maxRuntimePolicyBytes+1))
	closeErr := body.Close()
	if readErr != nil {
		return fmt.Errorf("read runtime policy object: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close runtime policy object: %w", closeErr)
	}
	sum := sha256.Sum256(raw)
	actualDigest := "sha256:" + hex.EncodeToString(sum[:])
	if actualDigest != digest ||
		int64(len(raw)) != object.SizeBytes {
		return errors.New("runtime policy bytes do not match the pinned object")
	}
	policy, err := ParseRuntimePolicy(raw)
	if err != nil {
		return err
	}
	if err := validateRuntimePolicyCatalog(policy, catalog); err != nil {
		return err
	}
	return installRuntimePolicyFile(parent, output, raw, ownerUID, ownerGID)
}

func installRuntimePolicyFile(
	parent,
	output string,
	raw []byte,
	ownerUID,
	ownerGID int,
) (returnErr error) {
	file, err := os.CreateTemp(parent, ".runtime-policy-*")
	if err != nil {
		return fmt.Errorf("create runtime policy temporary file: %w", err)
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			returnErr = errors.Join(returnErr, file.Close())
		}
		_ = os.Remove(tempPath)
	}()
	if err := file.Chown(ownerUID, ownerGID); err != nil {
		return fmt.Errorf("set runtime policy ownership: %w", err)
	}
	if err := file.Chmod(0o444); err != nil {
		return fmt.Errorf("set runtime policy mode: %w", err)
	}
	if _, err := file.Write(raw); err != nil {
		return fmt.Errorf("write runtime policy: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync runtime policy: %w", err)
	}
	if err := file.Close(); err != nil {
		file = nil
		return fmt.Errorf("close runtime policy: %w", err)
	}
	file = nil
	if err := os.Rename(tempPath, output); err != nil {
		return fmt.Errorf("install runtime policy: %w", err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("open runtime policy directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync runtime policy directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close runtime policy directory: %w", err)
	}
	return nil
}
