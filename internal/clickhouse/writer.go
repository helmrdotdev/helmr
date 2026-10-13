package clickhouse

import (
	"context"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

type Writer struct {
	client batchClient
}

type batchClient interface {
	PrepareBatch(ctx context.Context, query string) (driver.Batch, error)
}

func NewWriter(client batchClient) *Writer {
	return &Writer{client: client}
}
