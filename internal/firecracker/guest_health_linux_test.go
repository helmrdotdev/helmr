//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
)

func TestReadHealthSendsHTTPRequest(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	errc := make(chan error, 1)
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			errc <- err
			return
		}
		if req.Method != http.MethodGet || req.URL.Path != "/" {
			t.Errorf("request = %s %s", req.Method, req.URL.Path)
		}
		_, err = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 36\r\nConnection: close\r\n\r\n{\"status\":\"ok\",\"component\":\"guestd\"}")
		errc <- err
	}()

	response, err := readHealth(client)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.Component != "guestd" {
		t.Fatalf("response = %+v", response)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestWaitForHealthRetriesTransientReadFailure(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	attempts := 0
	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		attempts++
		client, server := net.Pipe()
		if attempts == 1 {
			_ = server.Close()
			return client, nil
		}
		go func() {
			defer server.Close()
			if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
				return
			}
			_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 36\r\nConnection: close\r\n\r\n{\"status\":\"ok\",\"component\":\"guestd\"}")
		}()
		return client, nil
	}

	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	if err := connector.waitForHealth(context.Background(), "vsock.sock", nil, nil); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("dial attempts = %d, want 2", attempts)
	}
}

func TestWaitForHealthRetriesStalledReadWithAttemptDeadline(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	attempts := 0
	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		attempts++
		client, server := net.Pipe()
		if attempts == 1 {
			go func() {
				defer server.Close()
				_, _ = http.ReadRequest(bufio.NewReader(server))
				time.Sleep(100 * time.Millisecond)
			}()
			return client, nil
		}
		go func() {
			defer server.Close()
			if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
				return
			}
			_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 36\r\nConnection: close\r\n\r\n{\"status\":\"ok\",\"component\":\"guestd\"}")
		}()
		return client, nil
	}

	var logs []string
	logf := func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	connector := &Connector{cfg: (Config{HealthTimeout: time.Second, HealthAttemptTimeout: 20 * time.Millisecond}).WithDefaults()}
	if err := connector.waitForHealth(context.Background(), "vsock.sock", nil, logf); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("dial attempts = %d, want 2", attempts)
	}
	if !strings.Contains(strings.Join(logs, "\n"), `bucket="read"`) {
		t.Fatalf("logs = %v, want read bucket attempt log", logs)
	}
}

func TestWaitForHealthClassifiesUnbufferedStalledWriteWithAttemptDeadline(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			time.Sleep(200 * time.Millisecond)
		}()
		return client, nil
	}

	connector := &Connector{cfg: (Config{HealthTimeout: 80 * time.Millisecond, HealthAttemptTimeout: 20 * time.Millisecond}).WithDefaults()}
	err := connector.waitForHealth(context.Background(), "vsock.sock", nil, nil)
	if err == nil {
		t.Fatal("waitForHealth error = nil, want timeout")
	}
	text := err.Error()
	if !strings.Contains(text, "write_errors=") || !strings.Contains(text, `last_bucket="write"`) {
		t.Fatalf("waitForHealth error = %v, want write bucket summary", err)
	}
}

func TestWaitForHealthAppliesAttemptDeadlineToDial(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	attempts := 0
	sawAttemptDeadline := false
	dialVsock = func(ctx context.Context, _ string, _ uint32, _ ...vsock.DialOption) (net.Conn, error) {
		attempts++
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("dial context has no deadline")
		}
		if remaining := time.Until(deadline); remaining > 80*time.Millisecond {
			t.Fatalf("dial context deadline remaining = %s, want attempt-scoped deadline", remaining)
		}
		sawAttemptDeadline = true
		<-ctx.Done()
		return nil, fmt.Errorf("dial blocked: %w", ctx.Err())
	}

	connector := &Connector{cfg: (Config{HealthTimeout: 120 * time.Millisecond, HealthAttemptTimeout: 20 * time.Millisecond}).WithDefaults()}
	err := connector.waitForHealth(context.Background(), "vsock.sock", nil, nil)
	if err == nil {
		t.Fatal("waitForHealth error = nil, want timeout")
	}
	if attempts < 1 {
		t.Fatalf("dial attempts = %d, want at least 1", attempts)
	}
	if !sawAttemptDeadline {
		t.Fatal("dial context was not checked")
	}
	text := err.Error()
	if !strings.Contains(text, "dial_errors=") || !strings.Contains(text, `last_bucket="dial"`) {
		t.Fatalf("waitForHealth error = %v, want dial bucket summary", err)
	}
}

func TestWaitForHealthLogsTerminalStatusWithoutStaleError(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
				return
			}
			body := `{"status":"degraded","component":"guestd"}`
			_, _ = fmt.Fprintf(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		}()
		return client, nil
	}

	var logs []string
	logf := func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	err := connector.waitForHealth(context.Background(), "vsock.sock", nil, logf)
	if err == nil {
		t.Fatal("waitForHealth error = nil, want terminal status error")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, `bucket="status"`) || !strings.Contains(joined, `error="guest health status=\"degraded\""`) {
		t.Fatalf("logs = %v, want status bucket with current status error", logs)
	}
}

