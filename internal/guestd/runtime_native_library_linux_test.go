package guestd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type nativeAddonResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Value any    `json:"value"`
}

type nativeLibraryProbe struct {
	ExecPath      string            `json:"execPath"`
	UID           uint32            `json:"uid"`
	Distro        nativeAddonResult `json:"distro"`
	Vendored      nativeAddonResult `json:"vendored"`
	Future        nativeAddonResult `json:"future"`
	Child         string            `json:"child"`
	DNS           string            `json:"dns"`
	ForeignFamily []string          `json:"foreignFamily"`
	OpenCode      struct {
		Status *int   `json:"status"`
		Stdout string `json:"stdout"`
		Error  string `json:"error"`
	} `json:"opencode"`
}

// TestManagedNodeNativeLibraries launches the platform Node through the real
// managed Program path (namespace init, pivot into the Workspace root, sealed
// Runtime and Program mounts, identity drop, direct exec) inside prepared
// Workspace roots. tests/guestd_native_library_e2e.sh provides the admitted
// Program, the Runtime artifact and the roots; it needs root on Linux.
func TestManagedNodeNativeLibraries(t *testing.T) {
	roots := os.Getenv("HELMR_GUESTD_NATIVE_WORKSPACES")
	if roots == "" {
		t.Skip("HELMR_GUESTD_NATIVE_WORKSPACES is not set")
	}
	tests := []struct {
		workspace string
		// library is whether the Workspace image installs the distribution
		// library the source-built addon links; glibc whether it can run the
		// dependency's glibc-linked executable at all.
		library, glibc bool
	}{
		{workspace: "bookworm-lib", library: true, glibc: true},
		{workspace: "bookworm-lib-nocache", library: true, glibc: true},
		{workspace: "ubuntu2004-lib", library: true, glibc: true},
		{workspace: "ubuntu2510-lib", library: true, glibc: true},
		{workspace: "bookworm-nolib", library: false, glibc: true},
		{workspace: "alpine", library: false, glibc: false},
	}
	for _, test := range tests {
		t.Run(test.workspace, func(t *testing.T) {
			imageRoot := filepath.Join(roots, test.workspace)
			if _, err := os.Stat(imageRoot); err != nil {
				t.Fatal(err)
			}
			user := &resolvedRuntimeUser{Name: "helmr", UID: 65532, GID: 65532, Home: "/tmp"}
			// A Workspace image may carry loader variables; pointing them at its
			// own libc is what would crash a Runtime that honoured them.
			env := managedRuntimeEnv(ociRuntimeConfig{Env: []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"LD_LIBRARY_PATH=/lib/x86_64-linux-gnu:/usr/lib/x86_64-linux-gnu",
				"LD_PRELOAD=/lib/x86_64-linux-gnu/libc.so.6",
			}}, user, "/")
			cleanup, err := mountImageRuntimeFilesystems(imageRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			cmd, err := imageCommand(t.Context(), managedProgramNode,
				[]string{"/opt/helmr/program/probe/check.cjs"}, "/", env, imageRoot, user,
				imageCommandOptions{ManagedProgram: true})
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("managed Node: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
			}
			var probe nativeLibraryProbe
			if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
				t.Fatalf("probe output %q: %v\nstderr: %s", stdout.String(), err, stderr.String())
			}
			t.Logf("%s", stdout.String())

			if probe.ExecPath != managedProgramNode || probe.UID != 65532 {
				t.Fatalf("launched %q as uid %d", probe.ExecPath, probe.UID)
			}
			if probe.Child != "[]" {
				t.Fatalf("spawned process inherited loader variables %s", probe.Child)
			}
			if len(probe.ForeignFamily) != 0 {
				t.Fatalf("C runtime components came from the Workspace image: %q", probe.ForeignFamily)
			}
			if probe.DNS != "ok" {
				t.Fatalf("name resolution = %q", probe.DNS)
			}
			if !probe.Vendored.OK {
				t.Fatalf("self-contained addon failed: %s", probe.Vendored.Error)
			}
			if test.library {
				if !probe.Distro.OK {
					t.Fatalf("addon linked to a Workspace library failed: %s", probe.Distro.Error)
				}
				values, _ := probe.Distro.Value.(map[string]any)
				if values["resolver"] != float64(0) || values["shm"] != true || values["aio"] != float64(0) ||
					!strings.HasPrefix(values["sqlite"].(string), "3.") {
					t.Fatalf("addon calls = %v", values)
				}
			} else if probe.Distro.OK || !strings.Contains(probe.Distro.Error, "libsqlite3.so.0: cannot open shared object file") {
				t.Fatalf("missing Workspace library must be the ordinary loader error, got %+v", probe.Distro)
			}
			if probe.Future.OK || !strings.Contains(probe.Future.Error, "GLIBC_9.99") {
				t.Fatalf("object needing a newer glibc must name the missing version, got %+v", probe.Future)
			}
			if test.glibc {
				if probe.OpenCode.Status == nil || *probe.OpenCode.Status != 0 || probe.OpenCode.Stdout != "1.18.30" {
					t.Fatalf("spawned dependency executable = %+v", probe.OpenCode)
				}
			} else if probe.OpenCode.Status != nil && *probe.OpenCode.Status == 0 {
				t.Fatalf("glibc executable unexpectedly ran in a musl Workspace: %+v", probe.OpenCode)
			}
		})
	}
}
