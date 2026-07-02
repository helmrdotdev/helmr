package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"
)

type Client struct {
	baseURL    *url.URL
	user       string
	password   string
	httpClient *http.Client
}

type Config struct {
	URL      string
	User     string
	Password string
}

func New(cfg Config) (*Client, error) {
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
	user := strings.TrimSpace(cfg.User)
	if user == "" {
		user = "default"
	}
	return &Client{
		baseURL:  base,
		user:     user,
		password: cfg.Password,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

func (c *Client) Exec(ctx context.Context, query string) error {
	_, err := c.do(ctx, query, nil, nil)
	return err
}

func (c *Client) InsertJSONEachRow(ctx context.Context, query string, rows any) error {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	value := reflect.ValueOf(rows)
	if value.Kind() != reflect.Slice {
		return fmt.Errorf("clickhouse insert rows must be a slice")
	}
	for idx := 0; idx < value.Len(); idx++ {
		if err := encoder.Encode(value.Index(idx).Interface()); err != nil {
			return fmt.Errorf("encode clickhouse row: %w", err)
		}
	}
	_, err := c.do(ctx, query, &body, nil)
	return err
}

func (c *Client) SelectJSONEachRow(ctx context.Context, query string, dest any) error {
	return c.SelectJSONEachRowParams(ctx, query, nil, dest)
}

func (c *Client) SelectJSONEachRowParams(ctx context.Context, query string, params map[string]string, dest any) error {
	body, err := c.do(ctx, query, nil, params)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	target, err := newSliceTarget(dest)
	if err != nil {
		return err
	}
	for {
		elem := target.newElement()
		if err := decoder.Decode(elem); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("decode clickhouse row: %w", err)
		}
		target.append(elem)
	}
}

type sliceTarget struct {
	value reflect.Value
}

func newSliceTarget(dest any) (sliceTarget, error) {
	value := reflect.ValueOf(dest)
	if value.Kind() != reflect.Pointer || value.IsNil() {
		return sliceTarget{}, fmt.Errorf("clickhouse destination must be a non-nil slice pointer")
	}
	slice := value.Elem()
	if slice.Kind() != reflect.Slice {
		return sliceTarget{}, fmt.Errorf("clickhouse destination must be a slice pointer")
	}
	slice.Set(slice.Slice(0, 0))
	return sliceTarget{value: slice}, nil
}

func (t sliceTarget) newElement() any {
	return reflect.New(t.value.Type().Elem()).Interface()
}

func (t sliceTarget) append(elem any) {
	t.value.Set(reflect.Append(t.value, reflect.ValueOf(elem).Elem()))
}

func (c *Client) do(ctx context.Context, query string, body io.Reader, params map[string]string) ([]byte, error) {
	endpoint := *c.baseURL
	values := endpoint.Query()
	values.Set("query", query)
	values.Set("wait_end_of_query", "1")
	values.Set("date_time_output_format", "iso")
	for name, value := range params {
		values.Set("param_"+name, value)
	}
	endpoint.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.user, c.password)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(responseBody))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("clickhouse %s: %s", resp.Status, message)
	}
	if readErr != nil {
		return nil, readErr
	}
	return responseBody, nil
}
