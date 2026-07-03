package clickhouse

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"
	"time"

	ch "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type Client struct {
	conn           driver.Conn
	requestTimeout time.Duration
}

type Config struct {
	URL      string
	User     string
	Password string
}

func New(cfg Config) (*Client, error) {
	options, err := optionsFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := ch.Open(options)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	return &Client{conn: conn, requestTimeout: defaultRequestTimeout}, nil
}

func Named(name string, value any) driver.NamedValue {
	return ch.Named(name, value)
}

func (c *Client) Exec(ctx context.Context, query string, args ...any) error {
	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()
	return c.conn.Exec(ctx, query, args...)
}

func (c *Client) Select(ctx context.Context, dest any, query string, args ...any) error {
	ctx, cancel := c.contextWithTimeout(ctx)
	defer cancel()
	return c.conn.Select(ctx, dest, query, args...)
}

func (c *Client) PrepareBatch(ctx context.Context, query string) (driver.Batch, error) {
	ctx, cancel := c.contextWithTimeout(ctx)
	batch, err := c.conn.PrepareBatch(ctx, query)
	if err != nil {
		cancel()
		return nil, err
	}
	return &timeoutBatch{Batch: batch, cancel: cancel}, nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func (c *Client) contextWithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok || c.requestTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.requestTimeout)
}

type timeoutBatch struct {
	driver.Batch
	cancel context.CancelFunc
}

func (b *timeoutBatch) Abort() error {
	defer b.cancel()
	return b.Batch.Abort()
}

func (b *timeoutBatch) Close() error {
	defer b.cancel()
	return b.Batch.Close()
}

func (b *timeoutBatch) Send() error {
	defer b.cancel()
	return b.Batch.Send()
}

const defaultRequestTimeout = 30 * time.Second

func optionsFromConfig(cfg Config) (*ch.Options, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return nil, fmt.Errorf("clickhouse url is required")
	}
	base, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse clickhouse url: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("clickhouse url must include scheme and host")
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("clickhouse url must use http or https")
	}
	user := strings.TrimSpace(cfg.User)
	if user == "" {
		user = "default"
	}
	options := &ch.Options{
		Protocol:        ch.HTTP,
		Addr:            []string{base.Host},
		Auth:            ch.Auth{Username: user, Password: cfg.Password},
		HttpUrlPath:     base.EscapedPath(),
		HttpHeaders:     map[string]string{},
		DialTimeout:     defaultRequestTimeout,
		ReadTimeout:     defaultRequestTimeout,
		MaxOpenConns:    5,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Hour,
	}
	if base.Scheme == "https" {
		options.TLS = &tls.Config{MinVersion: tls.VersionTLS12}
		options.HttpHeaders["X-ClickHouse-SSL-Certificate-Auth"] = "off"
	}
	return options, nil
}
