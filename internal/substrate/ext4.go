package substrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"io"
	"os"
	"os/exec"
	"strings"
)

const (
	ext4Features = "sparse_super,large_file,filetype,resize_inode,dir_index,ext_attr,has_journal,extent,huge_file,flex_bg,metadata_csum,metadata_csum_seed,64bit,dir_nlink,extra_isize,orphan_file"
)

func createExt4(
	ctx context.Context,
	mkfs string,
	mke2fsConfig string,
	filesystem *oci.Filesystem,
	path string,
	sizeBytes int64,
	key string,
) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create substrate ext4: %w", err)
	}
	if err := file.Truncate(sizeBytes); err != nil {
		_ = file.Close()
		return fmt.Errorf("size substrate ext4: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close substrate ext4: %w", err)
	}
	uuid := deterministicUUID(key)
	cmd := exec.CommandContext(
		ctx,
		mkfs,
		"-F",
		"-q",
		"-t", "ext4",
		"-b", "4096",
		"-I", "256",
		"-i", "16384",
		"-m", "0",
		"-O", ext4Features,
		"-U", uuid,
		"-E", "hash_seed="+uuid+",lazy_itable_init=0,lazy_journal_init=0,nodiscard,root_owner=0:0",
		"-d", "-",
		path,
	)
	cmd.Env = []string{
		"LC_ALL=C.UTF-8",
		"LANG=C.UTF-8",
		"TZ=UTC",
		// SOURCE_DATE_EPOCH also clamps authored mtimes. Use a nonzero fixed
		// filesystem clock without that clamping mode.
		"E2FSPROGS_FAKE_TIME=1",
		"MKE2FS_CONFIG=" + mke2fsConfig,
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open mkfs substrate input: %w", err)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start mkfs substrate ext4: %w", err)
	}
	archiveErr := filesystem.WriteArchive(stdin)
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("mkfs substrate ext4: %w: %s", waitErr, strings.TrimSpace(output.String()))
	}
	if archiveErr != nil {
		return archiveErr
	}
	if closeErr != nil {
		return fmt.Errorf("close mkfs substrate input: %w", closeErr)
	}
	return nil
}

func deterministicUUID(key string) string {
	sum := sha256.Sum256([]byte(key))
	raw := sum[:16]
	raw[6] = (raw[6] & 0x0f) | 0x50
	raw[8] = (raw[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(raw)
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:32]
}

func fileDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	digest := sha256.New()
	size, err := io.Copy(digest, file)
	if err != nil {
		return "", 0, err
	}
	return sha256sum.DigestHash(digest), size, nil
}
