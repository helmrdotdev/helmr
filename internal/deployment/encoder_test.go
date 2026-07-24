package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProgramEncoderUsesFixedProcessContract(t *testing.T) {
	t.Parallel()
	executable := writeEncoderFixture(t, `#!/bin/sh
if [ "$1" = "-version" ]; then
	printf 'mksquashfs version 4.6.1 (2023/03/25)\n'
	exit 0
fi
if [ "$*" != "`+strings.Join(encoderArguments, " ")+`" ]; then
	printf 'arguments: %s\n' "$*" >&2
	exit 2
fi
if [ "$LC_ALL" != "C" ] || [ "$TZ" != "UTC" ] ||
	[ -n "${HOME+x}" ] || [ -n "${SOURCE_DATE_EPOCH+x}" ]; then
	printf 'unexpected environment: LC_ALL=%s TZ=%s HOME=%s SOURCE_DATE_EPOCH=%s\n' \
		"$LC_ALL" "$TZ" "${HOME+x}" "${SOURCE_DATE_EPOCH+x}" >&2
	exit 3
fi
printf 'encoded' >&3
`)
	destination := emptyEncoderDestination(t)
	defer destination.Close()

	if err := encodeSquashFS(
		context.Background(),
		executable,
		strings.NewReader("archive"),
		destination,
	); err != nil {
		t.Fatalf("encodeSquashFS: %v", err)
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "encoded" {
		t.Fatalf("encoded output = %q", encoded)
	}
}

func TestProgramEncoderRejectsInvalidExecutable(t *testing.T) {
	t.Parallel()
	destination := emptyEncoderDestination(t)
	defer destination.Close()
	if err := encodeSquashFS(
		context.Background(),
		"mksquashfs",
		strings.NewReader("archive"),
		destination,
	); err == nil {
		t.Fatal("encodeSquashFS accepted a relative executable")
	}

	executable := writeEncoderFixture(t, `#!/bin/sh
printf 'mksquashfs version 4.7.0\n'
`)
	if err := encodeSquashFS(
		context.Background(),
		executable,
		strings.NewReader("archive"),
		destination,
	); err == nil {
		t.Fatal("encodeSquashFS accepted another encoder version")
	}
}

func TestProgramEncoderRejectsInvalidDestination(t *testing.T) {
	t.Parallel()
	executable := writeEncoderFixture(t, `#!/bin/sh
if [ "$1" = "-version" ]; then
	printf 'mksquashfs version 4.6.1 (2023/03/25)\n'
fi
`)
	tests := map[string]func(*testing.T) *os.File{
		"nonempty": func(t *testing.T) *os.File {
			file := emptyEncoderDestination(t)
			if _, err := file.WriteString("occupied"); err != nil {
				t.Fatal(err)
			}
			return file
		},
		"wrong mode": func(t *testing.T) *os.File {
			path := filepath.Join(t.TempDir(), "program.squashfs")
			file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
			if err != nil {
				t.Fatal(err)
			}
			return file
		},
	}
	for name, create := range tests {
		t.Run(name, func(t *testing.T) {
			destination := create(t)
			defer destination.Close()
			if err := encodeSquashFS(
				context.Background(),
				executable,
				strings.NewReader("archive"),
				destination,
			); err == nil {
				t.Fatal("encodeSquashFS accepted an invalid destination")
			}
		})
	}
}

func TestPinnedProgramEncoder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the encoder output path uses Linux procfs")
	}
	executable := os.Getenv("HELMR_SQUASHFS_ENCODER")
	if executable == "" {
		t.Skip("HELMR_SQUASHFS_ENCODER is not set")
	}

	var archive bytes.Buffer
	if err := writeProgramArchive(
		context.Background(),
		&archive,
		programArtifact,
		treeEntrySequence(programArchiveFixture()),
	); err != nil {
		t.Fatal(err)
	}
	first := encodeFixtureArchive(t, executable, archive.Bytes())
	second := encodeFixtureArchive(t, executable, archive.Bytes())
	if !bytes.Equal(first, second) {
		t.Fatal("pinned encoder produced different bytes for the same archive")
	}
	digest := sha256.Sum256(first)
	const wantDigest = "812bac5025ed5d0b02a32cb2309be99a5adeb9a9b457852d7e19176fa63e5c2f"
	if got := hex.EncodeToString(digest[:]); got != wantDigest {
		t.Fatalf("pinned encoder digest = %s, want %s", got, wantDigest)
	}

	reader, err := newSquashFSArtifactReader(
		context.Background(),
		bytes.NewReader(first),
		int64(len(first)),
		programArtifact,
	)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reader.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(programArchiveFixture())+1 {
		t.Fatalf("SquashFS entry count = %d", len(entries))
	}
	if entries[0].Path != "." ||
		entries[0].Kind != artifactEntryDirectory ||
		entries[0].Mode != 0755 {
		t.Fatalf("SquashFS root = %#v", entries[0])
	}
	content, err := reader.Open(context.Background(), "bin/tool")
	if err != nil {
		t.Fatal(err)
	}
	defer content.Close()
	raw, err := io.ReadAll(content)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "#!/usr/bin/env node\n" {
		t.Fatalf("bin/tool = %q", raw)
	}
}

func encodeFixtureArchive(t *testing.T, executable string, archive []byte) []byte {
	t.Helper()
	destination := emptyEncoderDestination(t)
	if err := encodeSquashFS(
		context.Background(),
		executable,
		bytes.NewReader(archive),
		destination,
	); err != nil {
		destination.Close()
		t.Fatal(err)
	}
	if _, err := destination.Seek(0, io.SeekStart); err != nil {
		destination.Close()
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(destination)
	if closeErr := destination.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func emptyEncoderDestination(t *testing.T) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "program.squashfs")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func writeEncoderFixture(t *testing.T, source string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mksquashfs")
	if err := os.WriteFile(path, []byte(source), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
