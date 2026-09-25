//go:build linux && computerproof

package firecracker

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/nbd"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/vm"
)

// This opt-in qualification requires an operator-owned disposable KVM host.
// It exercises the production connector and NBD device, but not CP authority,
// remote storage, customer programs or RAM continuation. Keep failed arenas.
func TestComputerRuntimeKVM(t *testing.T) {
	path := os.Getenv("HELMR_COMPUTER_KVM_CONFIG")
	if path == "" {
		t.Skip("requires disposable KVM qualification configuration")
	}
	if os.Getenv("HELMR_DISPOSABLE_VM_PROOF") != "1" || os.Geteuid() != 0 {
		t.Fatal("explicit disposable root host required")
	}
	var input struct {
		Runtime Config
		Arena   string
		Helper  string
		Devices []string
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(input.Arena) || !filepath.IsAbs(input.Helper) || len(input.Devices) == 0 {
		t.Fatal("explicit arena, helper and devices required")
	}
	// The caller supplies a new private arena; never borrow an existing live host.
	if err := os.Mkdir(input.Arena, 0700); err != nil {
		t.Fatal(err)
	}
	t.Logf("retained qualification arena: %s", input.Arena)
	cfg := input.Runtime.WithDefaults()
	cfg.StateDir = filepath.Join(input.Arena, "state")
	cfg.TempDir = filepath.Join(input.Arena, "tmp")
	cfg.JailerChrootBaseDir = filepath.Join(input.Arena, "jailer")
	// No credentials or protected origins are present in this local fixture.
	// Keep the real transport and fail closed if it is unexpectedly used.
	cfg.PrepareSecretTransport = func(context.Context, string, []netip.Prefix) (*secretproxy.Proxy, error) {
		denied := errors.New("network unavailable in qualification fixture")
		return secretproxy.New(secretproxy.Config{
			Certificate:        func(context.Context, string) (tls.Certificate, error) { return tls.Certificate{}, denied },
			Resolve:            func(context.Context, string, []string) (map[string][]byte, error) { return nil, denied },
			AllowedDestination: func(netip.Addr) bool { return false },
			DialContext:        func(context.Context, string, string) (net.Conn, error) { return nil, denied },
		})
	}
	connector, err := NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	runtime, err := connector.Qualify(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("production qualification=%s", time.Since(start))
	source := filepath.Join(input.Arena, "seed.ext4")
	const filesystemBytes = int64(64 << 20)
	const capacity = filesystemBytes + 4096
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(filesystemBytes); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	computerProofCommand(t, "mke2fs", "-q", "-t", "ext4", "-F", source)
	// The final block is outside ext4. Host-injected synthetic markers identify
	// consecutive captures without modifying a mounted guest filesystem. This
	// tests generation freshness, not guest application write/fsync semantics.
	if err := os.Truncate(source, capacity); err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(filepath.Join(input.Arena, "published"))
	if err != nil {
		t.Fatal(err)
	}
	publisher := &kvmPublication{filesystemPublication: filesystemPublication{store}, certified: map[string]bool{}}
	keyID := uuid.NewV7().String()
	keys := map[string][]byte{keyID: bytes.Repeat([]byte{11}, 32)} // synthetic data only
	f, err = os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := computer.CaptureInitialGeneration(t.Context(), computer.GenerationCapture{Disk: f, Capacity: capacity, StagingParent: input.Arena, Scope: "kvm-qualification", KeyID: keyID, Key: keys[keyID], Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 256 << 20, MaxObjects: 10000})
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	locator, err := initial.Publish(t.Context(), publisher)
	initial.Close()
	if err != nil {
		t.Fatal(err)
	}
	root, err := computer.NewGenerationRoot(locator, capacity)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(source); err != nil {
		t.Fatal(err)
	}
	var wantMarker []byte
	verifyMarker := func(root computer.GenerationRoot) {
		tree, err := computer.OpenGeneration(t.Context(), store, "kvm-qualification", keys, root, capacity)
		if err != nil {
			t.Fatal(err)
		}
		got, err := tree.ReadRange(t.Context(), filesystemBytes, len(wantMarker))
		if err != nil || !bytes.Equal(got, wantMarker) {
			t.Fatalf("published marker mismatch: %v", err)
		}
	}
	// Each iteration uses a fresh local generation; only published objects cross
	// the boundary. No previous mutable directory or device is reused.
	for _, name := range []string{"initial", "cold"} {
		func() {
			dir := filepath.Join(input.Arena, name)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			disk, err := computer.CreateLocalGeneration(t.Context(), computer.LocalGenerationConfig{Directory: filepath.Join(dir, "generation"), Base: root, BaseSource: store, Scope: "kvm-qualification", ActiveKey: keyID, Keys: keys, DirtyBlocks: 256, StagedBytes: 256 << 20, PackLimit: blockformat.MinPackLimit})
			if err != nil {
				t.Fatal(err)
			}
			arena := filepath.Join(dir, "attachment")
			if err := os.Mkdir(arena, 0700); err != nil {
				disk.Close()
				t.Fatal(err)
			}
			device, err := computer.AttachDevice(t.Context(), disk, nbd.Config{Helper: input.Helper, Arena: arena, Socket: filepath.Join(arena, "nbd.sock"), Devices: input.Devices, Size: capacity})
			if device != nil {
				defer func() {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					if err := device.Close(ctx); err != nil {
						t.Errorf("device cleanup: %v", err)
					}
				}()
			}
			if err != nil {
				t.Fatal(err)
			}
			identity, cpu, _, err := connector.boundSessionRuntime(cfg.VCPUCount)
			if err != nil {
				t.Fatal(err)
			}
			id := uuid.NewV7().String()
			owner := vm.Owner{Kind: vm.OwnerRuntime, ID: id}
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := runtime.Cleanup(ctx, owner); err != nil {
					t.Errorf("runtime cleanup: %v", err)
				}
			}()
			start := time.Now()
			session, err := runtime.Materialize(t.Context(), vm.MaterializeRequest{ID: id, OwnerKind: owner.Kind, Binding: vm.WorkloadBinding{WorkerEpoch: 1, OwnerID: id, Generation: 1, RuntimeInstanceID: id, RuntimeIdentityID: identity.ID}, RootfsDigest: connector.artifacts.Rootfs.Digest, WorkspaceMountPath: "/workspace", Resources: compute.ResourceVector{MilliCPU: cfg.VCPUCount * 1000, MemoryMiB: cfg.MemoryMiB, DiskMiB: cfg.ScratchDiskMiB, Slots: 1}, VMVCPUCount: int32(cfg.VCPUCount), CPUConfigDigest: cpu, Topology: vm.RuntimeTopology{Computer: &vm.RuntimeComputer{ComputerID: uuid.NewV7().String(), VersionID: uuid.NewV7().String(), SizeBytes: capacity, Device: device}}})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s materialize=%s", name, time.Since(start))
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := session.Close(ctx); err != nil {
					t.Errorf("session close: %v", err)
				}
			}()
			if len(wantMarker) != 0 {
				got := make([]byte, len(wantMarker))
				n, err := disk.ReadAt(t.Context(), got, filesystemBytes)
				if err != nil || n != len(got) || !bytes.Equal(got, wantMarker) {
					t.Fatalf("cold marker mismatch: %d %v", n, err)
				}
			}
			live := session.(interface {
				CaptureComputer(context.Context) (*vm.ComputerSnapshot, error)
			})
			for i := 0; i < 2; i++ {
				wantMarker = bytes.Repeat([]byte{0}, 4096)
				copy(wantMarker, fmt.Sprintf("%s/capture/%d", name, i))
				if n, err := disk.WriteAt(t.Context(), wantMarker, filesystemBytes); err != nil || n != len(wantMarker) {
					t.Fatalf("write marker: %d %v", n, err)
				}
				start = time.Now()
				cut, err := live.CaptureComputer(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				pause := time.Since(start)
				// Require a fresh application health response before publication; a
				// successful host-side resume response alone is not guest progress.
				guest := session.(*guestSession)
				err = connector.waitForHealth(t.Context(), guest.vsockHostPath, guest.machineExit, t.Logf)
				if err != nil {
					cut.Capture.Release()
					t.Fatal(err)
				}
				start = time.Now()
				err = cut.Capture.Publish(t.Context(), publisher)
				root = cut.Capture.Root()
				cut.Capture.Release()
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("%s capture=%d pause_and_capture=%s publish=%s", name, i, pause, time.Since(start))
				verifyMarker(root)
			}
			cut, err := session.(vm.ComputerCaptureSession).PauseComputer(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := live.CaptureComputer(t.Context()); err == nil {
				cut.Capture.Release()
				t.Fatal("terminal hold resumed")
			}
			err = cut.Capture.Publish(t.Context(), publisher)
			root = cut.Capture.Root()
			cut.Capture.Release()
			if err != nil {
				t.Fatal(err)
			}
		}()
		if t.Failed() {
			return
		}
		verifyMarker(root)
	}
	t.Log("production Computer capture and cold materialization passed; no CP or RAM continuation claim")
}

type kvmPublication struct {
	filesystemPublication
	certified map[string]bool
}

func (p *kvmPublication) Certify(_ context.Context, e blockformat.ObjectInspection) error {
	raw, err := json.Marshal(e)
	if err == nil {
		p.certified[string(raw)] = true
	}
	return err
}
func (p *kvmPublication) Reuse(_ context.Context, e blockformat.ObjectInspection) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if !p.certified[string(raw)] {
		return errors.New("uncertified fixture object")
	}
	return nil
}
