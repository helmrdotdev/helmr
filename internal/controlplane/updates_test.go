package controlplane

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestUpdateStreamWakesRechecksAuthorityAndJoins(t *testing.T) {
	wake := make(chan struct{}, 1)
	var denied atomic.Bool
	joined := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(joined)
		streamUpdates(w, r, wake, func() error {
			if denied.Load() {
				return errors.New("revoked")
			}
			return nil
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(response.Header)
	}
	scanner := bufio.NewScanner(response.Body)
	read := func() {
		t.Helper()
		for scanner.Scan() {
			if scanner.Text() == "event: change" {
				return
			}
		}
		t.Fatal("missing update", scanner.Err())
	}
	read()
	wake <- struct{}{}
	read()
	denied.Store(true)
	wake <- struct{}{}
	for scanner.Scan() {
		if scanner.Text() == "event: change" {
			t.Fatal("revoked stream emitted update")
		}
	}
	select {
	case <-joined:
	case <-ctx.Done():
		t.Fatal("stream did not join")
	}
}

type streamRecorder struct{ *httptest.ResponseRecorder }

func (streamRecorder) SetWriteDeadline(time.Time) error { return nil }

func TestUpdateStreamHeartbeatRechecksAuthorityWithoutWorkHint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		writer := streamRecorder{httptest.NewRecorder()}
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/updates", nil)
		wake := make(chan struct{}, 1)
		checks := 0
		done := make(chan struct{})
		go func() { defer close(done); streamUpdates(writer, request, wake, func() error { checks++; return nil }) }()
		synctest.Wait()
		time.Sleep(16 * time.Second)
		synctest.Wait()
		if checks != 2 || strings.Count(writer.Body.String(), "event: change") != 1 || !strings.Contains(writer.Body.String(), ": heartbeat") {
			t.Fatalf("heartbeat checks=%d frames=%q", checks, writer.Body.String())
		}
		wake <- struct{}{}
		synctest.Wait()
		if strings.Count(writer.Body.String(), "event: change") != 2 {
			t.Fatal("real wake did not invalidate")
		}
		cancel()
		<-done
	})
}
