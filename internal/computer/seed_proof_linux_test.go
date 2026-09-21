//go:build linux && computerproof

package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/substrate"
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
	seedSource := substrate.NewDiskSource(seed, fmt.Sprintf("sha256:%x", original), 64<<20)
	image := seedConfigImage(t)
	imagePath := filepath.Join(dir, "image.tar")
	if err := os.WriteFile(imagePath, image, 0600); err != nil {
		t.Fatal(err)
	}
	objects, err := cas.NewFile(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := DiskStore{CAS: objects, Cipher: cipher}
	initial, err := store.Initialize(t.Context(), diskTestComputer, Seed{
		ImagePath: imagePath, Image: cas.Descriptor{Digest: sha256sum.DigestBytes(image), SizeBytes: int64(len(image)), MediaType: "application/vnd.oci.image.layout.v1+tar"}, Disk: seedSource,
	}, disk, dir, 128<<20, resize)
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Disk.Close()
	if err := initial.Disk.Upload(t.Context(), diskTestPublisher{objects}); err != nil {
		t.Fatal(err)
	}
	// Simulate serialization of the exact publication payload, not a DB commit.
	record, err := json.Marshal(InitialDisk{Artifact: initial.Disk.Artifact(), Config: initial.Config})
	if err != nil {
		t.Fatal(err)
	}
	var published InitialDisk
	if err := json.Unmarshal(record, &published); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(dir, "restored.ext4")
	if err := store.Restore(t.Context(), diskTestComputer, published.Artifact, restored, 128<<20); err != nil {
		t.Fatal(err)
	}
	if computerProofDigest(t, restored) != computerProofDigest(t, disk) {
		t.Fatal("initial publication bytes differ")
	}
	if published.Config.User != "1000:1000" || published.Config.WorkingDir != "/workspace" || len(published.Config.Env) != 3 {
		t.Fatalf("lost seed configuration: %+v", published.Config)
	}
	if err := os.Remove(restored); err != nil {
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
	if err := seedDisk(t.Context(), seedSource, disk, 128<<20, resize); err == nil {
		t.Fatal("existing disk overwritten")
	}
	if computerProofDigest(t, disk) != current {
		t.Fatal("creation retry changed existing disk")
	}
	failed := filepath.Join(dir, "failed.ext4")
	if err := seedDisk(t.Context(), seedSource, failed, 128<<20, "/missing-resize2fs"); err == nil {
		t.Fatal("invalid resize command succeeded")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("failed candidate retained: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := seedDisk(ctx, seedSource, failed, 128<<20, resize); err == nil {
		t.Fatal("cancelled creation succeeded")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("cancelled candidate retained: %v", err)
	}
	if err := seedDisk(t.Context(), seedSource, failed, 32<<20, resize); err == nil {
		t.Fatal("undersized capacity accepted")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("undersized candidate retained: %v", err)
	}
	unclean := dir + "/./normalized.ext4"
	if err := seedDisk(t.Context(), seedSource, unclean, 64<<20, resize); err != nil {
		t.Fatal(err)
	}
	if computerProofDigest(t, filepath.Clean(unclean)) != original {
		t.Fatal("equal-size seed contents changed")
	}
	// Later wake needs neither the OCI image nor the ext4 seed.
	if err := os.Remove(seed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(t.Context(), diskTestComputer, published.Artifact, restored, 128<<20); err != nil {
		t.Fatal(err)
	}
	computerProofCommand(t, "e2fsck", "-fn", restored)
	if got := computerProofCommand(t, "debugfs", "-R", "cat /sandbox-state", restored); got != "initial environment" {
		t.Fatal("restored seed state differs")
	}
	t.Log("independent writable seed, filesystem growth, immutable source, exclusive creation, and failed candidate removal passed")
}

func computerProofCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
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
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}
