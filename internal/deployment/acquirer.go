package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
)

const managerPublishTTL = 5 * time.Minute

type managerDownloader interface {
	Download(context.Context, ManagerSelector, *os.File) (ManagerSource, error)
}

type ManagerAcquirer struct {
	WorkDir    string
	Store      *ManagerStore
	Downloader managerDownloader
	Normalizer ManagerNormalizer
	Encoder    string
}

func (acquirer ManagerAcquirer) Acquire(
	ctx context.Context,
	selector ManagerSelector,
) (capsule ManagerCapsule, returnErr error) {
	if ctx == nil {
		return ManagerCapsule{}, errors.New("manager acquisition context is nil")
	}
	if acquirer.Store == nil {
		return ManagerCapsule{}, errors.New("manager store is required")
	}
	if err := validateManagerSelector(selector); err != nil {
		return ManagerCapsule{}, err
	}

	capsule, err := acquirer.Store.Resolve(ctx, selector)
	if err == nil {
		return capsule, nil
	}
	if !errors.Is(err, ErrManagerNotClaimed) {
		return ManagerCapsule{}, err
	}
	if acquirer.Downloader == nil {
		return ManagerCapsule{}, errors.New("manager downloader is required")
	}
	if acquirer.Normalizer == nil {
		return ManagerCapsule{}, errors.New("manager normalizer is required")
	}
	if acquirer.WorkDir == "" ||
		!filepath.IsAbs(acquirer.WorkDir) ||
		filepath.Clean(acquirer.WorkDir) != acquirer.WorkDir {
		return ManagerCapsule{}, errors.New("manager acquisition work directory must be an absolute clean path")
	}
	if err := validateProgramEncoder(acquirer.Encoder); err != nil {
		return ManagerCapsule{}, err
	}

	scratch, err := os.MkdirTemp(acquirer.WorkDir, "manager-*")
	if err != nil {
		return ManagerCapsule{}, fmt.Errorf("create manager acquisition scratch: %w", err)
	}
	if err := os.Chmod(scratch, 0700); err != nil {
		_ = os.RemoveAll(scratch)
		return ManagerCapsule{}, fmt.Errorf("secure manager acquisition scratch: %w", err)
	}
	defer func() {
		if err := removeManagerScratch(scratch); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()

	archive, err := createManagerScratchFile(scratch, "source")
	if err != nil {
		return ManagerCapsule{}, err
	}
	defer archive.Close()
	source, err := acquirer.Downloader.Download(ctx, selector, archive)
	if err != nil {
		return ManagerCapsule{}, err
	}

	provisional, err := createManagerScratchFile(scratch, "tree.tar")
	if err != nil {
		return ManagerCapsule{}, err
	}
	defer provisional.Close()
	terminal, err := acquirer.Normalizer.Normalize(
		ctx,
		selector,
		source,
		archive,
		provisional,
	)
	if err != nil {
		return ManagerCapsule{}, err
	}
	switch terminal.Status {
	case ManagerAcquireStatusOK:
	case ManagerAcquireStatusUnsupportedLayout, ManagerAcquireStatusLimitExceeded:
		return ManagerCapsule{}, fmt.Errorf(
			"%w: acquisition guest returned %s",
			ErrManagerProtocolUnsupported,
			terminal.Status,
		)
	case ManagerAcquireStatusInternalError:
		return ManagerCapsule{}, vm.NewGuestError(
			errors.New("manager acquisition guest returned internal_error"),
		)
	default:
		return ManagerCapsule{}, vm.NewGuestError(
			fmt.Errorf("manager acquisition guest returned unknown status %q", terminal.Status),
		)
	}

	tree, err := createManagerScratchFile(scratch, "tree.squashfs")
	if err != nil {
		return ManagerCapsule{}, err
	}
	defer tree.Close()
	treeDescriptor, err := encodeManagerTree(
		ctx,
		acquirer.Encoder,
		provisional,
		tree,
	)
	if err != nil {
		return ManagerCapsule{}, err
	}
	entrypointKind, entrypointPath, _, err := managerDistribution(
		selector.PackageManager,
		selector.Architecture,
	)
	if err != nil {
		return ManagerCapsule{}, err
	}
	candidate := ManagerCapsule{
		Architecture: selector.Architecture,
		Entrypoint: ManagerEntrypoint{
			Kind: entrypointKind,
			Path: entrypointPath,
		},
		FormatVersion:  ManagerCapsuleFormatVersion,
		PackageManager: selector.PackageManager,
		Source:         source,
		Tree:           treeDescriptor,
	}
	if err := validateManagerCapsule(candidate); err != nil {
		return ManagerCapsule{}, err
	}

	publishCtx, cancelPublish := context.WithTimeout(
		context.WithoutCancel(ctx),
		managerPublishTTL,
	)
	defer cancelPublish()
	return acquirer.Store.Publish(publishCtx, selector, candidate, tree)
}

func encodeManagerTree(
	ctx context.Context,
	encoder string,
	provisional *os.File,
	destination *os.File,
) (ArtifactDescriptor, error) {
	if _, err := provisional.Seek(0, io.SeekStart); err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("rewind manager provisional tree: %w", err)
	}
	if err := encodeSquashFS(ctx, encoder, provisional, destination); err != nil {
		return ArtifactDescriptor{}, err
	}
	if err := destination.Sync(); err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("sync manager Capsule tree: %w", err)
	}
	info, err := destination.Stat()
	if err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("inspect manager Capsule tree: %w", err)
	}
	if info.Size() < 1 || info.Size() > maxManagerCapsuleTreeBytes {
		return ArtifactDescriptor{}, fmt.Errorf(
			"%w: encoded manager tree size is outside [1,%d]",
			ErrManagerProtocolUnsupported,
			maxManagerCapsuleTreeBytes,
		)
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("rewind manager Capsule tree: %w", err)
	}
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(destination, info.Size()+1))
	if err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("hash manager Capsule tree: %w", err)
	}
	if written != info.Size() {
		return ArtifactDescriptor{}, errors.New("manager Capsule tree changed while hashing")
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		return ArtifactDescriptor{}, fmt.Errorf("rewind manager Capsule tree for publication: %w", err)
	}
	return ArtifactDescriptor{
		Digest:    "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		MediaType: ManagerTreeMediaType,
		SizeBytes: info.Size(),
	}, nil
}

func createManagerScratchFile(root, name string) (*os.File, error) {
	file, err := os.OpenFile(
		filepath.Join(root, name),
		os.O_RDWR|os.O_CREATE|os.O_EXCL,
		0600,
	)
	if err != nil {
		return nil, fmt.Errorf("create manager acquisition %s: %w", name, err)
	}
	return file, nil
}

func removeManagerScratch(root string) error {
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove manager acquisition scratch: %w", err)
	}
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		if err == nil {
			return errors.New("manager acquisition scratch remains after cleanup")
		}
		return fmt.Errorf("verify manager acquisition scratch removal: %w", err)
	}
	return nil
}
