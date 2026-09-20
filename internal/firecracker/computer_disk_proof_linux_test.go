//go:build linux && computerproof

package firecracker

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This opt-in proof exercises the existing filepack codec against a real offline
// ext4 filesystem. It does not exercise guest mounts, encryption/upload, live
// writer exclusion, memory restore, or the proposed Computer lifecycle.
func TestComputerDiskProof(t *testing.T) {
	for _, name := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	files := map[string]string{
		"workspace/result.txt":            "child task result\n",
		"home/agent/.claude/history.json": `{ "conversation": "continued" }`,
		"etc/agent.conf":                  "configured outside workspace\n",
		"opt/agent/bin/codex":             "#!/bin/sh\nprintf continued\n",
	}
	for path, body := range files {
		full := filepath.Join(seed, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0600)
		if strings.HasPrefix(path, "opt/") {
			mode = 0755
		}
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	// The native Codex case uses a long absolute link into the separately pinned
	// Program mount. Preserve its bytes without pretending that offline image
	// inspection qualifies the mount or executable after guest restore.
	const nativeTarget = "/opt/helmr/program/node_modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex"
	if err := os.Symlink(nativeTarget, filepath.Join(seed, "home/agent/native-apply_patch")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filepath.Join(seed, "etc/agent.conf"), "user.computer-proof", []byte("retained"), 0); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(seed, "home/agent/apply_patch")
	if err := os.Symlink("/opt/agent/bin/codex", link); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(seed, "workspace/result.txt"), filepath.Join(seed, "workspace/result-link.txt")); err != nil {
		t.Fatal(err)
	}
	// Non-compressible content keeps timing/size observations from describing only
	// zero-filled disk space. This is a fixture, not a representative agent workload.
	payload := make([]byte, 8<<20)
	for offset := 0; offset < len(payload); offset += sha256.Size {
		block := sha256.Sum256([]byte(fmt.Sprint(offset)))
		copy(payload[offset:], block[:])
	}
	if err := os.WriteFile(filepath.Join(seed, "opt/agent/cache"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "computer.raw")
	packed := filepath.Join(root, "computer.filepack")
	restored := filepath.Join(root, "restored.raw")
	const logicalSize = 128 << 20
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(logicalSize); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	computerProofCommand(t, "mke2fs", "-q", "-t", "ext4", "-F", "-d", seed, source)
	computerProofCommand(t, "e2fsck", "-fn", source)
	sourceHash := computerProofDigest(t, source)
	start := time.Now()
	// Reuse the existing scratch-disk encoding for this private codec experiment;
	// this does not introduce a persisted Computer role or a compatibility mode.
	stats, err := packRuntimeFile(context.Background(), source, packed, filepackScratchRole)
	if err != nil {
		t.Fatal(err)
	}
	packTime := time.Since(start)
	info, err := os.Stat(packed)
	if err != nil {
		t.Fatal(err)
	}
	// Remove both original disk and seed: all restoration bytes must come from
	// the artifact, including paths that the old Workspace tar would omit/reject.
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(seed); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	restoredStats, err := unpackRuntimeFile(context.Background(), packed, restored, filepackScratchRole, logicalSize)
	if err != nil {
		t.Fatal(err)
	}
	restoreTime := time.Since(start)
	if got := computerProofDigest(t, restored); got != sourceHash {
		t.Fatalf("disk digest differs: %x != %x", got, sourceHash)
	}
	computerProofCommand(t, "e2fsck", "-fn", restored)
	for path, want := range files {
		if got := computerProofCommand(t, "debugfs", "-R", "cat /"+path, restored); got != want {
			t.Fatalf("%s = %q; want %q", path, got, want)
		}
	}
	linkStat := computerProofCommand(t, "debugfs", "-R", "stat /home/agent/apply_patch", restored)
	if !strings.Contains(linkStat, `Fast link dest: "/opt/agent/bin/codex"`) {
		t.Fatalf("absolute symlink not preserved: %s", linkStat)
	}
	if got := computerProofCommand(t, "debugfs", "-R", "cat /home/agent/native-apply_patch", restored); got != nativeTarget {
		t.Fatalf("native link target = %q; want %q", got, nativeTarget)
	}
	attributes := computerProofCommand(t, "debugfs", "-R", "ea_list /etc/agent.conf", restored)
	if !strings.Contains(attributes, `user.computer-proof (8) = "retained"`) {
		t.Fatalf("extended attribute not preserved: %s", attributes)
	}
	executableStat := computerProofCommand(t, "debugfs", "-R", "stat /opt/agent/bin/codex", restored)
	if !strings.Contains(executableStat, "0755") {
		t.Fatalf("executable permissions not preserved: %s", executableStat)
	}
	hardlinkStat := computerProofCommand(t, "debugfs", "-R", "stat /workspace/result-link.txt", restored)
	if !strings.Contains(hardlinkStat, "Links: 2") {
		t.Fatalf("hard link not preserved: %s", hardlinkStat)
	}
	if got := computerProofCommand(t, "debugfs", "-R", "cat /opt/agent/cache", restored); got != string(payload) {
		t.Fatal("cache content differs")
	}
	t.Logf("logical_bytes=%d artifact_bytes=%d pack=%s unpack=%s pack_stats=%+v unpack_stats=%+v", logicalSize, info.Size(), packTime, restoreTime, stats, restoredStats)
}

func computerProofCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, stderr.String())
	}
	return string(output)
}

func computerProofDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		t.Fatal(err)
	}
	return [32]byte(hash.Sum(nil))
}
