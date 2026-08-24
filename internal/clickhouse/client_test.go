package clickhouse

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

func TestIsReadinessErrorRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{name: "nil", err: nil},
		{name: "deadline", err: context.DeadlineExceeded, retryable: true},
		{name: "eof", err: io.EOF, retryable: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, retryable: true},
		{name: "connection refused", err: syscall.ECONNREFUSED, retryable: true},
		{name: "wrapped connection reset", err: fmt.Errorf("dial clickhouse: %w", syscall.ECONNRESET), retryable: true},
		{name: "driver acquire timeout", err: ch.ErrAcquireConnTimeout, retryable: true},
		{name: "driver connection closed", err: ch.ErrConnectionClosed, retryable: true},
		{name: "temporary network error", err: readinessNetError{temporary: true}, retryable: true},
		{name: "timeout network error", err: readinessNetError{timeout: true}, retryable: true},
		{name: "non-network temporary capability", err: readinessTemporaryError{}},
		{name: "caller cancellation", err: context.Canceled},
		{name: "server exception", err: &ch.Exception{Code: 516, Message: "authentication failed"}},
		{name: "unknown certificate authority", err: x509.UnknownAuthorityError{}},
		{name: "hostname mismatch", err: x509.HostnameError{}},
		{name: "invalid certificate", err: x509.CertificateInvalidError{}},
		{name: "generic error", err: errors.New("invalid clickhouse configuration")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := IsReadinessErrorRetryable(test.err); got != test.retryable {
				t.Fatalf("IsReadinessErrorRetryable(%v) = %t, want %t", test.err, got, test.retryable)
			}
		})
	}
}

type readinessNetError struct {
	temporary bool
	timeout   bool
}

func (e readinessNetError) Error() string   { return "network error" }
func (e readinessNetError) Temporary() bool { return e.temporary }
func (e readinessNetError) Timeout() bool   { return e.timeout }

var _ net.Error = readinessNetError{}

type readinessTemporaryError struct{}

func (readinessTemporaryError) Error() string   { return "generic temporary error" }
func (readinessTemporaryError) Temporary() bool { return true }

func TestClientPingAppliesRequestTimeout(t *testing.T) {
	client := &Client{
		conn: pingConn{ping: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("Ping context deadline missing")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > 20*time.Millisecond {
				t.Fatalf("Ping deadline remaining = %s, want within request timeout", remaining)
			}
			<-ctx.Done()
			return ctx.Err()
		}},
		requestTimeout: 10 * time.Millisecond,
	}

	if err := client.Ping(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ping error = %v, want request deadline", err)
	}
}

type pingConn struct {
	driver.Conn
	ping func(context.Context) error
}

func (c pingConn) Ping(ctx context.Context) error { return c.ping(ctx) }

func TestOptionsFromConfigUsesHTTPTransportForCloudURL(t *testing.T) {
	options, err := optionsFromConfig(Config{
		URL:      " https://clickhouse.example.test:8443/custom ",
		User:     "telemetry",
		Password: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(options.Addr) != 1 || options.Addr[0] != "clickhouse.example.test:8443" {
		t.Fatalf("addr = %#v, want clickhouse.example.test:8443", options.Addr)
	}
	if options.TLS == nil {
		t.Fatalf("TLS is nil, want TLS for https URL")
	}
	if got := options.HttpHeaders["X-ClickHouse-SSL-Certificate-Auth"]; got != "off" {
		t.Fatalf("certificate auth header = %q, want off", got)
	}
	if options.HttpUrlPath != "/custom" {
		t.Fatalf("HttpUrlPath = %q, want /custom", options.HttpUrlPath)
	}
	if options.Auth.Username != "telemetry" || options.Auth.Password != "secret" {
		t.Fatalf("auth = %#v, want telemetry/secret", options.Auth)
	}
}

func TestOptionsFromConfigDefaultsUser(t *testing.T) {
	options, err := optionsFromConfig(Config{URL: "http://localhost:8123"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Auth.Username != "default" {
		t.Fatalf("username = %q, want default", options.Auth.Username)
	}
	if options.TLS != nil {
		t.Fatalf("TLS = %#v, want nil for http URL", options.TLS)
	}
	if _, ok := options.HttpHeaders["X-ClickHouse-SSL-Certificate-Auth"]; ok {
		t.Fatalf("certificate auth header set for http URL")
	}
}

func TestClientContextWithTimeoutAddsDefaultDeadline(t *testing.T) {
	client := &Client{requestTimeout: 30 * time.Second}
	ctx, cancel := client.contextWithTimeout(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("deadline missing")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("deadline remaining = %s, want within 30s", remaining)
	}
}

func TestClientContextWithTimeoutPreservesCallerDeadline(t *testing.T) {
	callerCtx, callerCancel := context.WithTimeout(context.Background(), time.Minute)
	defer callerCancel()
	client := &Client{requestTimeout: 30 * time.Second}
	ctx, cancel := client.contextWithTimeout(callerCtx)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("deadline missing")
	}
	callerDeadline, _ := callerCtx.Deadline()
	if !deadline.Equal(callerDeadline) {
		t.Fatalf("deadline = %s, want caller deadline %s", deadline, callerDeadline)
	}
}
