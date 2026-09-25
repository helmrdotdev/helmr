//go:build linux && computerproof

package computer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestComputerSeedProof(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.ext4")
	disk := filepath.Join(dir, "computer.ext4")
	source, err := os.Create(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Truncate(128 << 20); err != nil {
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
	image := seedConfigImage(t)
	imagePath := filepath.Join(dir, "image.tar")
	if err := os.WriteFile(imagePath, image, 0600); err != nil {
		t.Fatal(err)
	}
	objects, err := cas.NewFile(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	config, err := oci.ReadVerifiedConfig(t.Context(), imagePath, sha256sum.DigestBytes(image), int64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	seeds := SeedStore{CAS: objects}
	seedSource := publishTestSeed(t, objects, seed, dir, config)
	// Runtime initialization must no longer read the original OCI archive.
	if err := os.Remove(imagePath); err != nil {
		t.Fatal(err)
	}
	if err := seeds.Decode(t.Context(), seedSource.Artifact, disk, 128<<20); err != nil {
		t.Fatal(err)
	}
	if seedSource.Config.User != "1000:1000" || seedSource.Config.WorkingDir != "/workspace" || len(seedSource.Config.Env) != 3 {
		t.Fatal("lost seed configuration")
	}
	computerProofCommand(t, "e2fsck", "-fn", disk)
	if got := computerProofCommand(t, "debugfs", "-R", "cat /sandbox-state", disk); got != "initial environment" {
		t.Fatalf("seed content lost: %q", got)
	}
	header := computerProofCommand(t, "debugfs", "-R", "stats", disk)
	if !strings.Contains(header, "Block count:              32768") {
		t.Fatalf("filesystem capacity changed: %s", header)
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
	if err := seeds.Decode(t.Context(), seedSource.Artifact, disk, 128<<20); err == nil {
		t.Fatal("existing disk overwritten")
	}
	if computerProofDigest(t, disk) != current {
		t.Fatal("creation retry changed existing disk")
	}
	failed := filepath.Join(dir, "failed.ext4")
	if err := seeds.Decode(t.Context(), seedSource.Artifact, failed, 256<<20); err == nil {
		t.Fatal("oversized capacity accepted")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("failed candidate retained: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := seeds.Decode(ctx, seedSource.Artifact, failed, 128<<20); err == nil {
		t.Fatal("cancelled creation succeeded")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("cancelled candidate retained: %v", err)
	}
	if err := seeds.Decode(t.Context(), seedSource.Artifact, failed, 32<<20); err == nil {
		t.Fatal("undersized capacity accepted")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("undersized candidate retained: %v", err)
	}
	unclean := dir + "/./normalized.ext4"
	if err := seeds.Decode(t.Context(), seedSource.Artifact, unclean, 128<<20); err != nil {
		t.Fatal(err)
	}
	if computerProofDigest(t, filepath.Clean(unclean)) != original {
		t.Fatal("equal-size seed contents changed")
	}

	t.Log("independent writable seed, exact capacity, immutable source, exclusive creation, and failed candidate removal passed")
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

// Simulates admitted client artifacts, not a durable publication transaction.
func publishTestSeed(t *testing.T, objects cas.Store, source, staging string, config oci.RuntimeConfig) Seed {
	t.Helper()
	path := filepath.Join(staging, "seed.filepack")
	artifact, err := EncodeSeed(t.Context(), source, path)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	object, err := objects.Put(t.Context(), SeedMediaType, file)
	if err != nil {
		t.Fatal(err)
	}
	if object.Digest != artifact.Object.Digest || object.SizeBytes != artifact.Object.SizeBytes {
		t.Fatal("uploaded seed differs")
	}
	return Seed{Artifact: artifact, Config: config}
}

func seedConfigImage(t *testing.T) []byte {
	t.Helper()
	config := []byte(`{"config":{"Env":["A=one","A=two","EMPTY="],"WorkingDir":"/workspace","User":"1000:1000","Entrypoint":["/bin/sh","-c"],"Cmd":["echo hello"]}}`)
	marshal := func(v any) []byte {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	descriptor := func(b []byte, media string) oci.Descriptor {
		return oci.Descriptor{Digest: sha256sum.DigestBytes(b), Size: int64(len(b)), MediaType: media}
	}
	manifest := marshal(oci.Manifest{Config: descriptor(config, "application/vnd.oci.image.config.v1+json")})
	index := marshal(oci.Index{Manifests: []oci.Descriptor{descriptor(manifest, "application/vnd.oci.image.manifest.v1+json")}})
	var result bytes.Buffer
	tw := tar.NewWriter(&result)
	for _, entry := range []struct {
		name string
		body []byte
	}{
		{"oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`)}, {"index.json", index},
		{"blobs/sha256/" + sha256sum.HexBytes(config), config}, {"blobs/sha256/" + sha256sum.HexBytes(manifest), manifest},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}
