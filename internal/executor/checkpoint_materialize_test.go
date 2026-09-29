package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type checkpointObjectReader struct {
	body []byte
}

func (r checkpointObjectReader) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, errors.New("stat is not used when materializing checkpoint objects")
}

func (r checkpointObjectReader) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(r.body)), nil
}

func encryptedCheckpointObject(t *testing.T, plaintext []byte, suffix string) []byte {
	t.Helper()
	var ciphertext bytes.Buffer
	if err := testCheckpointEncryptor(t).Encrypt(t.Context(), bytes.NewReader(plaintext), &ciphertext, checkpointPurpose(suffix)); err != nil {
		t.Fatal(err)
	}
	return ciphertext.Bytes()
}

func TestMaterializeCheckpointObjectDecryptsMatchingDescriptor(t *testing.T) {
	plaintext := []byte("runtime manifest")
	ciphertext := encryptedCheckpointObject(t, plaintext, "manifest")
	artifact := workerapi.CheckpointArtifact{Digest: sha256sum.DigestBytes(ciphertext), SizeBytes: int64(len(ciphertext)), MediaType: cas.CheckpointVMConfigMediaType}
	path, err := materializeCheckpointObject(t.Context(), checkpointObjectReader{body: ciphertext}, testCheckpointEncryptor(t), artifact, "manifest", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("materialized plaintext = %q, want %q", got, plaintext)
	}
}

func TestMaterializeCheckpointObjectRejectsDescriptorMismatch(t *testing.T) {
	ciphertext := encryptedCheckpointObject(t, []byte("runtime manifest"), "manifest")
	for name, artifact := range map[string]workerapi.CheckpointArtifact{
		"digest": {Digest: sha256sum.DigestBytes([]byte("other object")), SizeBytes: int64(len(ciphertext)), MediaType: cas.CheckpointVMConfigMediaType},
		"size":   {Digest: sha256sum.DigestBytes(ciphertext), SizeBytes: int64(len(ciphertext)) + 1, MediaType: cas.CheckpointVMConfigMediaType},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := materializeCheckpointObject(t.Context(), checkpointObjectReader{body: ciphertext}, testCheckpointEncryptor(t), artifact, "manifest", t.TempDir())
			if err == nil || err.Error() != "checkpoint object descriptor mismatch" {
				t.Fatalf("materialize error = %v, want descriptor mismatch", err)
			}
		})
	}
}

func TestMaterializeCheckpointObjectRejectsOversizeCiphertext(t *testing.T) {
	ciphertext := encryptedCheckpointObject(t, []byte("runtime manifest"), "manifest")
	for _, test := range []struct {
		name      string
		body      []byte
		sizeBytes int64
		want      string
	}{
		{name: "beyond advertised size", body: ciphertext, sizeBytes: int64(len(ciphertext)) - 1, want: "checkpoint object descriptor mismatch"},
		{name: "trailing bytes", body: append(bytes.Clone(ciphertext), 0), sizeBytes: int64(len(ciphertext)), want: "trailing checkpoint ciphertext after end record"},
	} {
		t.Run(test.name, func(t *testing.T) {
			artifact := workerapi.CheckpointArtifact{Digest: sha256sum.DigestBytes(ciphertext), SizeBytes: test.sizeBytes, MediaType: cas.CheckpointVMConfigMediaType}
			_, err := materializeCheckpointObject(t.Context(), checkpointObjectReader{body: test.body}, testCheckpointEncryptor(t), artifact, "manifest", t.TempDir())
			if err == nil || err.Error() != test.want {
				t.Fatalf("materialize error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMaterializeCheckpointObjectRequiresStorageAndEncryption(t *testing.T) {
	artifact := workerapi.CheckpointArtifact{Digest: sha256sum.DigestBytes([]byte("object")), SizeBytes: 1, MediaType: cas.CheckpointVMConfigMediaType}
	for name, call := range map[string]func() error{
		"storage": func() error {
			_, err := materializeCheckpointObject(t.Context(), nil, testCheckpointEncryptor(t), artifact, "manifest", t.TempDir())
			return err
		},
		"encryption": func() error {
			_, err := materializeCheckpointObject(t.Context(), checkpointObjectReader{}, nil, artifact, "manifest", t.TempDir())
			return err
		},
	} {
		if err := call(); err == nil || err.Error() != "checkpoint storage and encryption are required" {
			t.Fatalf("%s: materialize error = %v", name, err)
		}
	}
}
