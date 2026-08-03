package main

import (
	"context"
	"testing"
)

func TestOpenBrowserRespectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := openBrowser(ctx, "https://helmr.example.test"); err == nil {
		t.Fatal("expected canceled context error")
	}
}
