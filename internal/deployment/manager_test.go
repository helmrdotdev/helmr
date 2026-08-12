package deployment

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestManagerInvocationUsesVerifiedRuntimeClosure(t *testing.T) {
	tests := []struct {
		manager    PackageManager
		entrypoint ManagerEntrypoint
		want       []string
	}{
		{
			manager: PackageManager{Name: PackageManagerBun, Version: "1.3.10"},
			entrypoint: ManagerEntrypoint{
				Kind: ManagerEntrypointNative,
				Path: managerBunEntrypoint,
			},
			want: []string{managerBunEntrypoint, "--version"},
		},
		{
			manager: PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			entrypoint: ManagerEntrypoint{
				Kind: ManagerEntrypointNode,
				Path: managerNPMEntrypoint,
			},
			want: []string{
				"/opt/helmr/runtime/bin/node",
				managerNPMEntrypoint,
				"--version",
			},
		},
	}
	for _, test := range tests {
		t.Run(string(test.manager.Name), func(t *testing.T) {
			actual, err := ManagerInvocation(
				test.manager,
				test.entrypoint,
				"--version",
			)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(actual, test.want) {
				t.Fatalf("manager invocation = %q, want %q", actual, test.want)
			}
		})
	}
}

func TestManagerInvocationRejectsEntrypointMismatch(t *testing.T) {
	_, err := ManagerInvocation(
		PackageManager{Name: PackageManagerBun, Version: "1.3.10"},
		ManagerEntrypoint{Kind: ManagerEntrypointNative, Path: "/work/bun"},
	)
	if err == nil {
		t.Fatal("mismatched Manager entrypoint was accepted")
	}
}

func TestMaterializeBunManagerRetainsUpstreamBytesBehindRuntimeLauncher(t *testing.T) {
	source := t.TempDir()
	want := []byte("exact upstream Bun bytes")
	if err := os.WriteFile(filepath.Join(source, "bun"), want, 0755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := materializeManagerPayload(
		root,
		PackageManager{Name: PackageManagerBun, Version: "1.3.10"},
		source,
	); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "libexec", "bun"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("Manager payload changed the verified upstream Bun bytes")
	}
	target, err := os.Readlink(filepath.Join(root, "bin", "bun"))
	if err != nil {
		t.Fatal(err)
	}
	if target != managerNativeLauncherTarget {
		t.Fatalf("Bun launcher target = %q, want %q", target, managerNativeLauncherTarget)
	}
}
