package clickhouse

import "context"

type Reader struct {
	client historicalClient
}

type historicalClient interface {
	Select(ctx context.Context, dest any, query string, args ...any) error
}

func NewReader(client historicalClient) *Reader {
	return &Reader{client: client}
}
