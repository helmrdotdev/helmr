//go:build linux && computerproof

package firecracker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComputerSeedProof(t *testing.T) {
	resize, err := exec.LookPath("resize2fs")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.ext4")
	disk := filepath.Join(dir, "computer.ext4")
	source, err := os.Create(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MKE2FS_CONFIG") == "" {
		t.Fatal("MKE2FS_CONFIG must point to the repository mke2fs.conf")
	}
	seedRoot := filepath.Join(dir, "seed-root")
	if err := os.Mkdir(seedRoot, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedRoot, "sandbox-state"), []byte("initial environment"), 0600); err != nil {
		t.Fatal(err)
	}
	// Match the builder's filesystem features and eager metadata initialization;
	// timestamp/UUID reproducibility is owned by the substrate projection tests.
	computerProofCommand(t, "mke2fs", "-q", "-t", "ext4", "-F", "-b", "4096", "-I", "256", "-i", "16384", "-m", "0",
		"-O", "sparse_super,large_file,filetype,resize_inode,dir_index,ext_attr,has_journal,extent,huge_file,flex_bg,metadata_csum,metadata_csum_seed,64bit,dir_nlink,extra_isize,orphan_file",
		"-E", "lazy_itable_init=0,lazy_journal_init=0,nodiscard,root_owner=0:0", "-d", seedRoot, seed)
	original := computerProofDigest(t, seed)
	if err := seedComputerDisk(t.Context(), seed, disk, 128<<20, resize); err != nil {
		t.Fatal(err)
	}
	computerProofCommand(t, "e2fsck", "-fn", disk)
	if got := computerProofCommand(t, "debugfs", "-R", "cat /sandbox-state", disk); got != "initial environment" {
		t.Fatalf("seed content lost: %q", got)
	}
	header := computerProofCommand(t, "debugfs", "-R", "stats", disk)
	if !strings.Contains(header, "Block count:              32768") {
		t.Fatalf("filesystem did not grow: %s", header)
	}
	payload := filepath.Join(dir, "payload")
	if err := os.WriteFile(payload, []byte("customer modification"), 0600); err != nil {
		t.Fatal(err)
	}
	computerProofCommand(t, "debugfs", "-w", "-R", "write "+payload+" /customer-state", disk)
	if got := computerProofCommand(t, "debugfs", "-R", "cat /customer-state", disk); got != "customer modification" {
		t.Fatalf("customer state = %q", got)
	}
	if got := computerProofDigest(t, seed); got != original {
		t.Fatal("writable disk changed shared seed")
	}
	// Refuse to overwrite an existing Computer, even if a caller retries creation.
	current := computerProofDigest(t, disk)
	if err := seedComputerDisk(t.Context(), seed, disk, 128<<20, resize); err == nil {
		t.Fatal("existing disk overwritten")
	}
	if computerProofDigest(t, disk) != current {
		t.Fatal("creation retry changed existing disk")
	}
	failed := filepath.Join(dir, "failed.ext4")
	if err := seedComputerDisk(t.Context(), seed, failed, 128<<20, "/missing-resize2fs"); err == nil {
		t.Fatal("invalid resize command succeeded")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("failed candidate retained: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := seedComputerDisk(ctx, seed, failed, 128<<20, resize); err == nil {
		t.Fatal("cancelled creation succeeded")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("cancelled candidate retained: %v", err)
	}
	if err := seedComputerDisk(t.Context(), seed, failed, 32<<20, resize); err == nil {
		t.Fatal("undersized capacity accepted")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("undersized candidate retained: %v", err)
	}
	t.Log("independent writable seed, filesystem growth, immutable source, exclusive creation, and failed candidate removal passed")
}
