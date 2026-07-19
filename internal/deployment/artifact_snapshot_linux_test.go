//go:build linux

package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArtifactSnapshotOwnsPrivateNamedBytes(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "encoder-output")
	if err := os.WriteFile(sourcePath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	snapshot, err := snapshotArtifact(
		context.Background(),
		directory,
		codeArtifact,
		artifactSnapshotDescriptor(testProgramDescriptor(codeArtifact, []byte("original"))),
		source,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	leaseDirectory := snapshot.platform.directory.Name()
	snapshotPath := filepath.Join(leaseDirectory, snapshot.platform.name)

	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, []byte("replaced"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, sourcePath); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("snapshot directory entries = %#v", entries)
	}
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	if !names["encoder-output"] ||
		!names[filepath.Base(leaseDirectory)] {
		t.Fatalf("snapshot directory entries = %#v", entries)
	}

	reader, err := snapshot.uploadReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("snapshot bytes = %q, want original", got)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("closed snapshot stat error = %v", err)
	}
	if _, err := os.Stat(leaseDirectory); !os.IsNotExist(err) {
		t.Fatalf("closed snapshot lease stat error = %v", err)
	}
}

func TestArtifactSnapshotDescriptorsAreReadOnlyAndIndependent(t *testing.T) {
	snapshot := newTestArtifactSnapshot(t, []byte("abcdef"))
	verifier, err := snapshot.verifierFile()
	if err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]*os.File{
		"verifier": verifier,
		"upload":   snapshot.upload,
	} {
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFL, 0)
		if err != nil {
			t.Fatal(err)
		}
		if flags&unix.O_ACCMODE != unix.O_RDONLY {
			t.Fatalf("%s descriptor flags = %#x, want read-only", name, flags)
		}
	}
	var verifierStat, uploadStat unix.Stat_t
	if err := unix.Fstat(int(verifier.Fd()), &verifierStat); err != nil {
		t.Fatal(err)
	}
	if err := unix.Fstat(int(snapshot.upload.Fd()), &uploadStat); err != nil {
		t.Fatal(err)
	}
	if verifierStat.Dev != uploadStat.Dev || verifierStat.Ino != uploadStat.Ino {
		t.Fatal("snapshot descriptors do not identify the same inode")
	}
	if verifierStat.Mode&0o7777 != 0o400 || uploadStat.Mode&0o7777 != 0o400 {
		t.Fatalf(
			"snapshot modes = %#o/%#o, want 0400/0400",
			verifierStat.Mode&0o7777,
			uploadStat.Mode&0o7777,
		)
	}
	matches := 0
	descriptors, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range descriptors {
		fd, err := strconv.Atoi(descriptor.Name())
		if err != nil {
			continue
		}
		var descriptorStat unix.Stat_t
		if err := unix.Fstat(fd, &descriptorStat); err != nil {
			continue
		}
		if descriptorStat.Dev != verifierStat.Dev || descriptorStat.Ino != verifierStat.Ino {
			continue
		}
		matches++
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err != nil {
			t.Fatal(err)
		}
		if flags&unix.O_ACCMODE != unix.O_RDONLY {
			t.Fatalf("snapshot retained writable descriptor %d", fd)
		}
	}
	if matches != 2 {
		t.Fatalf("snapshot inode descriptor count = %d, want 2", matches)
	}

	first := make([]byte, 2)
	if _, err := verifier.Read(first); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, 2)
	if _, err := snapshot.upload.Read(second); err != nil {
		t.Fatal(err)
	}
	if string(first) != "ab" || string(second) != "ab" {
		t.Fatalf("independent reads = %q/%q, want ab/ab", first, second)
	}

	if os.Geteuid() != 0 {
		path := fmt.Sprintf("/proc/self/fd/%d", snapshot.upload.Fd())
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			writer.Close()
			t.Fatal("snapshot inode reopened for writing")
		}
	}
}

func TestArtifactSnapshotUploadRejectsTrustedOwnerMutation(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutate    func(*testing.T, *os.File)
		wantError string
	}{
		{
			name: "digest",
			mutate: func(t *testing.T, writer *os.File) {
				t.Helper()
				if _, err := writer.WriteAt([]byte("X"), 0); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "upload digest",
		},
		{
			name: "size",
			mutate: func(t *testing.T, writer *os.File) {
				t.Helper()
				if err := writer.Truncate(1); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "changed after sealing",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := newTestArtifactSnapshot(t, []byte("verified"))
			if err := snapshot.upload.Chmod(0o600); err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("/proc/self/fd/%d", snapshot.upload.Fd())
			writer, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, writer)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.upload.Chmod(0o400); err != nil {
				t.Fatal(err)
			}

			reader, err := snapshot.uploadReader(context.Background())
			if err == nil {
				_, err = io.ReadAll(reader)
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("upload error = %v, want %s", err, test.wantError)
			}
		})
	}
}

