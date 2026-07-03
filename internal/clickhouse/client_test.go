package clickhouse

import (
	"context"
	"testing"
	"time"
)

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
