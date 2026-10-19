//go:build linux

package localcopy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/cas"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"golang.org/x/sys/unix"
)

func TestArtifactCopyOwnsPrivateNamedBytes(t *testing.T) {
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

	local, err := copyArtifact(
		context.Background(),
		directory,
		artifact.RoleProgram,
		cas.Descriptor(testProgramDescriptor(artifact.RoleProgram, []byte("original"))),
		source,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	leaseDirectory := local.platform.directory.Name()
	snapshotPath := filepath.Join(leaseDirectory, local.platform.name)

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
		t.Fatalf("local directory entries = %#v", entries)
	}
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	if !names["encoder-output"] ||
		!names[filepath.Base(leaseDirectory)] {
		t.Fatalf("local directory entries = %#v", entries)
	}

	reader, err := local.UploadReader(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("local bytes = %q, want original", got)
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(snapshotPath); !os.IsNotExist(err) {
		t.Fatalf("closed local stat error = %v", err)
	}
	if _, err := os.Stat(leaseDirectory); !os.IsNotExist(err) {
		t.Fatalf("closed local lease stat error = %v", err)
	}
}

func TestArtifactCopyDescriptorsAreReadOnlyAndIndependent(t *testing.T) {
	local := newTestArtifactCopy(t, []byte("abcdef"))
	verifier, err := local.VerifierFile()
	if err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]*os.File{
		"verifier": verifier,
		"upload":   local.upload,
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
	if err := unix.Fstat(int(local.upload.Fd()), &uploadStat); err != nil {
		t.Fatal(err)
	}
	if verifierStat.Dev != uploadStat.Dev || verifierStat.Ino != uploadStat.Ino {
		t.Fatal("local descriptors do not identify the same inode")
	}
	if verifierStat.Mode&0o7777 != 0o400 || uploadStat.Mode&0o7777 != 0o400 {
		t.Fatalf(
			"local modes = %#o/%#o, want 0400/0400",
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
			t.Fatalf("local retained writable descriptor %d", fd)
		}
	}
	if matches != 2 {
		t.Fatalf("local inode descriptor count = %d, want 2", matches)
	}

	first := make([]byte, 2)
	if _, err := verifier.Read(first); err != nil {
		t.Fatal(err)
	}
	second := make([]byte, 2)
	if _, err := local.upload.Read(second); err != nil {
		t.Fatal(err)
	}
	if string(first) != "ab" || string(second) != "ab" {
		t.Fatalf("independent reads = %q/%q, want ab/ab", first, second)
	}

	if os.Geteuid() != 0 {
		path := fmt.Sprintf("/proc/self/fd/%d", local.upload.Fd())
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			writer.Close()
			t.Fatal("local inode reopened for writing")
		}
	}
}

func TestArtifactCopyUploadRejectsTrustedOwnerMutation(t *testing.T) {
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
			local := newTestArtifactCopy(t, []byte("verified"))
			if err := local.upload.Chmod(0o600); err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("/proc/self/fd/%d", local.upload.Fd())
			writer, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(t, writer)
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := local.upload.Chmod(0o400); err != nil {
				t.Fatal(err)
			}

			reader, err := local.UploadReader(context.Background())
			if err == nil {
				_, err = io.ReadAll(reader)
			}
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("upload error = %v, want %s", err, test.wantError)
			}
		})
	}
}

