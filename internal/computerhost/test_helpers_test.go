package computerhost

import (
	"github.com/helmrdotdev/helmr/internal/vm"
	"io"
)

type testStream struct{ io.ReadWriteCloser }

func testVMStream(stream io.ReadWriteCloser) vm.Stream {
	if stream == nil {
		return nil
	}
	return testStream{ReadWriteCloser: stream}
}
