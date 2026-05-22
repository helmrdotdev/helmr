package guestd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestResolveLaunchCwdDefaultsAndNormalizes(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"", defaultRuntimeWorkdir},
		{"app", "/app"},
		{"/workspace/./service", "/workspace/service"},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := resolveLaunchCwd(tt.raw, defaultRuntimeWorkdir)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("cwd = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveLaunchCwdRejectsUnsafeOrReservedPaths(t *testing.T) {
	for _, raw := range []string{"../escape", "/workspace/../etc", "/dev/null", "/proc/self", "/sys/kernel", "/opt/helmr/bin", "/.helmr-old-root/workspace"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := resolveLaunchCwd(raw, defaultRuntimeWorkdir); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPrepareLaunchPathDoesNotChownExistingTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires chown")
	}
	root := t.TempDir()
	existing := filepath.Join(root, "app")
	nested := filepath.Join(existing, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareLaunchPath(root, "/app", &resolvedRuntimeUser{UID: 1000, GID: 1000}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(nested)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid == 1000 || stat.Gid == 1000 {
		t.Fatalf("existing tree was chowned: uid=%d gid=%d", stat.Uid, stat.Gid)
	}
}
