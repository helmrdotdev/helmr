package executor

import (
	"context"
	"io"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type fakeGuestSession struct {
	stream io.ReadWriteCloser
}

func (s fakeGuestSession) Stream() vm.Stream {
	return testVMStream(s.stream)
}

func (s fakeGuestSession) OpenStream(context.Context) (vm.Stream, error) {
	return testVMStream(s.stream), nil
}

func (s fakeGuestSession) Close(context.Context) error {
	return s.stream.Close()
}

func (s fakeGuestSession) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

type testStream struct {
	io.ReadWriteCloser
}

func testVMStream(stream io.ReadWriteCloser) vm.Stream {
	if stream == nil {
		return nil
	}
	return testStream{ReadWriteCloser: stream}
}

// unusedCAS satisfies a required CAS collaborator that the test never reads.
type unusedCAS struct{ cas.Store }
