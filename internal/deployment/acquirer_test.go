package deployment

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
)

func TestManagerAcquirerPublishesAndThenResolvesWithoutAcquisitionTools(t *testing.T) {
	store := managerAcquirerStore(t)
	workDir := t.TempDir()
	selector := NewManagerSelector(
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
		ArchitectureX8664,
	)
	var downloads int
	downloader := managerDownloaderFunc(func(
		_ context.Context,
		selector ManagerSelector,
		destination *os.File,
	) (ManagerSource, error) {
		downloads++
		content := []byte("official distribution")
		if _, err := destination.Write(content); err != nil {
			return ManagerSource{}, err
		}
		if _, err := destination.Seek(0, io.SeekStart); err != nil {
			return ManagerSource{}, err
		}
		digest := sha256.Sum256(content)
		_, _, origin, err := managerDistribution(
			selector.PackageManager,
			selector.Architecture,
		)
		return ManagerSource{
			Digest:    "sha256:" + hex.EncodeToString(digest[:]),
			Origin:    origin,
			SizeBytes: int64(len(content)),
		}, err
	})
	normalizer := managerNormalizerFunc(func(
		_ context.Context,
		_ ManagerSelector,
		_ ManagerSource,
		_ *os.File,
		provisional *os.File,
	) (ManagerAcquireTerminal, error) {
		writer := tar.NewWriter(provisional)
		if err := writer.WriteHeader(&tar.Header{
			Name:     "bin/",
			Mode:     0555,
			Typeflag: tar.TypeDir,
		}); err != nil {
			return ManagerAcquireTerminal{}, err
		}
		if err := writer.Close(); err != nil {
			return ManagerAcquireTerminal{}, err
		}
		if _, err := provisional.Seek(0, io.SeekStart); err != nil {
			return ManagerAcquireTerminal{}, err
		}
		return ManagerAcquireTerminal{
			Status: ManagerAcquireStatusOK,
			Type:   managerAcquireTerminalType,
		}, nil
	})
	encoder := writeEncoderFixture(t, `#!/bin/sh
if [ "$1" = "-version" ]; then
	printf 'mksquashfs version 4.6.1 (2023/03/25)\n'
	exit 0
fi
printf 'encoded manager tree' >&3
`)
	acquirer := ManagerAcquirer{
		WorkDir:    workDir,
		Store:      store,
		Downloader: downloader,
		Normalizer: normalizer,
		Encoder:    encoder,
	}

	first, err := acquirer.Acquire(context.Background(), selector)
	if err != nil {
		t.Fatal(err)
	}
	if first.PackageManager != selector.PackageManager ||
		first.Architecture != selector.Architecture ||
		first.Tree.MediaType != ManagerTreeMediaType {
		t.Fatalf("capsule = %#v", first)
	}
	if downloads != 1 {
		t.Fatalf("downloads = %d, want 1", downloads)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("work directory retains acquisition scratch: %#v", entries)
	}

	resolved, err := (ManagerAcquirer{Store: store}).Acquire(
		context.Background(),
		selector,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != first {
		t.Fatalf("resolved = %#v, want %#v", resolved, first)
	}
	if downloads != 1 {
		t.Fatalf("downloads after resolve = %d, want 1", downloads)
	}
}

func TestManagerAcquirerDoesNotPublishUnsupportedDistribution(t *testing.T) {
	store := managerAcquirerStore(t)
	workDir := t.TempDir()
	selector := NewManagerSelector(
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
		ArchitectureAArch64,
	)
	downloader := managerDownloaderFunc(func(
		_ context.Context,
		selector ManagerSelector,
		destination *os.File,
	) (ManagerSource, error) {
		content := []byte("distribution")
		if _, err := destination.Write(content); err != nil {
			return ManagerSource{}, err
		}
		if _, err := destination.Seek(0, io.SeekStart); err != nil {
			return ManagerSource{}, err
		}
		digest := sha256.Sum256(content)
		_, _, origin, err := managerDistribution(
			selector.PackageManager,
			selector.Architecture,
		)
		return ManagerSource{
			Digest:    "sha256:" + hex.EncodeToString(digest[:]),
			Origin:    origin,
			SizeBytes: int64(len(content)),
		}, err
	})
	normalizer := managerNormalizerFunc(func(
		context.Context,
		ManagerSelector,
		ManagerSource,
		*os.File,
		*os.File,
	) (ManagerAcquireTerminal, error) {
		return ManagerAcquireTerminal{
			Status: ManagerAcquireStatusUnsupportedLayout,
			Type:   managerAcquireTerminalType,
		}, nil
	})
	encoder := writeEncoderFixture(t, `#!/bin/sh
printf 'mksquashfs version 4.6.1 (2023/03/25)\n'
`)

	_, err := (ManagerAcquirer{
		WorkDir:    workDir,
		Store:      store,
		Downloader: downloader,
		Normalizer: normalizer,
		Encoder:    encoder,
	}).Acquire(context.Background(), selector)
	if err == nil || !strings.Contains(err.Error(), ErrManagerProtocolUnsupported.Error()) {
		t.Fatalf("error = %v", err)
	}
	if _, err := store.Resolve(context.Background(), selector); err != ErrManagerNotClaimed {
		t.Fatalf("Resolve error = %v, want %v", err, ErrManagerNotClaimed)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("work directory retains failed acquisition scratch: %#v", entries)
	}
}

type managerDownloaderFunc func(
	context.Context,
	ManagerSelector,
	*os.File,
) (ManagerSource, error)

func (download managerDownloaderFunc) Download(
	ctx context.Context,
	selector ManagerSelector,
	destination *os.File,
) (ManagerSource, error) {
	return download(ctx, selector, destination)
}

type managerNormalizerFunc func(
	context.Context,
	ManagerSelector,
	ManagerSource,
	*os.File,
	*os.File,
) (ManagerAcquireTerminal, error)

func (normalize managerNormalizerFunc) Normalize(
	ctx context.Context,
	selector ManagerSelector,
	source ManagerSource,
	archive *os.File,
	provisional *os.File,
) (ManagerAcquireTerminal, error) {
	return normalize(ctx, selector, source, archive, provisional)
}

func managerAcquirerStore(t *testing.T) *ManagerStore {
	t.Helper()
	store, err := newManagerStore(
		newMemoryManagerDocuments(),
		newTestManagerTrees(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
