package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func buildOutputArchive(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, header := range headers {
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			if _, err := writer.Write(bytes.Repeat([]byte("x"), int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestExtractBuildOutputPreservesFilesAndLinks(t *testing.T) {
	modified := time.Unix(123456, 0)
	raw := buildOutputArchive(t,
		tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0555, ModTime: modified},
		tar.Header{Name: "bin/tool", Typeflag: tar.TypeReg, Mode: 0755, Size: 3, ModTime: modified, Uid: 65532, Gid: 65532},
		tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "bin/tool"},
		tar.Header{Name: "chain", Typeflag: tar.TypeSymlink, Linkname: "link"},
		tar.Header{Name: "dangling", Typeflag: tar.TypeSymlink, Linkname: "missing"},
		tar.Header{Name: "not-directory", Typeflag: tar.TypeSymlink, Linkname: "bin/tool/child"},
		tar.Header{Name: "hard", Typeflag: tar.TypeLink, Linkname: "bin/tool"},
	)
	destination := filepath.Join(t.TempDir(), "output")
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(destination, "bin"), 0755) })
	if err := extractBuildOutput(context.Background(), bytes.NewReader(raw), destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bin/tool", "link", "chain", "hard"} {
		body, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(body) != "xxx" {
			t.Fatalf("%s contents = %q, %v", name, body, err)
		}
	}
	file, err := os.Stat(filepath.Join(destination, "bin/tool"))
	if err != nil {
		t.Fatal(err)
	}
	hard, err := os.Stat(filepath.Join(destination, "hard"))
	if err != nil || !os.SameFile(file, hard) {
		t.Fatalf("hardlink = %v, %v", hard, err)
	}
	for name, mode := range map[string]os.FileMode{"bin": 0555, "bin/tool": 0755} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil || info.Mode().Perm() != mode || !info.ModTime().Equal(modified) {
			t.Fatalf("%s metadata = %v, %v", name, info, err)
		}
	}
}

func TestExtractBuildOutputRejectsUnsafeEntries(t *testing.T) {
	regular := tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644, Size: 1}
	for name, headers := range map[string][]tar.Header{
		"parent":          {{Name: "../escape", Typeflag: tar.TypeReg}},
		"nested-parent":   {{Name: "dir/../file", Typeflag: tar.TypeReg}},
		"absolute":        {{Name: "/escape", Typeflag: tar.TypeReg}},
		"symlink-parent":  {{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../escape"}},
		"symlink-root":    {{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}},
		"hardlink-parent": {{Name: "link", Typeflag: tar.TypeLink, Linkname: "../escape"}},
		"hardlink-symlink": {
			regular,
			{Name: "symlink", Typeflag: tar.TypeSymlink, Linkname: "file"},
			{Name: "hardlink", Typeflag: tar.TypeLink, Linkname: "symlink"},
		},
		"duplicate":    {regular, regular},
		"alias":        {regular, {Name: "./file", Typeflag: tar.TypeReg}},
		"fifo":         {{Name: "fifo", Typeflag: tar.TypeFifo}},
		"special-mode": {{Name: "file", Typeflag: tar.TypeReg, Mode: 04755}},
		"symlink-directory": {
			{Name: "dir", Typeflag: tar.TypeDir},
			{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "dir"},
			{Name: "link/", Typeflag: tar.TypeDir},
		},
		"symlink-parent-alias": {
			{Name: "s", Typeflag: tar.TypeSymlink, Linkname: "."},
			{Name: "s/link", Typeflag: tar.TypeSymlink, Linkname: "../outside"},
		},
		"symlink-expanded-parent": {
			{Name: "d", Typeflag: tar.TypeDir},
			{Name: "s", Typeflag: tar.TypeSymlink, Linkname: "."},
			{Name: "d/link", Typeflag: tar.TypeSymlink, Linkname: "../s/../outside"},
		},
		"symlink-cycle": {
			{Name: "a", Typeflag: tar.TypeSymlink, Linkname: "b"},
			{Name: "b", Typeflag: tar.TypeSymlink, Linkname: "a"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "output")
			err := extractBuildOutput(context.Background(), bytes.NewReader(buildOutputArchive(t, headers...)), destination)
			if err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestExtractBuildOutputRejectsTruncationAndCancellation(t *testing.T) {
	raw := buildOutputArchive(t, tar.Header{Name: "large", Typeflag: tar.TypeReg, Size: 4096})
	if err := extractBuildOutput(context.Background(), bytes.NewReader(raw[:1024]), filepath.Join(t.TempDir(), "output")); err == nil {
		t.Fatal("truncated file accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := extractBuildOutput(ctx, bytes.NewReader(raw), filepath.Join(t.TempDir(), "output")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled extraction = %v", err)
	}
}

func TestExecuteDockerBuildxStreamsLocalOutputAndCleansArchive(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "export-failure"}[fail], func(t *testing.T) {
			stage := t.TempDir()
			input := filepath.Join(stage, "input.tar")
			raw := buildOutputArchive(t, tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0644, Size: 3})
			if err := os.WriteFile(input, raw, 0600); err != nil {
				t.Fatal(err)
			}
			script := `#!/bin/sh
set -eu
while [ "$1" != "--output" ]; do shift; done
shift
case "$1" in type=tar,dest=*) ;; *) exit 90 ;; esac
cp "$BUILDX_TEST_ARCHIVE" "${1#type=tar,dest=}"
`
			if fail {
				script += "exit 1\n"
			}
			docker := filepath.Join(stage, "docker")
			if err := os.WriteFile(docker, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BUILDX_TEST_ARCHIVE", input)
			output := filepath.Join(stage, "output")
			err := executeDockerBuildx(context.Background(), &cobra.Command{}, dockerBuildxRequest{
				Runner: dockerBuildRunner{docker: docker, environment: os.Environ()},
				Output: output, OutputType: "local",
			})
			if fail {
				if err == nil {
					t.Fatal("export failure accepted")
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed export materialized output: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(filepath.Join(output, "file"))
				if err != nil || string(body) != "xxx" {
					t.Fatalf("exported file = %q, %v", body, err)
				}
			}
			archives, err := filepath.Glob(filepath.Join(stage, ".helmr-build-output-*.tar"))
			if err != nil || len(archives) != 0 {
				t.Fatalf("temporary archives = %v, %v", archives, err)
			}
		})
	}
}