func TestArtifactSnapshotUploadRejectsMetadataMutation(t *testing.T) {
	snapshot := newTestArtifactSnapshot(t, []byte("verified"))
	if err := snapshot.upload.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.uploadReader(context.Background()); err == nil {
		t.Fatal("expected metadata mutation rejection")
	}
}

func TestArtifactSnapshotRejectsMismatchAndOversizeWithoutVisibleFiles(t *testing.T) {
	for _, test := range []struct {
		name       string
		descriptor ProgramDescriptor
		source     []byte
	}{
		{
			name:       "size mismatch",
			descriptor: testProgramDescriptor(codeArtifact, []byte("short")),
			source:     []byte("longer"),
		},
		{
			name: "oversize descriptor",
			descriptor: ProgramDescriptor{
				Digest:    "sha256:" + strings.Repeat("0", 64),
				SizeBytes: maxCodePhysicalBytes + 1,
				MediaType: ProgramCodeArtifactMediaType,
			},
			source: []byte("irrelevant"),
		},
		{
			name: "wrong role media type",
			descriptor: ProgramDescriptor{
				Digest:    "sha256:" + strings.Repeat("0", 64),
				SizeBytes: 1,
				MediaType: ProgramDependencyArtifactMediaType,
			},
			source: []byte("irrelevant"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			snapshot, err := snapshotArtifact(
				context.Background(),
				directory,
				codeArtifact,
				artifactSnapshotDescriptor(test.descriptor),
				bytes.NewReader(test.source),
			)
			if snapshot != nil {
				snapshot.Close()
				t.Fatal("snapshot was returned")
			}
			if err == nil {
				t.Fatal("snapshot error is nil")
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed snapshot left visible files: %#v", entries)
			}
		})
	}
}

func TestArtifactSnapshotRejectsSharedDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	snapshot, err := produceArtifactSnapshot(
		context.Background(),
		directory,
		codeArtifact,
		artifactSnapshotOwner{UID: os.Geteuid(), GID: os.Getegid()},
		func(file *os.File) error {
			_, err := file.Write([]byte("content"))
			return err
		},
	)
	if snapshot != nil || err == nil {
		t.Fatalf("snapshot/error = %v/%v", snapshot, err)
	}
}

