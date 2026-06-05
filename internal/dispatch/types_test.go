package dispatch

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
)

func TestQueueNamesForRuntimeOrdersSpecificToBase(t *testing.T) {
	runtime := compute.RuntimeSelector{
		ID:              "sha256:runtime",
		Arch:            "amd64",
		ABI:             "helmr.firecracker.snapshot.v0",
		KernelDigest:    "sha256:kernel",
		InitramfsDigest: "sha256:initramfs",
		RootfsDigest:    "sha256:rootfs",
		CNIProfile:      "helmr/v0",
	}

	got := QueueNamesForRuntime("queue-a", runtime)
	want := []string{
		"queue-a:rt:sha256:runtime:amd64:helmr.firecracker.snapshot.v0:sha256:kernel:sha256:initramfs:sha256:rootfs:helmr/v0",
		"queue-a:rt:sha256:runtime:amd64:helmr.firecracker.snapshot.v0:sha256:kernel:sha256:initramfs:sha256:rootfs",
		"queue-a:rt:sha256:runtime:amd64:helmr.firecracker.snapshot.v0:sha256:kernel:sha256:initramfs",
		"queue-a:rt:sha256:runtime:amd64:helmr.firecracker.snapshot.v0:sha256:kernel",
		"queue-a:rt:sha256:runtime:amd64:helmr.firecracker.snapshot.v0",
		"queue-a:rt:sha256:runtime:amd64",
		"queue-a:rt:sha256:runtime",
		"queue-a",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queue names = %#v, want %#v", got, want)
	}
}

func TestQueueNameForRuntimeUsesBaseForUnconstrainedRuntime(t *testing.T) {
	if got := QueueNameForRuntime("queue-a", compute.RuntimeSelector{}); got != "queue-a" {
		t.Fatalf("queue name = %q", got)
	}
}
