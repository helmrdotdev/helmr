//go:build linux

package main

import (
	"context"
	"testing"
)

func TestPublishPlatformReleaseRejectsDescriptorValidInvalidRuntimeBeforeStoreWrite(t *testing.T) {
	directory, _ := platformReleaseFixture(t)
	store := &releasePublishStore{}
	if err := publishPlatformRelease(context.Background(), store, directory); err == nil {
		t.Fatal("descriptor-valid invalid Runtime was published")
	}
	if len(store.published) != 0 {
		t.Fatalf("published %d objects before deep verification completed", len(store.published))
	}
}
