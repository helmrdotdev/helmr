package computerhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
)

type checkpointObjectReader struct {
	cas.Reader
	transform func([]byte) []byte
}

func (r checkpointObjectReader) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	body, err := r.Reader.Get(ctx, digest)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(body)
	errClose := body.Close()
	if err != nil {
		return nil, err
	}
	if errClose != nil {
		return nil, errClose
	}
	return io.NopCloser(bytes.NewReader(r.transform(raw))), nil
}

func TestCheckpointObjectsAuthenticateExactBoundedFiles(t *testing.T) {
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plaintext := strings.Repeat("retained native state", 4096)
	object, path, err := encryptCheckpointObject(t.Context(), cipher, t.TempDir(), "checkpoint", "memory", cas.CheckpointMemoryMediaType, strings.NewReader(plaintext), int64(len(plaintext)))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	published, err := store.Put(t.Context(), object.MediaType, file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if published.Digest != object.Digest || published.SizeBytes != object.SizeBytes {
		t.Fatal("staged descriptor changed at upload")
	}
	for _, fault := range []string{"none", "truncated", "extra", "corrupt", "purpose", "size", "bound"} {
		t.Run(fault, func(t *testing.T) {
			var reader cas.Reader = store
			id := "checkpoint"
			o := computercheckpoint.RuntimeObject{Role: "memory", Object: object}
			limit := int64(len(plaintext))
			switch fault {
			case "purpose":
				id = "other-checkpoint"
			case "size":
				o.SizeBytes++
			case "bound":
				limit--
			case "truncated", "extra", "corrupt":
				reader = checkpointObjectReader{Reader: store, transform: func(raw []byte) []byte {
					switch fault {
					case "truncated":
						return raw[:len(raw)-1]
					case "extra":
						return append(raw, 1)
					default:
						raw[len(raw)/2] ^= 1
						return raw
					}
				}}
			}
			path, err := downloadCheckpointObject(t.Context(), reader, cipher, t.TempDir(), id, o, limit)
			if fault != "none" {
				if !errors.Is(err, errInvalidCheckpoint) {
					t.Fatalf("invalid checkpoint classification: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != plaintext {
				t.Fatal("restored bytes changed")
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatal("checkpoint plaintext is not private")
			}
		})
	}
	if _, _, err := encryptCheckpointObject(t.Context(), cipher, t.TempDir(), "checkpoint", "memory", cas.CheckpointMemoryMediaType, strings.NewReader(plaintext), int64(len(plaintext)-1)); err == nil {
		t.Fatal("oversized capture accepted")
	}
}

// Local storage and transport failures must not withdraw retained process state.
func TestCheckpointDownloadSeparatesInvalidContentsFromReadFailures(t *testing.T) {
	cipher, _ := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, path, err := encryptCheckpointObject(t.Context(), cipher, t.TempDir(), "checkpoint", "memory", cas.CheckpointMemoryMediaType, strings.NewReader("state"), 5)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := store.Put(t.Context(), object.MediaType, f); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		fault   error
		invalid bool
	}{
		{"missing object", os.ErrNotExist, true}, {"digest mismatch", cas.ErrDigestMismatch, true},
		{"unavailable", cas.ErrUnavailable, false}, {"permission", os.ErrPermission, false},
		{"io", syscall.EIO, false}, {"cancelled", context.Canceled, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := checkpointReadFailure{Reader: store, err: test.fault}
			_, err := downloadCheckpointObject(t.Context(), reader, cipher, t.TempDir(), "checkpoint", computercheckpoint.RuntimeObject{Role: "memory", Object: object}, 5)
			if !errors.Is(err, test.fault) || errors.Is(err, errInvalidCheckpoint) != test.invalid {
				t.Fatalf("classification: %v", err)
			}
		})
	}

	_, err = downloadCheckpointObject(t.Context(), interruptedVerifiedCheckpoint{Reader: store}, cipher, t.TempDir(), "checkpoint", computercheckpoint.RuntimeObject{Role: "memory", Object: object}, 5)
	if !errors.Is(err, syscall.ECONNRESET) || errors.Is(err, errInvalidCheckpoint) {
		t.Fatalf("interrupted verified stream marked corrupt: %v", err)
	}
	_, err = downloadCheckpointObject(t.Context(), store, cipher, t.TempDir()+"/absent", "checkpoint", computercheckpoint.RuntimeObject{Role: "memory", Object: object}, 5)
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, errInvalidCheckpoint) {
		t.Fatalf("local absence marked invalid: %v", err)
	}
}

type checkpointReadFailure struct {
	cas.Reader
	err error
}

func (r checkpointReadFailure) Get(context.Context, string) (io.ReadCloser, error) { return nil, r.err }

type interruptedVerifiedCheckpoint struct{ cas.Reader }

func (r interruptedVerifiedCheckpoint) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	return cas.NewVerifyingReadCloser(io.NopCloser(io.MultiReader(strings.NewReader("prefix"), checkpointTransportFailure{})), digest), nil
}

type checkpointTransportFailure struct{}

func (checkpointTransportFailure) Read([]byte) (int, error) { return 0, syscall.ECONNRESET }
