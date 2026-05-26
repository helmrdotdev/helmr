//go:build linux

package guestd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRuntimeResolverKeepsResolverDirectives(t *testing.T) {
	got := string(normalizeRuntimeResolver([]byte("\n# generated\n nameserver 10.0.0.2 \nsearch example.test\n\n")))
	if got != "nameserver 10.0.0.2\nsearch example.test\n" {
		t.Fatalf("resolver = %q", got)
	}
}

func TestWriteImageRuntimeFileReplacesImageSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "resolv.conf"), filepath.Join(root, "etc", "resolv.conf")); err != nil {
		t.Fatal(err)
	}

	if err := writeImageRuntimeFile(root, "etc/resolv.conf", []byte("nameserver 10.0.0.2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("runtime file remained a symlink")
	}
	body, err := os.ReadFile(filepath.Join(root, "etc", "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "nameserver 10.0.0.2\n" {
		t.Fatalf("body = %q", body)
	}
	if _, err := os.Stat(filepath.Join(outside, "resolv.conf")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside resolver was modified, err=%v", err)
	}
}

func TestWriteImageRuntimeFileRejectsSymlinkedParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := writeImageRuntimeFile(root, "etc/hosts", []byte("127.0.0.1 localhost\n"), 0o644); err == nil {
		t.Fatal("expected symlinked parent rejection")
	}
}

func TestImageRuntimeHostsFileNamesSandbox(t *testing.T) {
	hosts := string(imageRuntimeHostsFile("helmr-test"))
	if !strings.Contains(hosts, "127.0.0.1 localhost helmr-test") {
		t.Fatalf("hosts = %q", hosts)
	}
	if !strings.Contains(hosts, "::1 localhost") {
		t.Fatalf("hosts = %q", hosts)
	}
}
