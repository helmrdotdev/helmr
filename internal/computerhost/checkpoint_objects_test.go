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

// A canceled transport completion must not be called corrupt.
func TestCheckpointDownloadRequiresTransportCompletion(t *testing.T) {
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
	for _, mode := range []string{"cancel at close", "close failure"} {
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
					panic("unknown failure")
				}
			}}
			output, err := downloadCheckpointObject(ctx, reader, cipher, directory, "checkpoint", computercheckpoint.RuntimeObject{Role: "memory", Object: object}, 5)
			if output != "" {
				t.Fatal("returned unqualified plaintext")
			}
			want := map[string]error{"cancel at close": context.Canceled, "close failure": syscall.EIO}[mode]
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if errors.Is(err, errInvalidCheckpoint) {
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

func TestCheckpointDownloadVerifiesStreamBeforeClassifyingEncryptedContents(t *testing.T) {
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var original bytes.Buffer
	if err := cipher.Encrypt(t.Context(), strings.NewReader("state"), &original, "checkpoint/memory"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"authenticated corruption", "verified unavailable key", "unverified key", "interrupted key", "invalid header close failure"} {
		t.Run(mode, func(t *testing.T) {
			store, err := cas.NewFile(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			raw := bytes.Clone(original.Bytes())
			want := errInvalidCheckpoint
			switch mode {
			case "authenticated corruption":
				raw[headerSize+8+nonceSize] ^= 1
			case "verified unavailable key":
				raw[len(magic)] ^= 1
				want = ErrCheckpointKeyUnavailable
			case "interrupted key":
				want = syscall.ECONNRESET
			case "invalid header close failure":
				want = syscall.EIO
			}
			object, err := store.Put(t.Context(), cas.CheckpointMemoryMediaType, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			var reader cas.Reader = store
			switch mode {
			case "unverified key":
				reader = checkpointObjectReader{Reader: store, transform: func(p []byte) []byte { p[len(magic)] ^= 1; return p }}
			case "interrupted key":
				header := bytes.Clone(raw[:headerSize])
				header[len(magic)] ^= 1
				reader = checkpointCustomBodyStore{Reader: store, body: cas.NewVerifyingReadCloser(io.NopCloser(io.MultiReader(bytes.NewReader(header), checkpointTransportFailure{})), object.Digest)}
			case "invalid header close failure":
				// Use a valid CAS object with an invalid encryption header so the
				// close error, rather than a digest failure, is the transport result.
				raw[0] ^= 1
				object, err = store.Put(t.Context(), cas.CheckpointMemoryMediaType, bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				reader = checkpointCloseHookStore{Reader: store, hook: func() error { return syscall.EIO }}
			}
			directory := t.TempDir()
			path, err := downloadCheckpointObject(t.Context(), reader, cipher, directory, "checkpoint", computercheckpoint.RuntimeObject{Role: "memory", Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}, 5)
			if path != "" || !errors.Is(err, want) {
				t.Fatalf("path %q, error %v, want %v", path, err, want)
			}
			if errors.Is(err, errInvalidCheckpoint) != errors.Is(want, errInvalidCheckpoint) {
				t.Fatalf("invalid classification: %v", err)
			}
			if errors.Is(err, ErrCheckpointKeyUnavailable) != errors.Is(want, ErrCheckpointKeyUnavailable) {
				t.Fatalf("key classification: %v", err)
			}
		})
	}
}

type checkpointCustomBodyStore struct {
	cas.Reader
	body io.ReadCloser
}

func (s checkpointCustomBodyStore) Get(context.Context, string) (io.ReadCloser, error) {
	return s.body, nil
}
