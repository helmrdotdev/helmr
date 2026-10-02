package workerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type preparationCall struct {
	name, path string
	response   any
	call       func(context.Context, *Client) error
}

func preparationCalls() []preparationCall {
	id := uuid.NewV7().String()
	key := workerapi.ComputerKeyMaterial{Scope: "scope", ID: id, Key: bytes.Repeat([]byte{1}, 32)}
	root := disk.VersionRoot{FormatVersion: 1, LogicalBytes: 4096, Offset: 128, Pack: disk.VersionPack{Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 512, Rank: 2}, Page: disk.VersionPage{Digest: "sha256:" + strings.Repeat("b", 64), Salt: strings.Repeat("c", 64), KeyID: id, Kind: 3, Count: 1, SizeBytes: 64}}
	return []preparationCall{
		{"key", "/worker/v1/run/computer-instances/initialization/key", key, func(ctx context.Context, c *Client) error {
			m, e := c.InitialComputerKey(ctx, workerapi.InitialComputerKeyRequest{ComputerInstanceID: id, DesiredVersion: 7})
			defer clear(m.Key)
			if e == nil && (m.ID != id || len(m.Key) != 32) {
				return errors.New("wrong key")
			}
			return e
		}},
		{"source", "/worker/v1/run/computer-instances/computer-source", workerapi.ComputerSourceMaterial{VersionID: id, WriteKeyID: id, Root: root, Keys: []workerapi.ComputerKeyMaterial{key}}, func(ctx context.Context, c *Client) error {
			m, e := c.ComputerSource(ctx, workerapi.ComputerSourceRequest{ComputerInstanceID: id, DesiredVersion: 7})
			defer m.Clear()
			if e == nil && (m.VersionID != id || len(m.Keys) != 1) {
				return errors.New("wrong source")
			}
			return e
		}},
		{"register", "/worker/v1/run/computer-instances/initialization/objects/register", struct{}{}, func(ctx context.Context, c *Client) error {
			return c.RegisterInitialComputerObject(ctx, workerapi.InitialComputerObjectRequest{ComputerInstanceID: id, DesiredVersion: 7})
		}},
		{"certify", "/worker/v1/run/computer-instances/initialization/objects/certify", struct{}{}, func(ctx context.Context, c *Client) error {
			return c.CertifyInitialComputerObject(ctx, workerapi.InitialComputerObjectRequest{ComputerInstanceID: id, DesiredVersion: 7})
		}},
		{"publish", "/worker/v1/run/computer-instances/initialization/version", workerapi.InitialComputerVersionResponse{ComputerID: id, VersionID: id}, func(ctx context.Context, c *Client) error {
			m, e := c.PublishInitialComputerVersion(ctx, workerapi.InitialComputerVersionRequest{ComputerInstanceID: id, DesiredVersion: 7, Root: root})
			if e == nil && (m.ComputerID != id || m.VersionID != id) {
				return errors.New("wrong publication")
			}
			return e
		}},
	}
}

func preparationClient(t *testing.T, url string, h *http.Client) *Client {
	t.Helper()
	c, e := New(url, WithHTTPClient(h), WithAuth(uuid.NewV7().String(), "fixture"), WithService(uuid.NewV7().String()))
	if e != nil {
		t.Fatal(e)
	}
	c.auth.credential = "credential"
	c.auth.expiresAt = time.Now().Add(time.Hour)
	return c
}

func TestPreparationRetriesIdenticalRequests(t *testing.T) {
	for _, operation := range preparationCalls() {
		for _, failure := range []string{"503", "disconnect", "truncated"} {
			t.Run(operation.name+"/"+failure, func(t *testing.T) {
				var mu sync.Mutex
				var bodies [][]byte
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != operation.path {
						t.Errorf("unexpected path %s", r.URL.Path)
						w.WriteHeader(404)
						return
					}
					body, _ := io.ReadAll(r.Body)
					mu.Lock()
					bodies = append(bodies, body)
					n := len(bodies)
					mu.Unlock()
					if n == 1 {
						switch failure {
						case "503":
							w.WriteHeader(503)
							return
						case "disconnect":
							conn, _, e := w.(http.Hijacker).Hijack()
							if e != nil {
								t.Error(e)
								return
							}
							conn.Close()
							return
						case "truncated":
							conn, b, e := w.(http.Hijacker).Hijack()
							if e != nil {
								t.Error(e)
								return
							}
							b.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\n{")
							b.Flush()
							conn.Close()
							return
						}
					}
					json.NewEncoder(w).Encode(operation.response)
				}))
				defer server.Close()
				if e := operation.call(t.Context(), preparationClient(t, server.URL, server.Client())); e != nil {
					t.Fatal(e)
				}
				mu.Lock()
				defer mu.Unlock()
				if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
					t.Fatalf("replay bodies=%q", bodies)
				}
			})
		}
	}
}

