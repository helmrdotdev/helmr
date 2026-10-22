package computerhost

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// OptionalTransport shares one paced byte budget across uploads, downloads,
// retries and range reads. Limiting request bodies at the transport boundary
// also covers multipart uploads whose SDK first stages a seekable local file.
func (p *PreparedMachines) OptionalTransport(base http.RoundTripper) http.RoundTripper {
	return optionalTransport{machines: p, base: base}
}

type optionalTransport struct {
	machines *PreparedMachines
	base     http.RoundTripper
}

func (t optionalTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	if request.Body != nil {
		request.Body = &optionalTransferBody{ReadCloser: request.Body, ctx: request.Context(), machines: t.machines}
	}
	response, err := t.base.RoundTrip(request)
	if err == nil && response.Body != nil {
		response.Body = &optionalTransferBody{ReadCloser: response.Body, ctx: request.Context(), machines: t.machines}
	}
	return response, err
}

type optionalTransferBody struct {
	io.ReadCloser
	ctx      context.Context
	machines *PreparedMachines
}

func (b *optionalTransferBody) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	n, err := b.machines.waitOptionalTransfer(b.ctx, len(buffer))
	if err != nil {
		return 0, err
	}
	return b.ReadCloser.Read(buffer[:n])
}

func (p *PreparedMachines) waitOptionalTransfer(ctx context.Context, size int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, context.Cause(ctx)
	}
	p.optional.mu.Lock()
	rate := p.optional.policy.Optional.IOBytesPerSecond
	revision := p.optional.policy.Revision
	if p.optional.closed || rate <= 0 || p.optional.policy.Revision <= 0 || time.Since(p.optional.confirmed) >= 2*workerapi.WorkerObservationInterval {
		p.fenceOptionalTransfersLocked(revision)
		p.optional.mu.Unlock()
		return 0, errOptionalReclaimed
	}
	// At most a tenth of a second per body read, with a 64 KiB ceiling.
	n := min(size, 64<<10, int(max(int64(1), min(int64(64<<10), rate/10))))
	duration := time.Duration((int64(n)*int64(time.Second) + rate - 1) / rate)
	next := maxTime(time.Now(), p.optional.transferNext).Add(duration)
	p.optional.transferNext = next
	p.optional.mu.Unlock()
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return 0, context.Cause(ctx)
	case <-timer.C:
		p.optional.mu.Lock()
		valid := !p.optional.closed && p.optional.policy.Revision == revision && p.optional.policy.Optional.IOBytesPerSecond == rate && time.Since(p.optional.confirmed) < 2*workerapi.WorkerObservationInterval
		if !valid {
			p.fenceOptionalTransfersLocked(revision)
		}
		p.optional.mu.Unlock()
		if !valid {
			return 0, errOptionalReclaimed
		}
		return n, nil
	}
}
