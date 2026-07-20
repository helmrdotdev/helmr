//go:build linux

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildManagerProjectProducesSealedDrive(t *testing.T) {
	parent := t.TempDir()
	encoder := writeEncoderFixture(t, `#!/bin/sh
if [ "$1" = "-version" ]; then
	printf 'mksquashfs version 4.6.1 (2023/03/25)\n'
	exit 0
fi
cat >/dev/null
printf 'encoded-manager-project' >&3
`)
	project, err := buildManagerProject(
		context.Background(),
		parent,
		encoder,
		dependencyProjectionSource(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	leaseDirectory := project.snapshot.platform.directory.Name()
	wantBytes := []byte("encoded-manager-project")
	digest := sha256.Sum256(wantBytes)
	want := ManagerArtifact{
		Digest:    "sha256:" + hex.EncodeToString(digest[:]),
		MediaType: ManagerProjectMediaType,
		SizeBytes: int64(len(wantBytes)),
	}
	if project.descriptor != want {
		t.Fatalf("manager project descriptor = %#v, want %#v", project.descriptor, want)
	}
	entries, err := os.ReadDir(leaseDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != project.snapshot.platform.name {
		t.Fatalf("manager project lease entries = %#v", entries)
	}

	target := filepath.Join(t.TempDir(), "drives")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := project.LinkInto(
		target,
		"project.squashfs",
		os.Geteuid(),
		os.Getegid(),
	); err != nil {
		t.Fatal(err)
	}
	if err := project.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaseDirectory); !os.IsNotExist(err) {
		t.Fatalf("manager project lease after close: %v", err)
	}
	if err := project.LinkInto(
		target,
		"closed.squashfs",
		os.Geteuid(),
		os.Getegid(),
	); err == nil {
		t.Fatal("closed manager project remained linkable")
	}
}

func TestBuildManagerProjectCleansFailedEncoding(t *testing.T) {
	parent := t.TempDir()
	encoder := writeEncoderFixture(t, `#!/bin/sh
if [ "$1" = "-version" ]; then
	printf 'mksquashfs version 4.6.1 (2023/03/25)\n'
	exit 0
fi
printf partial >&3
exit 2
`)
	project, err := buildManagerProject(
		context.Background(),
		parent,
		encoder,
		dependencyProjectionSource(t),
	)
	if project != nil || err == nil || !strings.Contains(err.Error(), "encode") {
		t.Fatalf("manager project = %#v, error = %v", project, err)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("manager project failure left entries %#v", entries)
	}
}