func TestArtifactCopyUploadRejectsMetadataMutation(t *testing.T) {
	local := newTestArtifactCopy(t, []byte("verified"))
	if err := local.upload.Chmod(0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := local.UploadReader(context.Background()); err == nil {
		t.Fatal("expected metadata mutation rejection")
	}
}

func TestArtifactCopyRejectsMismatchAndOversizeWithoutVisibleFiles(t *testing.T) {
	for _, test := range []struct {
		name       string
		descriptor artifact.ProgramDescriptor
		source     []byte
	}{
		{
			name:       "size mismatch",
			descriptor: testProgramDescriptor(artifact.RoleProgram, []byte("short")),
			source:     []byte("longer"),
		},
		{
			name: "oversize descriptor",
			descriptor: artifact.ProgramDescriptor{
				Digest:    "sha256:" + strings.Repeat("0", 64),
				SizeBytes: artifact.MaxProgramPhysicalBytes + 1,
				MediaType: artifact.ProgramArtifactMediaType,
			},
			source: []byte("irrelevant"),
		},
		{
			name: "wrong role media type",
			descriptor: artifact.ProgramDescriptor{
				Digest:    "sha256:" + strings.Repeat("0", 64),
				SizeBytes: 1,
				MediaType: "application/octet-stream",
			},
			source: []byte("irrelevant"),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			local, err := copyArtifact(
				context.Background(),
				directory,
				artifact.RoleProgram,
				cas.Descriptor(test.descriptor),
				bytes.NewReader(test.source),
			)
			if local != nil {
				local.Close()
				t.Fatal("local was returned")
			}
			if err == nil {
				t.Fatal("local error is nil")
			}
			entries, readErr := os.ReadDir(directory)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed local left visible files: %#v", entries)
			}
		})
	}
}

func TestArtifactCopyRejectsSharedDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	local, err := Produce(
		context.Background(),
		directory,
		artifact.RoleProgram,
		Owner{UID: os.Geteuid(), GID: os.Getegid()},
		false,
		func(file *os.File) error {
			_, err := file.Write([]byte("content"))
			return err
		},
	)
	if local != nil || err == nil {
		t.Fatalf("local/error = %v/%v", local, err)
	}
}