func TestPreparationDoesNotRetryRejectionsOrMalformedResponses(t *testing.T) {
	for _, operation := range preparationCalls() {
		for _, status := range []int{400, 401, 403, 409, 410, 422, 500, 502, 504, 200} {
			t.Run(operation.name+"/"+strconv.Itoa(status), func(t *testing.T) {
				calls := 0
				client := preparationClient(t, "http://127.0.0.1", &http.Client{Transport: preparationRoundTrip(func(r *http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader("invalid-json")), Header: make(http.Header)}, nil
				})})
				// A persistent 401 may refresh once; this fixture stops at the credential endpoint.
				e := operation.call(t.Context(), client)
				if e == nil {
					t.Fatal("invalid response accepted")
				}
				want := 1
				if status == 401 {
					want = 2
				}
				if calls != want {
					t.Fatalf("calls=%d want=%d err=%v", calls, want, e)
				}
				if status == 409 && !httpclient.IsStatus(e, 409) {
					t.Fatalf("conflict identity lost: %v", e)
				}
			})
		}
	}
}

type preparationRoundTrip func(*http.Request) (*http.Response, error)

func (f preparationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPreparationRetryLimitsAndCancellation(t *testing.T) {
	for _, operation := range preparationCalls() {
		for _, stop := range []string{"limit", "cancel", "deadline"} {
			t.Run(operation.name+"/"+stop, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					if stop == "deadline" {
						var end context.CancelFunc
						ctx, end = context.WithTimeout(ctx, 50*time.Millisecond)
						defer end()
					}
					calls := 0
					client := preparationClient(t, "http://127.0.0.1", &http.Client{Transport: preparationRoundTrip(func(r *http.Request) (*http.Response, error) {
						calls++
						if stop == "cancel" {
							cancel()
						}
						return &http.Response{StatusCode: 503, Status: "503", Body: io.NopCloser(strings.NewReader("SECRET-MARKER")), Header: make(http.Header)}, nil
					})})
					e := operation.call(ctx, client)
					want := 1
					if stop == "limit" {
						want = 3
					}
					if calls != want || e == nil {
						t.Fatalf("calls=%d want=%d err=%v", calls, want, e)
					}
					if stop == "cancel" && !errors.Is(e, context.Canceled) {
						t.Fatalf("cancel cause lost: %v", e)
					}
					if stop == "deadline" && !errors.Is(e, context.DeadlineExceeded) {
						t.Fatalf("deadline cause lost: %v", e)
					}
					if strings.Contains(e.Error(), "SECRET-MARKER") {
						t.Fatal("sensitive error body leaked")
					}
				})
			})
		}
	}
}

func TestPreparationRefreshThenRetriesWithoutChangingPayload(t *testing.T) {
	for _, operation := range preparationCalls() {
		t.Run(operation.name, func(t *testing.T) {
			var mu sync.Mutex
			var bodies [][]byte
			credentials := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/worker/v1/instance/credential" {
					credentials++
					json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{Credential: "renewed", ExpiresInSeconds: 3600})
					return
				}
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, body)
				switch len(bodies) {
				case 1:
					w.WriteHeader(401)
					return
				case 2:
					w.WriteHeader(503)
					return
				}
				if r.Header.Get("authorization") != "Bearer renewed" {
					t.Error("refreshed authority missing")
				}
				json.NewEncoder(w).Encode(operation.response)
			}))
			defer server.Close()
			if e := operation.call(t.Context(), preparationClient(t, server.URL, server.Client())); e != nil {
				t.Fatal(e)
			}
			mu.Lock()
			defer mu.Unlock()
			if credentials != 1 || len(bodies) != 3 || !bytes.Equal(bodies[0], bodies[1]) || !bytes.Equal(bodies[1], bodies[2]) {
				t.Fatalf("credentials=%d bodies=%q", credentials, bodies)
			}
		})
	}
}

