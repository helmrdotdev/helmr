package hostinstall

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/release"
)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type member struct {
	name string
	data []byte
	mode int64
	kind byte
}

// fixture builds a host and a runtime bundle that verify.
type fixture struct {
	t               *testing.T
	dir             string
	host, runtime   []member
	runtimeManifest map[string]any
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, dir: t.TempDir()}
	var files []map[string]any
	for _, file := range hostFiles {
		data := []byte("payload-" + file.path)
		f.host = append(f.host, member{name: file.path, data: data, mode: int64(file.mode)})
		files = append(files, map[string]any{"path": file.path, "mode": fmt.Sprintf("%04o", file.mode), "size_bytes": len(data), "digest": digestOf(data)})
	}
	raw, _ := json.Marshal(map[string]any{"schema": "helmr.worker-host-artifacts.v0", "arch": "amd64", "files": files})
	f.host = append(f.host, member{name: hostManifestName, data: raw, mode: 0o644})
	f.runtimeManifest = map[string]any{"schema": "helmr.runtime-artifacts.v0", "arch": "amd64", "vm_runtime_contract": runtimeContract}
	for _, entry := range []struct{ key, name, data string }{{"kernel", "vmlinuz", "kernel"}, {"initramfs", "initramfs", "init"}, {"rootfs", "rootfs.squashfs", "root"}} {
		f.runtime = append(f.runtime, member{name: entry.name, data: []byte(entry.data), mode: 0o444})
		f.runtimeManifest[entry.key] = map[string]any{"digest": digestOf([]byte(entry.data)), "size_bytes": len(entry.data)}
	}
	return f
}

func archive(t *testing.T, path string, members []member) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, m := range members {
		kind := m.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{Name: m.name, Mode: m.mode, Typeflag: kind, Format: tar.FormatUSTAR}
		if kind == tar.TypeReg {
			header.Size = int64(len(m.data))
		} else {
			header.Linkname = "/etc/passwd"
		}
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := w.Write(m.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (f *fixture) bundles() Bundles {
	runtimeRaw, _ := json.Marshal(f.runtimeManifest)
	runtime := append(slices.Clone(f.runtime), member{name: runtimeManifestName, data: runtimeRaw, mode: 0o444})
	var hostManifest []byte
	for _, m := range f.host {
		if m.name == hostManifestName {
			hostManifest = m.data
		}
	}
	hostPath, runtimePath := filepath.Join(f.dir, "host.tar"), filepath.Join(f.dir, "runtime.tar")
	return Bundles{
		HostPath: hostPath, RuntimePath: runtimePath,
		Host:            release.Asset{Digest: digestOf(archive(f.t, hostPath, f.host))},
		HostManifest:    release.Asset{Digest: digestOf(hostManifest)},
		Runtime:         release.Asset{Digest: digestOf(archive(f.t, runtimePath, runtime))},
		RuntimeManifest: release.Asset{Digest: digestOf(runtimeRaw)},
	}
}

func (f *fixture) verify() error {
	v, err := Verify(f.bundles(), f.t.TempDir())
	if err == nil {
		_ = v.Close()
	}
	return err
}

func TestVerifyAcceptsTheReleaseBundles(t *testing.T) {
	if err := newFixture(t).verify(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRefusesAlteredBundles(t *testing.T) {
	for name, alter := range map[string]func(f *fixture, b *Bundles){
		"bundle digest":   func(_ *fixture, b *Bundles) { b.Host.Digest = digestOf([]byte("other")) },
		"manifest digest": func(_ *fixture, b *Bundles) { b.HostManifest.Digest = digestOf([]byte("other")) },
		"runtime digest":  func(_ *fixture, b *Bundles) { b.Runtime.Digest = digestOf([]byte("other")) },
		"invalid digest":  func(_ *fixture, b *Bundles) { b.RuntimeManifest.Digest = "sha256:x" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			b := f.bundles()
			alter(f, &b)
			if _, err := Verify(b, t.TempDir()); err == nil {
				t.Fatal("verified")
			}
		})
	}
	for name, alter := range map[string]func(f *fixture){
		"host payload":    func(f *fixture) { f.host[0].data = []byte("tampered") },
		"host mode":       func(f *fixture) { f.host[0].mode = 0o777 },
		"runtime payload": func(f *fixture) { f.runtime[0].data = []byte("tampered") },
		"link member":     func(f *fixture) { f.host[0] = member{name: f.host[0].name, kind: tar.TypeSymlink} },
		"extra member":    func(f *fixture) { f.host = append(f.host, member{name: "extra", data: []byte("x"), mode: 0o644}) },
		"repeated member": func(f *fixture) { f.host = append(f.host, f.host[0]) },
		"traversal":       func(f *fixture) { f.host[0].name = "../escaped" },
		"missing member":  func(f *fixture) { f.host = f.host[1:] },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			alter(f)
			if err := f.verify(); err == nil {
				t.Fatal("verified")
			}
			if _, err := os.Stat(filepath.Join(f.dir, "..", "escaped")); err == nil {
				t.Fatal("a member escaped the install directory")
			}
		})
	}
}

// The runtime contract cases are shared with the image's own validation.
func TestVerifyRequiresTheRuntimeContract(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/runtime-artifacts/contracts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string          `json:"name"`
		Value json.RawMessage `json:"value"`
		Valid bool            `json:"valid"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			f := newFixture(t)
			delete(f.runtimeManifest, "vm_runtime_contract")
			if c.Value != nil {
				f.runtimeManifest["vm_runtime_contract"] = c.Value
			}
			if err := f.verify(); (err == nil) != c.Valid {
				t.Fatalf("verify = %v, want valid %v", err, c.Valid)
			}
		})
	}
}

func TestInstallPlacesVerifiedFiles(t *testing.T) {
	f := newFixture(t)
	v, err := Verify(f.bundles(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	root := t.TempDir()
	var calls []string
	state := "inactive"
	installer := Installer{Root: root, Run: func(name string, args ...string) (string, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		switch {
		case strings.HasPrefix(call, "systemctl show"):
			return state + "\n", nil
		case strings.HasPrefix(call, "id -"):
			return "1001\n", nil
		}
		return "", nil
	}}
	if err := installer.Install(v); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{"/usr/local/bin/worker": 0o755, "/usr/local/bin/helmr": 0o755, "/usr/share/helmr/mke2fs.conf": 0o444, "/var/lib/helmr/images/guest/out/rootfs.squashfs": 0o444} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s = %v, %v", path, info, err)
		}
	}
	unit, err := os.ReadFile(filepath.Join(root, "etc/systemd/system", ServiceName))
	if err != nil || !strings.Contains(string(unit), "RestartPreventExitStatus=78") {
		t.Fatalf("unit = %s, %v", unit, err)
	}
	if calls[len(calls)-1] != "systemctl daemon-reload" {
		t.Fatalf("calls = %v", calls)
	}

	state = "active"
	if err := installer.Install(v); err == nil || !strings.Contains(err.Error(), "stop and quiesce") {
		t.Fatalf("install over a running worker = %v", err)
	}
}
