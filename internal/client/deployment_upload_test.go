package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestDeploymentObjectUploadReportsSlowProgressWithoutTimingOut(t *testing.T) {
	objectPath := writeDeploymentUploadTestObject(t, "abcdef")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		buffer := make([]byte, 1)
		for {
			_, err := request.Body.Read(buffer)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			time.Sleep(20 * time.Millisecond)
		}
		return deploymentUploadTestResponse(request), nil
	})
	client := newDeploymentUploadTestClient(t, transport)
	var mu sync.Mutex
	var observed []int64
	err := client.uploadDeploymentBundleObject(
		t.Context(),
		deploymentUploadTestPlan("https://upload.example/object", 6),
		objectPath,
		func(bytesRead int64, totalBytes int64) error {
			if totalBytes != 6 {
				return fmt.Errorf("total bytes = %d", totalBytes)
			}
			mu.Lock()
			observed = append(observed, bytesRead)
			mu.Unlock()
			return nil
		},
		5*time.Millisecond,
		80*time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) == 0 {
		t.Fatal("upload emitted no progress observations")
	}
	for _, bytesRead := range observed {
		if bytesRead > 0 && bytesRead < 6 {
			return
		}
	}
	t.Fatalf("progress observations = %v", observed)
}

func TestDeploymentObjectUploadCancelsWhenRequestBodyMakesNoProgress(t *testing.T) {
	const secret = "presigned-secret-sentinel"
	objectPath := writeDeploymentUploadTestObject(t, "abcdef")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client := newDeploymentUploadTestClient(t, transport)
	started := time.Now()
	err := client.uploadDeploymentBundleObject(
		t.Context(),
		deploymentUploadTestPlan("https://upload.example/object?X-Amz-Signature="+secret, 6),
		objectPath,
		nil,
		5*time.Millisecond,
		25*time.Millisecond,
	)
	if !errors.Is(err, ErrDeploymentObjectUploadNoProgress) {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error exposed presigned credential: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("no-progress cancellation took %s", elapsed)
	}
}

func TestDeploymentObjectUploadBoundsResponseWaitAfterBodyWasRead(t *testing.T) {
	objectPath := writeDeploymentUploadTestObject(t, "abcdef")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if _, err := io.Copy(io.Discard, request.Body); err != nil {
			return nil, err
		}
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client := newDeploymentUploadTestClient(t, transport)
	err := client.uploadDeploymentBundleObject(
		t.Context(),
		deploymentUploadTestPlan("https://upload.example/object", 6),
		objectPath,
		nil,
		5*time.Millisecond,
		25*time.Millisecond,
	)
	if !errors.Is(err, ErrDeploymentObjectUploadNoProgress) {
		t.Fatalf("error = %v", err)
	}
}

func TestDeploymentObjectUploadPreservesParentCancellation(t *testing.T) {
	objectPath := writeDeploymentUploadTestObject(t, "abcdef")
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	client := newDeploymentUploadTestClient(t, transport)
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(20*time.Millisecond, cancel)
	err := client.uploadDeploymentBundleObject(
		ctx,
		deploymentUploadTestPlan("https://upload.example/object", 6),
		objectPath,
		nil,
		5*time.Millisecond,
		time.Second,
	)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrDeploymentObjectUploadNoProgress) {
		t.Fatalf("error = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func newDeploymentUploadTestClient(t *testing.T, transport http.RoundTripper) *Client {
	t.Helper()
	client, err := New(
		"https://control.example",
		WithHTTPClient(&http.Client{Transport: transport}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeDeploymentUploadTestObject(t *testing.T, body string) string {
	t.Helper()
	path := t.TempDir() + "/object"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func deploymentUploadTestPlan(url string, size int64) api.DeploymentBundleUpload {
	return api.DeploymentBundleUpload{
		Method: http.MethodPut,
		URL:    url,
		Headers: map[string]string{
			"Content-Length": strconv.FormatInt(size, 10),
		},
	}
}

func deploymentUploadTestResponse(request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Status:     "204 No Content",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    request,
	}
}