func TestPreparationConflictWithLostErrorBodyIsNotRetried(t *testing.T) {
	for _, operation := range preparationCalls() {
		t.Run(operation.name, func(t *testing.T) {
			calls := 0
			client := preparationClient(t, "http://127.0.0.1", &http.Client{Transport: preparationRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 409, Status: "409", Body: io.NopCloser(preparationBrokenBody{}), Header: make(http.Header)}, nil
			})})
			err := operation.call(t.Context(), client)
			if calls != 1 || !httpclient.IsStatus(err, 409) {
				t.Fatalf("conflict was retried or lost: calls=%d err=%v", calls, err)
			}
		})
	}
}

type preparationBrokenBody struct{}

func (preparationBrokenBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestPreparationRefreshesOnlyOnce(t *testing.T) {
	for _, operation := range preparationCalls() {
		t.Run(operation.name, func(t *testing.T) {
			var mu sync.Mutex
			requests, credentials := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/worker/v1/instance/credential" {
					credentials++
					json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{Credential: "renewed", ExpiresInSeconds: 3600})
					return
				}
				requests++
				if requests == 2 {
					w.WriteHeader(503)
				} else {
					w.WriteHeader(401)
				}
			}))
			defer server.Close()
			err := operation.call(t.Context(), preparationClient(t, server.URL, server.Client()))
			mu.Lock()
			defer mu.Unlock()
			if requests != 3 || credentials != 1 || !httpclient.IsStatus(err, 401) {
				t.Fatalf("extra refresh: requests=%d credentials=%d err=%v", requests, credentials, err)
			}
		})
	}
}

func TestPreparationSensitiveTransportDetailsAreNotRetained(t *testing.T) {
	for _, operation := range preparationCalls()[:2] {
		t.Run(operation.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client := preparationClient(t, "http://127.0.0.1", &http.Client{Transport: preparationRoundTrip(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("SECRET-MARKER") })})
				err := operation.call(t.Context(), client)
				if calls != 3 || !errors.Is(err, httpclient.ErrSensitiveTransport) || strings.Contains(err.Error(), "SECRET-MARKER") {
					t.Fatalf("sensitive transport result: calls=%d err=%v", calls, err)
				}
			})
		})
	}
}

func TestPreparationSnapshotsPublicationRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		id := uuid.NewV7().String()
		request := workerapi.InitialComputerVersionRequest{ComputerInstanceID: id, DesiredVersion: 7}
		request.Config.Env = []string{"VALUE=original"}
		var bodies [][]byte
		client := preparationClient(t, "http://127.0.0.1", &http.Client{Transport: preparationRoundTrip(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			status, encoded := 200, `{"computer_id":"`+id+`","version_id":"`+id+`"}`
			if len(bodies) == 1 {
				request.Config.Env[0] = "VALUE=changed"
				status = 503
				encoded = ""
			}
			return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(encoded)), Header: make(http.Header)}, nil
		})})
		if _, err := client.PublishInitialComputerVersion(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) || !bytes.Contains(bodies[1], []byte("VALUE=original")) {
			t.Fatalf("publication changed during retry: %q", bodies)
		}
	})
}

func TestPreparationDoesNotExtendRetryToOtherWorkerMutations(t *testing.T) {
	calls := 0
	client := preparationClient(t, "http://127.0.0.1", &http.Client{Transport: preparationRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 503, Status: "503", Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})})
	_, err := client.RegisterCheckpoint(t.Context(), workerapi.RegisterCheckpointRequest{})
	if calls != 1 || !httpclient.IsStatus(err, 503) {
		t.Fatalf("unrelated mutation retried: calls=%d err=%v", calls, err)
	}
}
