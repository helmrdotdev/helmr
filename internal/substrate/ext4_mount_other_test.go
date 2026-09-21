//go:build !linux

package substrate

import (
	"os"
	"testing"
)

func proveNonrootImageWrite(t *testing.T, _ string) {
	t.Helper()
	if os.Getenv("HELMR_SUBSTRATE_MOUNT_PROOF") == "1" {
		t.Fatal("mount proof requires Linux")
	}
}
