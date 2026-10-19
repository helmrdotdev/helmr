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
	return cas.NewVerifyingReadCloser(io.NopCloser(bytes.NewReader(r.transform(raw))), digest), nil
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
	for _, fault := range []string{"none", "truncated", "extra", "corrupt", "other valid ciphertext", "purpose", "size", "bound"} {
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
			case "truncated", "extra", "corrupt", "other valid ciphertext":
				reader = checkpointObjectReader{Reader: store, transform: func(raw []byte) []byte {
					switch fault {
					case "truncated":
						return raw[:len(raw)-1]
					case "extra":
						return append(raw, 1)
					case "other valid ciphertext":
						var replacement bytes.Buffer
						if err := cipher.Encrypt(t.Context(), strings.NewReader(plaintext), &replacement, "checkpoint/memory"); err != nil {
							t.Fatal(err)
						}
						return replacement.Bytes()
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
		{"missing object", os.ErrNotExist, false}, {"digest mismatch", cas.ErrDigestMismatch, true},
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

	for _, statFailure := range []bool{false, true} {
		reader := &missingCheckpointReader{Reader: store, missing: true, statFailure: statFailure}
		_, err := downloadCheckpointObject(t.Context(), reader, cipher, t.TempDir(), "checkpoint", computercheckpoint.RuntimeObject{Role: "memory", Object: object}, 5)
		if !errors.Is(err, os.ErrNotExist) || errors.Is(err, errInvalidCheckpoint) {
			t.Fatalf("remote absence discarded checkpoint: %v", err)
		}
		reader.missing = false
		path, err := downloadCheckpointObject(t.Context(), reader, cipher, t.TempDir(), "checkpoint", computercheckpoint.RuntimeObject{Role: "memory", Object: object}, 5)
		if err != nil {
			t.Fatal(err)
		}
		if restored, err := os.ReadFile(path); err != nil || string(restored) != "state" {
			t.Fatalf("repaired payload: %q %v", restored, err)
		}
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

// Availability can be repaired without replacing the authenticated checkpoint.
type missingCheckpointReader struct {
	cas.Reader
	missing, statFailure bool
}

func (r *missingCheckpointReader) Stat(ctx context.Context, digest string) (cas.Object, error) {
	if r.missing && r.statFailure {
		return cas.Object{}, os.ErrNotExist
	}
	return r.Reader.Stat(ctx, digest)
}
func (r *missingCheckpointReader) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	if r.missing {
		return nil, os.ErrNotExist
	}
	return r.Reader.Get(ctx, digest)
}

// Changes to stored ciphertext after its stream digest was checked must still
// fail authentication. A canceled transport completion must not be called corrupt.
func TestCheckpointDownloadAuthenticatesStoredBytesAndCancellation(t *testing.T) {
	cipher, _ := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, path, err := encryptCheckpointObject(t.Context(), cipher, t.TempDir(), "checkpoint", "memory", cas.CheckpointMemoryMediaType, strings.NewReader("state"), 5)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Put(t.Context(), object.MediaType, file)
	err = errors.Join(err, file.Close())
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"stored corruption", "cancel at close", "close failure"} {
		t.Run(mode, func(t *testing.T) {
			directory := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			reader := checkpointCloseHookStore{Reader: store, hook: func() error {
				switch mode {
				case "cancel at close":
					cancel()
					return nil
				case "close failure":
					return syscall.EIO
				default:
					entries, err := os.ReadDir(directory)
					if err != nil {
						return err
					}
					for _, entry := range entries {
						if strings.Contains(entry.Name(), "-download-") {
							f, err := os.OpenFile(directory+"/"+entry.Name(), os.O_RDWR, 0)
							if err != nil {
								return err
							}
							// The original stream hash is intact; damage an authenticated record.
							offset := int64(headerSize + 8 + nonceSize)
							var b [1]byte
							if _, err = f.ReadAt(b[:], offset); err == nil {
								b[0] ^= 1
								_, err = f.WriteAt(b[:], offset)
							}
							return errors.Join(err, f.Close())
						}
					}
					return errors.New("missing private ciphertext")
				}
			}}
			output, err := downloadCheckpointObject(ctx, reader, cipher, directory, "checkpoint", computercheckpoint.RuntimeObject{Role: "memory", Object: object}, 5)
			if output != "" {
				t.Fatal("returned unqualified plaintext")
			}
			want := map[string]error{"stored corruption": errInvalidCheckpoint, "cancel at close": context.Canceled, "close failure": syscall.EIO}[mode]
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if mode != "stored corruption" && errors.Is(err, errInvalidCheckpoint) {
				t.Fatalf("availability failure marked corrupt: %v", err)
			}
		})
	}
}

type checkpointCloseHookStore struct {
	cas.Reader
	hook func() error
}

func (s checkpointCloseHookStore) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	body, err := s.Reader.Get(ctx, digest)
	if err != nil {
		return nil, err
	}
	return checkpointCloseHookBody{ReadCloser: body, hook: s.hook}, nil
}

type checkpointCloseHookBody struct {
	io.ReadCloser
	hook func() error
}

func (b checkpointCloseHookBody) Close() error { return errors.Join(b.ReadCloser.Close(), b.hook()) }
