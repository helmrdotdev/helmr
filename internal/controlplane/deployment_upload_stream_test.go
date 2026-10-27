package controlplane

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeploymentUploadEarlyRejectionDoesNotDrainBody(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadRequest, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(deploymentUploadTransport(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })))
			defer server.Close()
			conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			_, err = fmt.Fprint(conn, "PUT /v1/deployment-bundles/bundle/objects/object HTTP/1.1\r\nHost: test\r\nContent-Length: 1024\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != status || !response.Close {
				t.Fatalf("response=%s close=%v", response.Status, response.Close)
			}
		})
	}
}

func TestDeploymentUploadOverridesOrdinaryReadTimeout(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(fmt.Sprintf("http2=%v", h2), func(t *testing.T) {
			result := make(chan error, 1)
			server := httptest.NewUnstartedServer(deploymentUploadTransport(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Real preflight can exceed the ordinary body read timeout.
				time.Sleep(300 * time.Millisecond)
				stream, err := newDeploymentUploadStream(w, r, time.Second, 3*time.Second)
				if err != nil {
					result <- err
					w.WriteHeader(500)
					return
				}
				data, err := io.ReadAll(stream)
				stream.close()
				if err == nil && string(data) != "slow" {
					err = fmt.Errorf("body=%q", data)
				}
				result <- err
				w.WriteHeader(http.StatusNoContent)
			})))
			server.EnableHTTP2 = h2
			server.Config.ReadTimeout = 200 * time.Millisecond
			server.StartTLS()
			defer server.Close()
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, server.URL+"/v1/deployment-bundles/b/objects/o", &slowUploadReader{data: []byte("slow"), delay: 300 * time.Millisecond})
			request.ContentLength = 4
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusNoContent || (h2 && response.ProtoMajor != 2) {
				t.Fatalf("response=%s %s", response.Proto, response.Status)
			}
		})
	}
}

type slowUploadReader struct {
	data  []byte
	delay time.Duration
}

func (r *slowUploadReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

type observedUploadBody struct {
	io.ReadCloser
	returned *atomic.Bool
	late     *atomic.Int32
}

func (b observedUploadBody) Read(p []byte) (int, error) {
	if b.returned.Load() {
		b.late.Add(1)
	}
	return b.ReadCloser.Read(p)
}

func TestDeploymentUploadCancellationJoinsBodyReads(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		for _, mode := range []string{"client-stall", "backend-stall", "response-stall", "overall-deadline"} {
			t.Run(fmt.Sprintf("http2=%v/%s", h2, mode), func(t *testing.T) {
				var returned atomic.Bool
				var late atomic.Int32
				finished := make(chan error, 1)
				release := make(chan struct{})
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if mode == "response-stall" {
						_, _ = io.Copy(io.Discard, r.Body)
					}
					<-release
				}))
				defer backend.Close()
				defer close(release)
				server := httptest.NewUnstartedServer(deploymentUploadTransport(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					r.Body = observedUploadBody{ReadCloser: r.Body, returned: &returned, late: &late}
					idle, maximum := 150*time.Millisecond, 3*time.Second
					if mode == "overall-deadline" {
						idle = time.Second
						maximum = 120 * time.Millisecond
					}
					stream, err := newDeploymentUploadStream(w, r, idle, maximum)
					if err != nil {
						finished <- err
						w.WriteHeader(500)
						return
					}
					if mode == "backend-stall" || mode == "response-stall" {
						out, _ := http.NewRequestWithContext(stream.ctx, http.MethodPut, backend.URL, stream)
						out.ContentLength = r.ContentLength
						response, requestErr := backend.Client().Do(out)
						if response != nil {
							response.Body.Close()
						}
						err = requestErr
					} else {
						_, err = io.Copy(io.Discard, stream)
					}
					stream.close()
					// Simulate a transport that attempts one last asynchronous read.
					if _, readErr := stream.Read(make([]byte, 1)); readErr == nil {
						err = fmt.Errorf("read remained open after close")
					}
					returned.Store(true)
					finished <- err
					w.WriteHeader(http.StatusServiceUnavailable)
				})))
				server.EnableHTTP2 = h2
				server.StartTLS()
				defer server.Close()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var body io.Reader
				length := int64(16 << 20)
				if mode == "client-stall" {
					body = &slowUploadReader{data: []byte("x"), delay: 500 * time.Millisecond}
					length = 1
				} else if mode == "overall-deadline" {
					body = &slowUploadReader{data: []byte("abcdefgh"), delay: 40 * time.Millisecond}
					length = 8
				} else if mode == "response-stall" {
					body = bytes.NewReader([]byte("body"))
					length = 4
				} else {
					body = io.LimitReader(zeroUploadReader{}, length)
				}
				request, _ := http.NewRequestWithContext(ctx, http.MethodPut, server.URL+"/v1/deployment-bundles/b/objects/o", body)
				request.ContentLength = length
				response, _ := server.Client().Do(request)
				if response != nil {
					response.Body.Close()
				}
				select {
				case err := <-finished:
					if err == nil {
						t.Fatal("stalled upload succeeded")
					}
				case <-ctx.Done():
					t.Fatal("upload did not release its reader")
				}
				time.Sleep(50 * time.Millisecond)
				if late.Load() != 0 {
					t.Fatalf("reads after handler returned=%d", late.Load())
				}
			})
		}
	}
}

type zeroUploadReader struct{}

func (zeroUploadReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
