package computer

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"io/fs"
	"testing"
)

func TestSourceFailureRequiresPublishedProvenance(t *testing.T) {
	for _, cause := range []error{fs.ErrNotExist, blockformat.ErrIntegrity} {
		local := deviceFailure(fmt.Errorf("read: %w", cause))
		var device *DeviceFailure
		var source *SourceFailure
		if !errors.As(local, &device) || errors.As(local, &source) || !errors.Is(local, cause) {
			t.Fatalf("local classification: %v", local)
		}
		published := PublishedSourceFailure(fmt.Errorf("root: %w", cause))
		if !errors.As(published, &source) || !errors.Is(published, cause) {
			t.Fatalf("published classification: %v", published)
		}
	}
	var disk *DeviceFailure
	if !errors.As(deviceFailure(blockformat.ErrAuthentication), &disk) || PublishedSourceFailure(blockformat.ErrAuthentication) != blockformat.ErrAuthentication {
		t.Fatal("authentication ambiguity discarded")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, errors.New("temporary network error"), errors.New("unknown key version"), nil} {
		if PublishedSourceFailure(cause) != cause || deviceFailure(cause) != cause {
			t.Fatalf("ambiguous failure reclassified: %v", cause)
		}
	}
}