func TestWaitForHealthReportsMachineExit(t *testing.T) {
	exit := &machineExit{done: make(chan struct{})}
	exit.err = errors.New("vm exited")
	close(exit.done)

	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	err := connector.waitForHealth(context.Background(), "vsock.sock", exit, nil)
	if err == nil {
		t.Fatal("waitForHealth error = nil, want machine exit error")
	}
	if !strings.Contains(err.Error(), "the Firecracker machine exited during guest health wait") {
		t.Fatalf("waitForHealth error = %v, want Firecracker machine exit context", err)
	}
	if !strings.Contains(err.Error(), "machine_exited=true") {
		t.Fatalf("waitForHealth error = %v, want machine_exited summary", err)
	}
}

func TestConnectGuestPortReturnsMachineExitWithoutHealthTimeout(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	dialEntered := make(chan struct{})
	dialVsock = func(ctx context.Context, _ string, _ uint32, _ ...vsock.DialOption) (net.Conn, error) {
		close(dialEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	exit := &machineExit{done: make(chan struct{})}
	connector := &Connector{cfg: (Config{HealthTimeout: time.Minute}).WithDefaults()}

	result := make(chan error, 1)
	go func() {
		_, err := connector.connectGuestPort(context.Background(), "vsock.sock", exit)
		result <- err
	}()
	select {
	case <-dialEntered:
	case <-time.After(time.Second):
		t.Fatal("connectGuestPort did not enter dial")
	}
	exit.err = errors.New("vm exited")
	close(exit.done)

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "the Firecracker machine exited before guest port") {
			t.Fatalf("connectGuestPort error = %v, want Firecracker machine exit", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connectGuestPort waited after machine exit")
	}
}

func TestConnectGuestPortUsesFullDuplexStream(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	client, server := net.Pipe()
	defer server.Close()
	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		return client, nil
	}

	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	stream, err := connector.connectGuestPort(context.Background(), "vsock.sock", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	errChannel := make(chan error, 1)
	go func() {
		request := make([]byte, len("request"))
		if _, err := io.ReadFull(server, request); err != nil {
			errChannel <- err
			return
		}
		if string(request) != "request" {
			errChannel <- fmt.Errorf("request = %q, want request", request)
			return
		}
		_, err := io.WriteString(server, "response")
		errChannel <- err
	}()
	if _, err := io.WriteString(stream, "request"); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("response"))
	if _, err := io.ReadFull(stream, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "response" {
		t.Fatalf("response = %q, want response", response)
	}
	if err := <-errChannel; err != nil {
		t.Fatal(err)
	}
}

func TestReadHealthRejectsOversizedBody(t *testing.T) {
	client, server := net.Pipe()
	errc := make(chan error, 1)
	go func() {
		defer server.Close()
		if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
			errc <- err
			return
		}
		body := strings.Repeat("x", maxGuestHealthResponseBytes+1)
		_, err := fmt.Fprintf(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		errc <- err
	}()

	_, err := readHealth(client)
	if err == nil {
		t.Fatal("readHealth error = nil, want oversized body error")
	}
	if !strings.Contains(err.Error(), "body exceeds") {
		t.Fatalf("readHealth error = %v, want body size context", err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestReadHealthAcceptsChunkedBody(t *testing.T) {
	client, server := net.Pipe()
	errc := make(chan error, 1)
	go func() {
		defer server.Close()
		if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
			errc <- err
			return
		}
		_, err := io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n24\r\n{\"status\":\"ok\",\"component\":\"guestd\"}\r\n0\r\n\r\n")
		errc <- err
	}()

	response, err := readHealth(client)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.Component != "guestd" {
		t.Fatalf("response = %+v", response)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestPreparedGuestSeparatesHealthFromGuestPort(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	var ports []uint32
	dialVsock = func(_ context.Context, _ string, port uint32, _ ...vsock.DialOption) (net.Conn, error) {
		ports = append(ports, port)
		client, server := net.Pipe()
		if port == uint32((Config{}).WithDefaults().HealthPort) {
			if len(ports) == 1 {
				_ = server.Close()
				return client, nil
			}
			go func() {
				defer server.Close()
				if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
					return
				}
				_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 36\r\nConnection: close\r\n\r\n{\"status\":\"ok\",\"component\":\"guestd\"}")
			}()
			return client, nil
		}
		go func() {
			<-time.After(10 * time.Millisecond)
			_ = server.Close()
		}()
		return client, nil
	}

	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	if err := connector.waitForHealth(
		context.Background(),
		"vsock.sock",
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 {
		t.Fatalf("ports after health = %v, want health only", ports)
	}
	conn, err := connector.connectGuestPort(
		context.Background(),
		"vsock.sock",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	want := []uint32{uint32(connector.cfg.HealthPort), uint32(connector.cfg.HealthPort), uint32(connector.cfg.GuestPort)}
	if len(ports) != len(want) {
		t.Fatalf("ports = %v, want %v", ports, want)
	}
	for i := range want {
		if ports[i] != want[i] {
			t.Fatalf("ports = %v, want %v", ports, want)
		}
	}
}
