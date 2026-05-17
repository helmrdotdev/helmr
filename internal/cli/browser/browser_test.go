package browser

import (
	"context"
	"testing"
)

func TestOpenRespectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Open(ctx, "https://helmr.example.test"); err == nil {
		t.Fatal("expected canceled context error")
	}
}
