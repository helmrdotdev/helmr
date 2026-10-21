package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestS3CancelledBodyDoesNotReportContentMismatch(t *testing.T) {
	content := []byte("complete checkpoint ciphertext")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content[:3])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := awss3.New(awss3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(server.URL),
		UsePathStyle: true, Credentials: aws.AnonymousCredentials{},
		HTTPClient: server.Client(),
	})
	store := &Store{client: client, bucket: "bucket", prefix: "cas"}
	body, err := store.Get(ctx, sha256sum.DigestBytes(content))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if _, err := io.ReadFull(body, make([]byte, 3)); err != nil {
		t.Fatal(err)
	}
	cancel()
	_, readErr := io.ReadAll(body)
	err = errors.Join(readErr, body.Close())
	if !errors.Is(err, context.Canceled) || errors.Is(err, cas.ErrDigestMismatch) {
		t.Fatalf("cancelled HTTP body reported content state: %v", err)
	}
}
