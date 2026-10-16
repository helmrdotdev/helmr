package disk

import (
	"context"
	"io"
	"sync"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

// A range body is exempt only during each blocking call. Keeping a reader open
// while a parser/executor is stalled must not suppress request hang detection.
type guestIORangeSource struct{ source blockformat.RangeSource }

func (s guestIORangeSource) GetRange(ctx context.Context, digest string, size, offset, length int64) (io.ReadCloser, error) {
	wait := cas.BeginGuestIOWait(ctx)
	body, err := s.source.GetRange(ctx, digest, size, offset, length)
	wait.End()
	if body == nil || wait == nil {
		return body, err
	}
	return guestIORangeBody{ctx, body}, err
}

type guestIORangeBody struct {
	ctx context.Context
	io.ReadCloser
}

func (b guestIORangeBody) Read(p []byte) (int, error) {
	wait := cas.BeginGuestIOWait(b.ctx)
	n, err := b.ReadCloser.Read(p)
	wait.End()
	return n, err
}
func (b guestIORangeBody) Close() error {
	wait := cas.BeginGuestIOWait(b.ctx)
	err := b.ReadCloser.Close()
	wait.End()
	return err
}

func lockGuestIO(ctx context.Context, mu *sync.Mutex) {
	wait := cas.BeginGuestIOWait(ctx)
	mu.Lock()
	wait.End()
}

func readLockGuestIO(ctx context.Context, mu *sync.RWMutex) {
	wait := cas.BeginGuestIOWait(ctx)
	mu.RLock()
	wait.End()
}