func TestArtifactCopyCloseFailsAccess(t *testing.T) {
	local := newTestArtifactCopy(t, []byte("closed"))
	path := filepath.Join(
		local.platform.directory.Name(),
		local.platform.name,
	)
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("closed local stat error = %v", err)
	}
	if _, err := local.VerifierFile(); err == nil {
		t.Fatal("closed verifier descriptor remained accessible")
	}
	if _, err := local.UploadReader(context.Background()); err == nil {
		t.Fatal("closed upload descriptor remained accessible")
	}
	if err := local.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestProduceArtifactCopySealsDirectOutput(t *testing.T) {
	content := []byte("direct encoder output")
	directory := privateArtifactTestDirectory(t)
	local, err := Produce(
		context.Background(),
		directory,
		artifact.RoleProgram,
		Owner{UID: os.Geteuid(), GID: os.Getegid()},
		false,
		func(file *os.File) error {
			_, err := file.Write(content)
			return err
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	want := cas.Descriptor(testProgramDescriptor(artifact.RoleProgram, content))
	if local.descriptor != want {
		t.Fatalf("descriptor = %+v, want %+v", local.descriptor, want)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(local.upload.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if stat.Mode&0o7777 != 0o400 {
		t.Fatalf("local mode = %#o, want 0400", stat.Mode&0o7777)
	}
	if int(stat.Uid) != os.Geteuid() || int(stat.Gid) != os.Getegid() {
		t.Fatalf(
			"local owner = %d:%d, want %d:%d",
			stat.Uid,
			stat.Gid,
			os.Geteuid(),
			os.Getegid(),
		)
	}
}

func TestProduceArtifactCopyCleansFailedOutput(t *testing.T) {
	directory := privateArtifactTestDirectory(t)
	local, err := Produce(
		context.Background(),
		directory,
		artifact.RoleProgram,
		Owner{UID: os.Geteuid(), GID: os.Getegid()},
		false,
		func(file *os.File) error {
			if _, err := file.Write([]byte("partial")); err != nil {
				return err
			}
			return fmt.Errorf("encoder failed")
		},
	)
	if local != nil || err == nil {
		t.Fatalf("local/error = %v/%v", local, err)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("failed producer left entries: %#v", entries)
	}
}

func TestArtifactCopyLinksExactSealedInode(t *testing.T) {
	local := newTestArtifactCopy(t, []byte("linked"))
	directory := t.TempDir()
	if err := local.LinkInto(
		directory,
		"program.squashfs",
		os.Geteuid(),
		os.Getegid(),
	); err != nil {
		t.Fatal(err)
	}

	var source, linked unix.Stat_t
	if err := unix.Fstat(int(local.upload.Fd()), &source); err != nil {
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

func TestArtifactCopyTransfersOwnershipForJailer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to transfer artifact ownership")
	}
	local := newTestArtifactCopy(t, []byte("linked"))
	directory := t.TempDir()
	const jailerID = 65534
	if err := local.LinkInto(
		directory,
		"program.squashfs",
		jailerID,
		jailerID,
	); err != nil {
		t.Fatal(err)
	}

	var source, linked unix.Stat_t
	if err := unix.Fstat(int(local.upload.Fd()), &source); err != nil {
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

func TestArtifactCopyLinkDoesNotReplaceDestination(t *testing.T) {
	local := newTestArtifactCopy(t, []byte("linked"))
	directory := t.TempDir()
	path := filepath.Join(directory, "program.squashfs")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := local.LinkInto(
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

func TestArtifactCopyLinkRejectsCrossFilesystem(t *testing.T) {
	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("/dev/shm is unavailable")
	}
	local := newTestArtifactCopy(t, []byte("linked"))
	directory, err := os.MkdirTemp("/dev/shm", "helmr-local-")
	if err != nil {
		t.Skipf("create cross-filesystem directory: %v", err)
	}
	defer os.RemoveAll(directory)

	var sourceStat, directoryStat unix.Stat_t
	if err := unix.Fstat(int(local.upload.Fd()), &sourceStat); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(directory, &directoryStat); err != nil {
		t.Fatal(err)
	}
	if sourceStat.Dev == directoryStat.Dev {
		t.Skip("local and /dev/shm use the same filesystem")
	}
	if err := local.LinkInto(
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

func newTestArtifactCopy(t *testing.T, content []byte) *Artifact {
	t.Helper()
	local, err := copyArtifact(
		context.Background(),
		t.TempDir(),
		artifact.RoleProgram,
		cas.Descriptor(testProgramDescriptor(artifact.RoleProgram, content)),
		bytes.NewReader(content),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Error(err)
		}
	})
	return local
}

func testProgramDescriptor(role artifact.Role, content []byte) artifact.ProgramDescriptor {
	digest := sha256.Sum256(content)
	return artifact.ProgramDescriptor{
		Digest:    fmt.Sprintf("sha256:%x", digest),
		SizeBytes: int64(len(content)),
		MediaType: artifact.ProgramArtifactMediaType,
	}
}

func TestReceiveVerifiesPinnedBytesAndOwnsCleanup(t *testing.T) {
	for _, content := range []string{"original", "modified", "short", "original-long"} {
		t.Run(content, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Chmod(directory, 0700); err != nil {
				t.Fatal(err)
			}
			expected := cas.Descriptor(testProgramDescriptor(artifact.RoleProgram, []byte("original")))
			var writer *os.File
			local, err := Receive(t.Context(), directory, artifact.RoleProgram, expected, func(file *os.File) error {
				writer = file
				_, err := file.WriteString(content)
				return err
			})
			if writer != nil {
				if _, writeErr := writer.WriteString("mutation"); !errors.Is(writeErr, os.ErrClosed) {
					t.Fatalf("writer remains open: %v", writeErr)
				}
			}
			if content == "original" {
				if err != nil {
					t.Fatal(err)
				}
				file, err := local.VerifierFile()
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(file)
				if err != nil || string(body) != content {
					t.Fatalf("content %q %v", body, err)
				}
				if err := local.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				_ = local.Close()
				t.Fatal("received wrong bytes")
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("cleanup %v %v", entries, err)
			}
		})
	}
}

func TestReceiveFailureDoesNotRetainBytes(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Chmod(directory, 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			want := errors.New("transfer failed")
			if canceled {
				want = context.Canceled
			}
			expected := cas.Descriptor(testProgramDescriptor(artifact.RoleProgram, []byte("original")))
			local, err := Receive(ctx, directory, artifact.RoleProgram, expected, func(file *os.File) error {
				if _, err := file.WriteString("original"); err != nil {
					return err
				}
				if canceled {
					cancel()
					return nil
				}
				return want
			})
			if local != nil || !errors.Is(err, want) {
				t.Fatalf("received %v, %v", local, err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("cleanup %v %v", entries, err)
			}
		})
	}
}
