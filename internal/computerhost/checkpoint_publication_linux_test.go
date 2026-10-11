package computerhost

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestCheckpointEncryptedFilesPublishThroughImmutableStore(t *testing.T) {
	config := filepath.Join(t.TempDir(), "empty-aws-config")
	if err := os.WriteFile(config, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"AWS_ACCESS_KEY_ID": "fixture-access", "AWS_SECRET_ACCESS_KEY": "fixture-secret", "AWS_SESSION_TOKEN": "",
		"AWS_REGION": "us-east-1", "AWS_PROFILE": "", "AWS_CONFIG_FILE": config, "AWS_SHARED_CREDENTIALS_FILE": config,
		"AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(name, value)
	}
	cipher, err := NewCheckpointEncryptor(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct{ role, media string }{
		{"vm_config", cas.CheckpointVMConfigMediaType}, {"vm_state", cas.CheckpointVMStateMediaType},
		{"memory", cas.CheckpointMemoryMediaType}, {"scratch_disk", cas.CheckpointScratchDiskMediaType},
	} {
		t.Run(item.role, func(t *testing.T) {
			plaintext := []byte("retained " + item.role)
			object, path, err := encryptCheckpointObject(t.Context(), cipher, t.TempDir(), "checkpoint", item.role, item.media, bytes.NewReader(plaintext), int64(len(plaintext)))
			if err != nil {
				t.Fatal(err)
			}
			type upload struct {
				method, path, media, condition string
				body                           []byte
				err                            error
			}
			uploaded := make(chan upload, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				select {
				case uploaded <- upload{r.Method, r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("If-None-Match"), body, err}:
				default:
					http.Error(w, "unexpected additional upload request", http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			store, err := cass3.NewImmutable(t.Context(), "s3://checkpoint-test/runtime?endpoint="+url.QueryEscape(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			expected := cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
			published, err := store.Publish(t.Context(), expected, file)
			if err != nil {
				t.Fatal(err)
			}
			if err := cas.RequireExact(published, expected); err != nil {
				t.Fatal(err)
			}
			request := <-uploaded
			if request.err != nil || request.method != http.MethodPut || request.path != "/checkpoint-test/"+published.Key || request.media != object.MediaType || request.condition != "*" {
				t.Fatalf("immutable upload request: %+v", request)
			}
			if int64(len(request.body)) != object.SizeBytes || sha256sum.DigestBytes(request.body) != object.Digest {
				t.Fatal("published ciphertext differs from the encrypted descriptor")
			}
			var restored bytes.Buffer
			if err := cipher.Decrypt(t.Context(), bytes.NewReader(request.body), &restored, "checkpoint/"+item.role); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(restored.Bytes(), plaintext) {
				t.Fatal("published checkpoint plaintext changed")
			}
		})
	}
}
