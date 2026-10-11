//go:build computerproof

package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestNativeStorageObservationIncludesSDKRetries(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "payload" {
			t.Errorf("request body=%q error=%v", body, err)
		}
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `<Error><Code>SlowDown</Code></Error>`)
			return
		}
		w.Header().Set("ETag", `"fixture"`)
	}))
	defer server.Close()
	records := make(chan nativeHTTPObservation, 4)
	client := awss3.New(awss3.Options{
		Region: "us-east-1", Credentials: aws.AnonymousCredentials{},
		BaseEndpoint: aws.String(server.URL), UsePathStyle: true,
		HTTPClient:                 nativeObservedClient{server.Client(), func(record nativeHTTPObservation) { records <- record }},
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = 3
			o.Backoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
		}),
	})
	_, err := client.PutObject(t.Context(), &awss3.PutObjectInput{Bucket: aws.String("fixture"), Key: aws.String("sha256/" + strings.Repeat("a", 64)), Body: strings.NewReader("payload")})
	if err != nil {
		t.Fatal(err)
	}
	invocation := ""
	for index := range 3 {
		select {
		case record := <-records:
			if record.ObjectDigest != "sha256:"+strings.Repeat("a", 64) || record.Operation != "PutObject" || record.InvocationID == "" || record.StartedAt.IsZero() || !strings.Contains(record.Attempt, fmt.Sprintf("attempt=%d", index+1)) {
				t.Fatalf("missing request/retry identity: %+v", record)
			}
			if index == 0 {
				invocation = record.InvocationID
			} else if record.InvocationID != invocation {
				t.Fatal("SDK retry changed invocation identity")
			}
			status := http.StatusServiceUnavailable
			if index == 2 {
				status = http.StatusOK
			}
			if record.Method != http.MethodPut || record.Status != status || record.RequestBytes != 7 || record.TransportError || (index < 2 && record.ResponseBytes == 0) {
				t.Fatalf("attempt %d: %+v", index, record)
			}
		case <-time.After(time.Second):
			t.Fatal("attempt observation missing")
		}
	}
	if attempts != 3 || len(records) != 0 {
		t.Fatalf("attempts=%d extra records=%d", attempts, len(records))
	}
}

type nativeHTTPClientFunc func(*http.Request) (*http.Response, error)

func (f nativeHTTPClientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeStorageObservationCountsPartialFailedBody(t *testing.T) {
	records := make(chan nativeHTTPObservation, 1)
	client := nativeObservedClient{nativeHTTPClientFunc(func(r *http.Request) (*http.Response, error) {
		var prefix [3]byte
		if _, err := io.ReadFull(r.Body, prefix[:]); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}), func(record nativeHTTPObservation) { records <- record }}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPut, "https://unused.invalid/", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(request); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	record := <-records
	if record.RequestBytes != 3 || record.ResponseBytes != 0 || !record.TransportError || record.Status != 0 {
		t.Fatalf("partial failure: %+v", record)
	}
}

func TestNativeStorageObservationCountsHTTPBodyReplay(t *testing.T) {
	records := make(chan nativeHTTPObservation, 1)
	client := nativeObservedClient{nativeHTTPClientFunc(func(r *http.Request) (*http.Response, error) {
		var prefix [3]byte
		if _, err := io.ReadFull(r.Body, prefix[:]); err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		replayed, err := r.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, replayed); err != nil {
			t.Fatal(err)
		}
		replayed.Close()
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}), func(record nativeHTTPObservation) { records <- record }}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://unused.invalid/", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	record := <-records
	if record.RequestBytes != 10 || record.RequestBodyReplays != 1 || record.TransportError {
		t.Fatalf("replayed body: %+v", record)
	}
}

type nativeClosingBody struct{ reading, closed chan struct{} }

func (b nativeClosingBody) Read(p []byte) (int, error) {
	close(b.reading)
	<-b.closed
	return copy(p, "abc"), io.EOF
}
func (b nativeClosingBody) Close() error { close(b.closed); return nil }

func TestNativeStorageObservationJoinsConcurrentReadAndClose(t *testing.T) {
	counts := make(chan int64, 2)
	underlying := nativeClosingBody{make(chan struct{}), make(chan struct{})}
	body := &nativeCountedBody{ReadCloser: underlying, finished: func(n int64) { counts <- n }}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.ReadAll(body) }()
	<-underlying.reading
	body.Close()
	<-done
	if count := <-counts; count != 3 || len(counts) != 0 {
		t.Fatalf("count=%d duplicate records=%d", count, len(counts))
	}
}
