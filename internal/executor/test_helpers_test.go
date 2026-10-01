package executor

import (
	"context"
	"io"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type fakeGuestMachine struct {
	stream io.ReadWriteCloser
}

func (s fakeGuestMachine) Stream() vm.Stream {
	return testVMStream(s.stream)
}

func (s fakeGuestMachine) OpenStream(context.Context) (vm.Stream, error) {
	return testVMStream(s.stream), nil
}

func (s fakeGuestMachine) Close(context.Context) error {
	return s.stream.Close()
}

func (s fakeGuestMachine) Wait(ctx context.Context) error {
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
