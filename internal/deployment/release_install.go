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

const BuildPolicyMediaType = "application/vnd.helmr.build-policy.v0+json"

func RunReleaseInstall(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("release install", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var storeURI, digest, output string
	flags.StringVar(&storeURI, "store", "", "release store URI")
	flags.StringVar(&digest, "digest", "", "build policy digest")
	flags.StringVar(&output, "output", "", "installed build policy path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("release install accepts no positional arguments")
	}
	if storeURI == "" || digest == "" || output == "" {
		return errors.New("release install requires --store, --digest, and --output")
	}
	if os.Geteuid() != 0 {
		return errors.New("release install must run as root")
	}
	store, err := cas.NewImmutableS3(ctx, storeURI)
	if err != nil {
		return fmt.Errorf("configure release store: %w", err)
	}
	return installBuildPolicy(ctx, store, digest, output, 0, 0)
}

func installBuildPolicy(
	ctx context.Context,
	store cas.Reader,
	digest,
	output string,
	ownerUID,
	ownerGID int,
) error {
	if ctx == nil {
		return errors.New("build policy install context is nil")
	}
	if store == nil {
		return errors.New("build policy store is required")
	}
	if _, err := RuntimeDigestBytes(digest); err != nil {
		return fmt.Errorf("build policy digest: %w", err)
	}
	if output == "" || !filepath.IsAbs(output) || filepath.Clean(output) != output {
		return errors.New("build policy output path must be canonical and absolute")
	}
	parent := filepath.Dir(output)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("inspect build policy directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("build policy parent is not a directory")
	}

	object, err := store.Stat(ctx, digest)
	if err != nil {
		return fmt.Errorf("stat build policy object: %w", err)
	}
	if object.Digest != digest ||
		object.MediaType != BuildPolicyMediaType ||
		object.SizeBytes < 1 ||
		object.SizeBytes > maxBuildPolicyBytes {
		return errors.New("build policy object does not match its descriptor")
	}
	body, err := store.Get(ctx, digest)
	if err != nil {
		return fmt.Errorf("open build policy object: %w", err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(body, maxBuildPolicyBytes+1))
	closeErr := body.Close()
	if readErr != nil {
		return fmt.Errorf("read build policy object: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close build policy object: %w", closeErr)
	}
	sum := sha256.Sum256(raw)
	actualDigest := "sha256:" + hex.EncodeToString(sum[:])
	if actualDigest != digest ||
		int64(len(raw)) != object.SizeBytes {
		return errors.New("build policy bytes do not match the pinned object")
	}
	if _, err := ParseBuildPolicy(raw); err != nil {
		return err
	}
	return installBuildPolicyFile(parent, output, raw, ownerUID, ownerGID)
}

func installBuildPolicyFile(
	parent,
	output string,
	raw []byte,
	ownerUID,
	ownerGID int,
) (returnErr error) {
	file, err := os.CreateTemp(parent, ".build-policy-*")
	if err != nil {
		return fmt.Errorf("create build policy temporary file: %w", err)
	}
	tempPath := file.Name()
	defer func() {
		if file != nil {
			returnErr = errors.Join(returnErr, file.Close())
		}
		_ = os.Remove(tempPath)
	}()
	if err := file.Chown(ownerUID, ownerGID); err != nil {
		return fmt.Errorf("set build policy ownership: %w", err)
	}
	if err := file.Chmod(0o444); err != nil {
		return fmt.Errorf("set build policy mode: %w", err)
	}
	if _, err := file.Write(raw); err != nil {
		return fmt.Errorf("write build policy: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync build policy: %w", err)
	}
	if err := file.Close(); err != nil {
		file = nil
		return fmt.Errorf("close build policy: %w", err)
	}
	file = nil
	if err := os.Rename(tempPath, output); err != nil {
		return fmt.Errorf("install build policy: %w", err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		return fmt.Errorf("open build policy directory: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync build policy directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close build policy directory: %w", err)
	}
	return nil
}