func TestArtifactSnapshotCloseFailsAccess(t *testing.T) {
	snapshot := newTestArtifactSnapshot(t, []byte("closed"))
	path := filepath.Join(
		snapshot.platform.directory.Name(),
		snapshot.platform.name,
	)
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("closed snapshot stat error = %v", err)
	}
	if _, err := snapshot.verifierFile(); err == nil {
		t.Fatal("closed verifier descriptor remained accessible")
	}
	if _, err := snapshot.uploadReader(context.Background()); err == nil {
		t.Fatal("closed upload descriptor remained accessible")
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestProduceArtifactSnapshotSealsDirectOutput(t *testing.T) {
	content := []byte("direct encoder output")
	directory := privateArtifactTestDirectory(t)
	snapshot, err := produceArtifactSnapshot(
		context.Background(),
		directory,
		codeArtifact,
		artifactSnapshotOwner{UID: os.Geteuid(), GID: os.Getegid()},
		func(file *os.File) error {
			_, err := file.Write(content)
			return err
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	want := artifactSnapshotDescriptor(testProgramDescriptor(codeArtifact, content))
	if snapshot.descriptor != want {
		t.Fatalf("descriptor = %+v, want %+v", snapshot.descriptor, want)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(snapshot.upload.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Mode&0o7777 != 0o400 {
		t.Fatalf("snapshot mode = %#o, want 0400", stat.Mode&0o7777)
	}
	if int(stat.Uid) != os.Geteuid() || int(stat.Gid) != os.Getegid() {
		t.Fatalf(
			"snapshot owner = %d:%d, want %d:%d",
			stat.Uid,
			stat.Gid,
			os.Geteuid(),
			os.Getegid(),
		)
	}
}

func TestProduceArtifactSnapshotCleansFailedOutput(t *testing.T) {
	directory := privateArtifactTestDirectory(t)
	snapshot, err := produceArtifactSnapshot(
		context.Background(),
		directory,
		codeArtifact,
		artifactSnapshotOwner{UID: os.Geteuid(), GID: os.Getegid()},
		func(file *os.File) error {
			if _, err := file.Write([]byte("partial")); err != nil {
				return err
			}
			return fmt.Errorf("encoder failed")
		},
	)
	if snapshot != nil || err == nil {
		t.Fatalf("snapshot/error = %v/%v", snapshot, err)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed producer left entries: %#v", entries)
	}
}

func TestArtifactSnapshotLinksExactSealedInode(t *testing.T) {
	snapshot := newTestArtifactSnapshot(t, []byte("linked"))
	directory := t.TempDir()
	if err := snapshot.LinkInto(
		directory,
		"program.squashfs",
		os.Geteuid(),
		os.Getegid(),
	); err != nil {
		t.Fatal(err)
	}

	var source, linked unix.Stat_t
	if err := unix.Fstat(int(snapshot.upload.Fd()), &source); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(filepath.Join(directory, "program.squashfs"), &linked); err != nil {
		t.Fatal(err)
	}
	if source.Dev != linked.Dev ||
		source.Ino != linked.Ino ||
		source.Size != linked.Size ||
		source.Mode != linked.Mode ||
		source.Uid != linked.Uid ||
		source.Gid != linked.Gid {
		t.Fatalf("linked identity = %+v, want %+v", linked, source)
	}
}

func TestArtifactSnapshotTransfersOwnershipForJailer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to transfer artifact ownership")
	}
	snapshot := newTestArtifactSnapshot(t, []byte("linked"))
	directory := t.TempDir()
	const jailerID = 65534
	if err := snapshot.LinkInto(
		directory,
		"program.squashfs",
		jailerID,
		jailerID,
	); err != nil {
		t.Fatal(err)
	}

	var source, linked unix.Stat_t
	if err := unix.Fstat(int(snapshot.upload.Fd()), &source); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(filepath.Join(directory, "program.squashfs"), &linked); err != nil {
		t.Fatal(err)
	}
	if source.Uid != jailerID || source.Gid != jailerID {
		t.Fatalf("source owner = %d:%d, want %d:%d", source.Uid, source.Gid, jailerID, jailerID)
	}
	if linked.Uid != jailerID || linked.Gid != jailerID {
		t.Fatalf("linked owner = %d:%d, want %d:%d", linked.Uid, linked.Gid, jailerID, jailerID)
	}
	if source.Dev != linked.Dev || source.Ino != linked.Ino || source.Mode != linked.Mode {
		t.Fatalf("linked identity = %+v, want %+v", linked, source)
	}
}

func TestArtifactSnapshotLinkDoesNotReplaceDestination(t *testing.T) {
	snapshot := newTestArtifactSnapshot(t, []byte("linked"))
	directory := t.TempDir()
	path := filepath.Join(directory, "program.squashfs")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.LinkInto(
		directory,
		"program.squashfs",
		os.Geteuid(),
		os.Getegid(),
	); err == nil {
		t.Fatal("expected existing destination rejection")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "existing" {
		t.Fatalf("destination bytes = %q", content)
	}
}

func TestArtifactSnapshotLinkRejectsCrossFilesystem(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("/dev/shm is unavailable")
	}
	snapshot := newTestArtifactSnapshot(t, []byte("linked"))
	directory, err := os.MkdirTemp("/dev/shm", "helmr-snapshot-")
	if err != nil {
		t.Skipf("create cross-filesystem directory: %v", err)
	}
	defer os.RemoveAll(directory)

	var sourceStat, directoryStat unix.Stat_t
	if err := unix.Fstat(int(snapshot.upload.Fd()), &sourceStat); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(directory, &directoryStat); err != nil {
		t.Fatal(err)
	}
	if sourceStat.Dev == directoryStat.Dev {
		t.Skip("snapshot and /dev/shm use the same filesystem")
	}
	if err := snapshot.LinkInto(
		directory,
		"program.squashfs",
		os.Geteuid(),
		os.Getegid(),
	); err == nil {
		t.Fatal("expected cross-filesystem link rejection")
	}
	if _, err := os.Stat(filepath.Join(directory, "program.squashfs")); !os.IsNotExist(err) {
		t.Fatalf("cross-filesystem destination stat error = %v", err)
	}
}

func privateArtifactTestDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func newTestArtifactSnapshot(t *testing.T, content []byte) *artifactSnapshot {
	t.Helper()
	snapshot, err := snapshotArtifact(
		context.Background(),
		t.TempDir(),
		codeArtifact,
		artifactSnapshotDescriptor(testProgramDescriptor(codeArtifact, content)),
		bytes.NewReader(content),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	})
	return snapshot
}

func testProgramDescriptor(role artifactRole, content []byte) ProgramDescriptor {
	digest := sha256.Sum256(content)
	mediaType := ProgramCodeArtifactMediaType
	if role == dependencyArtifact {
		mediaType = ProgramDependencyArtifactMediaType
	}
	return ProgramDescriptor{
		Digest:    fmt.Sprintf("sha256:%x", digest),
		SizeBytes: int64(len(content)),
		MediaType: mediaType,
	}
}
