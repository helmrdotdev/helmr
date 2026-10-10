//go:build computerproof

package s3

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// The S3 constructor resolves its ordinary client before calling options. Keep
// that exact client/transport and observe Do below SDK retries. These are body
// bytes read by the client, not acknowledged network bytes or HTTP/TLS overhead.
// Only the explicitly instrumented computerproof artifact includes this wrapper.
var nativeDigestPath = regexp.MustCompile(`/sha256/([0-9a-f]{64}|[0-9a-f]{2}/[0-9a-f]{62})$`)

var nativeStorageLogger = slog.New(slog.NewJSONHandler(os.Stderr, nil))

func observeNativeHTTP(options *awss3.Options, bucket, prefix string) {
	options.HTTPClient = nativeObservedClient{options.HTTPClient, func(record nativeHTTPObservation) {
		nativeStorageLogger.Info("Native storage request attempt", "method", record.Method, "status", record.Status,
			"operation", record.Operation, "object_digest", record.ObjectDigest, "store_bucket", bucket, "store_prefix", prefix,
			"invocation_id", record.InvocationID, "request_attempt", record.Attempt, "started_at", record.StartedAt,
			"request_body_bytes_read", record.RequestBytes, "response_body_bytes_read", record.ResponseBytes,
			"request_body_replays", record.RequestBodyReplays,
			"duration_ms", record.DurationMS, "transport_error", record.TransportError)
	}}
}

type nativeHTTPObservation struct {
	Operation, InvocationID, Attempt, ObjectDigest string
	StartedAt                                      time.Time
	Method                                         string
	Status                                         int
	RequestBytes, ResponseBytes                    int64
	RequestBodyReplays                             int64
	DurationMS                                     float64
	TransportError                                 bool
}

type nativeObservedClient struct {
	next   aws.HTTPClient
	record func(nativeHTTPObservation)
}

func (c nativeObservedClient) Do(request *http.Request) (*http.Response, error) {
	started := time.Now()
	var responseDone atomic.Bool
	var requestBytes, responseBytes atomic.Int64
	var requestBodies, bodyReplays atomic.Int64
	var once sync.Once
	// These SDK-generated correlation headers contain no authorization or object
	// data. Do not log arbitrary request headers, URLs, keys or payloads.
	result := nativeHTTPObservation{Method: request.Method, StartedAt: started.UTC(),
		Operation:    awsmiddleware.GetOperationName(request.Context()),
		InvocationID: request.Header.Get("Amz-Sdk-Invocation-Id"), Attempt: request.Header.Get("Amz-Sdk-Request")}
	if match := nativeDigestPath.FindStringSubmatch(request.URL.Path); match != nil {
		result.ObjectDigest = "sha256:" + strings.ReplaceAll(match[1], "/", "")
	}
	finish := func() {
		if responseDone.Load() && requestBodies.Load() == 0 {
			once.Do(func() {
				result.RequestBytes, result.ResponseBytes = requestBytes.Load(), responseBytes.Load()
				result.RequestBodyReplays = bodyReplays.Load()
				result.DurationMS = float64(time.Since(started)) / float64(time.Millisecond)
				c.record(result)
			})
		}
	}
	copy := request.Clone(request.Context())
	countBody := func(body io.ReadCloser) io.ReadCloser {
		if body == nil || body == http.NoBody {
			return body
		}
		requestBodies.Add(1)
		return &nativeCountedBody{ReadCloser: body, finished: func(n int64) {
			requestBytes.Add(n)
			requestBodies.Add(-1)
			finish()
		}}
	}
	copy.Body = countBody(request.Body)
	if request.GetBody != nil {
		copy.GetBody = func() (io.ReadCloser, error) {
			body, err := request.GetBody()
			if err != nil {
				return body, err
			}
			bodyReplays.Add(1)
			return countBody(body), nil
		}
	}
	response, err := c.next.Do(copy)
	result.TransportError = err != nil
	if response != nil {
		result.Status = response.StatusCode
	}
	if response != nil && response.Body != nil && err == nil {
		response.Body = &nativeCountedBody{ReadCloser: response.Body, finished: func(n int64) {
			responseBytes.Store(n)
			responseDone.Store(true)
			finish()
		}}
	} else {
		responseDone.Store(true)
		finish()
	}
	return response, err
}

// Close can race a blocked Read during cancellation. Wait for already-entered
// reads before reporting their final count; never wait on the request path.
type nativeCountedBody struct {
	io.ReadCloser
	mu       sync.Mutex
	bytes    int64
	readers  int
	closed   bool
	reported bool
	finished func(int64)
}

func (b *nativeCountedBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.readers++
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	b.bytes += int64(n)
	b.readers--
	if err == io.EOF {
		b.closed = true
	}
	done, size := b.takeLocked()
	b.mu.Unlock()
	if done {
		b.finished(size)
	}
	return n, err
}

func (b *nativeCountedBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	b.closed = true
	done, size := b.takeLocked()
	b.mu.Unlock()
	if done {
		b.finished(size)
	}
	return err
}

func (b *nativeCountedBody) takeLocked() (bool, int64) {
	if b.closed && b.readers == 0 && !b.reported {
		b.reported = true
		return true, b.bytes
	}
	return false, 0
}
