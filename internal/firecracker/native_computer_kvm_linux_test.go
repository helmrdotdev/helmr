//go:build linux && computerproof

package firecracker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/nbd"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

// Remote objects are the only mutable input to the replacement VM. The model
// server and CP responder remain synthetic; this is not durable CP adoption or
// release-artifact qualification. Source/restore modes allow separate physical
// hosts; their host identities and object transport are verified by the operator.
func TestNativeComputerKVM(t *testing.T) {
	configPath := os.Getenv("HELMR_NATIVE_KVM_CONFIG")
	if configPath == "" {
		t.Skip("requires disposable native KVM configuration")
	}
	if os.Geteuid() != 0 || os.Getenv("HELMR_DISPOSABLE_VM_PROOF") != "1" {
		t.Fatal("explicit disposable root host required")
	}
	var input struct {
		Runtime                                                                     Config
		Arena, Helper, SeedDisk, RuntimeDisk, ProgramDisk, ModelConfig, RemoteStore string
		Mode, Continuation, HostIdentity                                            string
		Devices                                                                     []string
	}
	raw, err := os.ReadFile(configPath)
	nativeKVMMust(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	nativeKVMMust(t, decoder.Decode(&input))
	if input.RemoteStore == "" || len(input.Devices) == 0 {
		t.Fatal("remote store and exclusive NBD devices required")
	}
	for _, path := range []string{input.Arena, input.Helper, input.SeedDisk, input.RuntimeDisk, input.ProgramDisk, input.ModelConfig} {
		if !filepath.IsAbs(path) {
			t.Fatalf("absolute fixture path required: %s", path)
		}
	}
	if input.Mode != "" && input.Mode != "source" && input.Mode != "restore" {
		t.Fatal("invalid fixture mode")
	}
	if input.Mode != "" && (!filepath.IsAbs(input.Continuation) || input.HostIdentity == "") {
		t.Fatal("split fixture requires continuation file and observed host identity")
	}
	if input.Mode != "" {
		observed, err := os.ReadFile("/sys/devices/virtual/dmi/id/board_asset_tag")
		nativeKVMMust(t, err)
		if strings.TrimSpace(string(observed)) != input.HostIdentity {
			t.Fatal("host identity does not match DMI board asset tag")
		}
		t.Logf("observed host identity: %s", input.HostIdentity)
	}
	var saved nativeKVMSavedState
	if input.Mode == "restore" {
		body, err := os.ReadFile(input.Continuation)
		nativeKVMMust(t, err)
		nativeKVMMust(t, json.Unmarshal(body, &saved))
		if saved.HostIdentity == "" || saved.HostIdentity == input.HostIdentity || saved.RemoteStore != input.RemoteStore {
			t.Fatal("restore requires a different verified host and the same remote store")
		}
	}
	nativeKVMMust(t, os.Mkdir(input.Arena, 0700))
	t.Logf("retained native KVM arena: %s", input.Arena)
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Minute)
	defer cancel()
	var endpoints struct {
		Codex, Claude, Proof int
		Host                 string
	}
	raw, err = os.ReadFile(input.ModelConfig)
	nativeKVMMust(t, err)
	nativeKVMMust(t, json.Unmarshal(raw, &endpoints))
	if input.Mode == "restore" {
		var expected struct {
			Codex, Claude, Proof int
			Host                 string
		}
		nativeKVMMust(t, json.Unmarshal(saved.ModelConfig, &expected))
		if endpoints != expected {
			t.Fatal("restored model endpoints changed")
		}
	}
	if endpoints.Host != "203.0.113.1" {
		t.Fatal("model fixture must advertise 203.0.113.1")
	}
	for _, port := range []int{endpoints.Codex, endpoints.Claude, endpoints.Proof} {
		if port <= 1024 || port > 65535 {
			t.Fatal("invalid fixture port")
		}
	}
	cfg := input.Runtime.WithDefaults()
	cfg.StateDir = filepath.Join(input.Arena, "state")
	cfg.TempDir = filepath.Join(input.Arena, "tmp")
	cfg.JailerChrootBaseDir = filepath.Join(input.Arena, "jailer")
	// The real transparent transport forwards only two synthetic destinations to
	// host-loopback model servers. No Internet connection or credential is used.
	cfg.PrepareSecretTransport = func(context.Context, string, []netip.Prefix) (*secretproxy.Proxy, error) {
		denied := errors.New("no protected credentials in native fixture")
		ports := map[string]bool{strconv.Itoa(endpoints.Codex): true, strconv.Itoa(endpoints.Claude): true}
		return secretproxy.New(secretproxy.Config{
			Origins:            []string{fmt.Sprintf("https://fixture.invalid:%d", endpoints.Codex), fmt.Sprintf("https://fixture.invalid:%d", endpoints.Claude)},
			Certificate:        func(context.Context, string) (tls.Certificate, error) { return tls.Certificate{}, denied },
			Resolve:            func(context.Context, string, []string) (map[string][]byte, error) { return nil, denied },
			AllowedDestination: func(address netip.Addr) bool { return address.String() == endpoints.Host },
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil || host != endpoints.Host || !ports[port] {
					return nil, denied
				}
				return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
			},
		})
	}
	connector, err := NewConnector(cfg)
	nativeKVMMust(t, err)
	runtime, err := connector.Qualify(ctx)
	nativeKVMMust(t, err)
	identity, cpu, _, err := connector.boundMachineRuntime(cfg.VCPUCount)
	nativeKVMMust(t, err)
	remote, err := cass3.NewImmutable(ctx, input.RemoteStore, cass3.WithTempDir(input.Arena))
	nativeKVMMust(t, err)
	publisher := &kvmPublication{Reader: remote, upload: remote.Publish, certified: map[string]bool{}}
	var root disk.VersionRoot
	var capacity int64
	keyID := saved.KeyID
	phaseStarted := time.Now()
	if input.Mode == "restore" {
		root, capacity = saved.Root, saved.Root.LogicalBytes
		nativeKVMMust(t, root.Validate(capacity))
	} else {
		seed, err := os.Open(input.SeedDisk)
		nativeKVMMust(t, err)
		info, err := seed.Stat()
		nativeKVMMust(t, err)
		capacity = info.Size()
		keyID = uuid.NewV7().String()
		initial, err := disk.CaptureInitialVersion(ctx, disk.VersionCapture{Disk: seed, Capacity: capacity, StagingParent: input.Arena, Scope: "native-kvm", KeyID: keyID, Key: bytes.Repeat([]byte{17}, 32), Fanout: 64, PackLimit: blockformat.MinPackLimit, MaxStagedBytes: 1 << 30, MaxObjects: 10000})
		seed.Close()
		nativeKVMMust(t, err)
		locator, err := initial.Publish(ctx, publisher)
		initial.Close()
		nativeKVMMust(t, err)
		t.Logf("initial remote seed publication: %s", time.Since(phaseStarted))
		root, err = disk.NewVersionRoot(locator, capacity)
		nativeKVMMust(t, err)
	}
	keys := map[string][]byte{keyID: bytes.Repeat([]byte{17}, 32)}

	resources := vm.Resources{MilliCPU: cfg.VCPUCount * 1000, MemoryMiB: cfg.MemoryMiB, DiskMiB: cfg.ScratchDiskMiB, Slots: 1}
	computerID := uuid.NewV7().String()
	versionID := uuid.NewV7().String()
	var snapshot vm.SnapshotArtifact
	var captureRequest *agentv1.ComputerSessionCapture
	var peers []*nativeKVMPeer
	var first []nativeKVMResult
	var published []cas.Object
	startPhase, endPhase := 0, 2
	if input.Mode == "source" {
		endPhase = 1
	}
	if input.Mode == "restore" {
		startPhase = 1
		computerID, versionID = saved.ComputerID, saved.VersionID
		snapshot, captureRequest, published, first = saved.Snapshot, saved.Capture, saved.Published, saved.First
		if len(saved.Peers) != 2 || len(first) != 2 || len(published) != 3 || len(snapshot.Memory) != 1 || captureRequest == nil {
			t.Fatal("incomplete source continuation")
		}
		for _, state := range saved.Peers {
			if state.Grant.GetIdentity().GetSessionId() == "" || state.Completed != 1 || len(state.Receipts) == 0 {
				t.Fatal("incomplete saved Session fixture")
			}
			peer := newNativeKVMPeer(state.Grant)
			peer.receipts, peer.completed = state.Receipts, state.Completed
			peers = append(peers, peer)
		}
	}
	for phase := startPhase; phase < endPhase; phase++ {
		phaseStarted = time.Now()
		func() {
			dir := filepath.Join(input.Arena, fmt.Sprintf("physical-%d", phase))
			nativeKVMMust(t, os.Mkdir(dir, 0700))
			local, err := disk.CreateLocalVersion(ctx, disk.LocalVersionConfig{Directory: filepath.Join(dir, "version"), Base: root, BaseSource: remote, Scope: "native-kvm", ActiveKey: keyID, Keys: keys, DirtyBlocks: 1024, StagedBytes: 1 << 30, PackLimit: blockformat.MinPackLimit})
			nativeKVMMust(t, err)
			attachment := filepath.Join(dir, "attachment")
			nativeKVMMust(t, os.Mkdir(attachment, 0700))
			var device *disk.Device
			defer func() {
				if device == nil {
					if err := local.Close(); err != nil {
						t.Error(err)
					}
				}
			}()
			device, err = disk.AttachDevice(ctx, local, nbd.Config{Helper: input.Helper, Arena: attachment, Socket: filepath.Join(attachment, "nbd.sock"), Devices: input.Devices, Size: capacity})
			if device != nil {
				defer func() {
					cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
					defer stop()
					if err := device.Close(cleanup); err != nil {
						t.Error(err)
					}
				}()
			}
			nativeKVMMust(t, err)
			instance := uuid.NewV7().String()
			t.Logf("owned VM instance: %s", instance)
			owner := vm.Owner{Kind: vm.OwnerInstance, ID: instance}
			binding := vm.WorkloadBinding{WorkerEpoch: int64(phase + 1), OwnerID: instance, Generation: 1, ComputerInstanceID: instance, VMPlatformID: identity.ID}
			topology := vm.Topology{Computer: &vm.ComputerDisk{ComputerID: computerID, VersionID: versionID, SizeBytes: capacity, Device: device}}
			var machine vm.CheckpointableMachine
			if phase == 0 {
				machine, err = runtime.Materialize(ctx, vm.MaterializeRequest{ID: instance, OwnerKind: owner.Kind, Binding: binding, RootfsDigest: connector.artifacts.Rootfs.Digest, ComputerMountPath: "/workspace", Resources: resources, VMVCPUCount: int32(cfg.VCPUCount), CPUConfigDigest: cpu, Topology: topology})
			} else {
				paths := make([]string, len(published))
				for i, object := range published {
					paths[i] = filepath.Join(dir, fmt.Sprintf("remote-%d", i))
					nativeKVMDownload(t, ctx, remote, object, paths[i])
				}
				machine, err = runtime.Restore(ctx, vm.RestoreRequest{ID: captureRequest.CheckpointId, ComputerInstanceID: instance, OwnerKind: owner.Kind, Binding: binding, Resources: resources, VMState: paths[0], VMStateMediaType: snapshot.VMState.MediaType, ScratchDisk: paths[1], ScratchDiskMediaType: snapshot.ScratchDisk.MediaType, Memory: paths[2:], MemoryMediaTypes: []string{snapshot.Memory[0].MediaType}, Manifest: snapshot.Manifest, Checkpoint: vm.CheckpointIdentity{RuntimeBackend: snapshot.RuntimeBackend, RuntimeArch: snapshot.RuntimeArch, VMRuntimeContract: snapshot.VMRuntimeContract, RuntimeID: snapshot.RuntimeID, KernelDigest: snapshot.KernelDigest, InitramfsDigest: snapshot.InitramfsDigest, RootfsDigest: snapshot.RootfsDigest, VMConfigDigest: snapshot.VMConfigDigest, VMVCPUCount: snapshot.VMVCPUCount, CPUConfigDigest: snapshot.CPUConfigDigest}, Topology: topology})
			}
			nativeKVMMust(t, err)
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
				defer stop()
				if err := machine.Close(cleanup); err != nil {
					t.Error(err)
				}
				if err := runtime.Cleanup(cleanup, owner); err != nil {
					t.Error(err)
				}
			}()
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
				defer stop()
				for _, peer := range peers {
					if err := peer.close(cleanup); err != nil {
						t.Error(err)
					}
				}
			}()
			guest := machine.(*guestMachine)
			// Only TPROXY input/output reaches the host model. Drop every
			// forwarded packet (including DNS) in this exclusively owned guest
			// namespace before starting or thawing authored/native processes.
			nativeKVMMust(t, connector.applyNftScript(ctx, guest.networkBinding.manifest.NamespaceName,
				"add table inet native_fixture\nadd chain inet native_fixture forward { type filter hook forward priority 10; policy drop; }\n"))
			if phase == 0 {
				credential := uuid.NewV7().String()
				nativeKVMPrepare(t, ctx, guest, instance, credential, raw)
				for _, provider := range []string{"codex", "claude"} {
					grant := &agentv1.SessionGrant{Identity: &agentv1.SessionIdentity{SessionId: provider, ProcessEpoch: 1}, ComputerId: computerID, ComputerInstanceId: instance, WriterGeneration: 1, WorkerHostId: "source", ComputerLeaseEpoch: 2, AuthorityGeneration: 3, ExpiresAtUnixNano: time.Now().Add(20 * time.Minute).UnixNano(), ChannelCredential: credential}
					peer := newNativeKVMPeer(grant)
					runtime := nativeKVMProgramArtifact(t, input.RuntimeDisk)
					program := nativeKVMProgramArtifact(t, input.ProgramDisk)
					peer.program = &agentv1.SessionProgram{Runtime: runtime, Artifact: program}
					peer.programFiles = map[string]string{runtime.Digest: input.RuntimeDisk, program.Digest: input.ProgramDisk}
					nativeKVMMust(t, peer.attach(ctx, guest, 1, true))
					peers = append(peers, peer)
				}
				for _, peer := range peers {
					peer.waitReady(t, ctx)
					first = append(first, peer.turn(t, ctx, 1, 3, 2))
				}
				captureRequest = &agentv1.ComputerSessionCapture{Envelope: &computerv0.ComputerOperationEnvelope{OperationId: uuid.NewV7().String(), ComputerId: computerID, ComputerInstanceId: instance, WriterGeneration: 1, ChannelCredential: credential, OperationExpiresAtUnixNano: time.Now().Add(10 * time.Minute).UnixNano()}, CheckpointId: uuid.NewV7().String(), DesiredVersion: 1, MembershipRevision: 2}
				for _, peer := range peers {
					captureRequest.Sessions = append(captureRequest.Sessions, peer.grant.Identity)
				}
				hold, err := machine.BeginCheckpoint(ctx, vm.SnapshotRequest{ID: captureRequest.CheckpointId})
				nativeKVMMust(t, err)
				_, err = computerhost.CaptureAgentComputer(ctx, hold, captureRequest)
				nativeKVMMust(t, err)
				captureStarted := time.Now()
				snapshot, err = hold.CreateSnapshot(ctx)
				nativeKVMMust(t, err)
				t.Logf("RAM snapshot serialization: %s", time.Since(captureStarted))
				captureStarted = time.Now()
				publicationErr := snapshot.Computer.Capture.Publish(ctx, publisher)
				root = snapshot.Computer.Capture.Root()
				snapshot.Computer.Capture.Release()
				nativeKVMMust(t, publicationErr)
				for _, file := range append([]vm.SnapshotFile{snapshot.VMState, snapshot.ScratchDisk}, snapshot.Memory...) {
					published = append(published, nativeKVMPublish(t, ctx, remote, file))
					nativeKVMMust(t, os.Remove(file.Path))
				}
				t.Logf("remote snapshot publication: %s", time.Since(captureStarted))
				for _, peer := range peers {
					nativeKVMMust(t, peer.close(ctx))
				}
				versionID = uuid.NewV7().String()
			} else {
				envelope := proto.Clone(captureRequest.Envelope).(*computerv0.ComputerOperationEnvelope)
				envelope.OperationId = uuid.NewV7().String()
				envelope.ComputerInstanceId = instance
				envelope.WriterGeneration = 2
				envelope.ChannelCredential = uuid.NewV7().String()
				envelope.OperationExpiresAtUnixNano = time.Now().Add(5 * time.Minute).UnixNano()
				install := &agentv1.ComputerSessionInstallation{Capture: captureRequest, Envelope: envelope, DesiredVersion: 2, BaseComputerDiskVersionId: versionID}
				for _, peer := range peers {
					grant := proto.Clone(peer.grant).(*agentv1.SessionGrant)
					grant.ComputerInstanceId = instance
					grant.WriterGeneration = 2
					grant.WorkerHostId = "destination"
					grant.ComputerLeaseEpoch = 3
					grant.AuthorityGeneration = 4
					grant.ChannelCredential = envelope.ChannelCredential
					grant.ExpiresAtUnixNano = time.Now().Add(20 * time.Minute).UnixNano()
					install.Grants = append(install.Grants, grant)
				}
				_, err = computerhost.ContinueAgentComputer(ctx, guest, install, &nativeKVMContinuation{machine: guest, peers: peers})
				nativeKVMMust(t, err)
				for i, peer := range peers {
					peer.waitReady(t, ctx)
					second := peer.turn(t, ctx, 2, 4, 3)
					if second.PID != first[i].PID || second.NativePID != first[i].NativePID || second.Nonce != first[i].Nonce {
						t.Fatalf("native continuity: first=%+v restored=%+v", first[i], second)
					}
					nativeKVMMust(t, peer.close(ctx))
					t.Logf("%s restored same managed/native PIDs and setup nonce", peer.grant.Identity.SessionId)
				}
			}
		}()
		t.Logf("physical phase %d: %s", phase, time.Since(phaseStarted))
		if t.Failed() {
			return
		}
		if phase == 0 {
			nativeKVMMust(t, os.RemoveAll(filepath.Join(input.Arena, "physical-0")))
		}
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/", endpoints.Proof))
	nativeKVMMust(t, err)
	defer response.Body.Close()
	var history map[string][]json.RawMessage
	nativeKVMMust(t, json.NewDecoder(response.Body).Decode(&history))
	if input.Mode == "source" {
		snapshot.Computer = nil
		snapshot.VMState.Path, snapshot.ScratchDisk.Path = "", ""
		for i := range snapshot.Memory {
			snapshot.Memory[i].Path = ""
		}
		saved = nativeKVMSavedState{HostIdentity: input.HostIdentity, RemoteStore: input.RemoteStore, Root: root, KeyID: keyID, ComputerID: computerID, VersionID: versionID, Snapshot: snapshot, Capture: captureRequest, First: first, Published: published, ModelConfig: raw, ModelHistory: history}
		for _, peer := range peers {
			saved.Peers = append(saved.Peers, nativeKVMSavedPeer{Grant: peer.grant, Receipts: peer.receipts, Completed: peer.completed})
		}
		body, err := json.Marshal(saved)
		nativeKVMMust(t, err)
		file, err := os.OpenFile(input.Continuation, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		nativeKVMMust(t, err)
		_, err = file.Write(body)
		closeErr := file.Close()
		nativeKVMMust(t, err)
		nativeKVMMust(t, closeErr)
		object := nativeKVMPublish(t, ctx, remote, vm.SnapshotFile{Path: input.Continuation, MediaType: "application/json"})
		descriptor, err := json.Marshal(object)
		nativeKVMMust(t, err)
		nativeKVMMust(t, os.WriteFile(input.Continuation+".object.json", descriptor, 0600))
		t.Logf("source closed; continuation metadata published remotely: %s", object.Digest)
		return
	}
	for _, provider := range []string{"codex", "claude"} {
		requests := history[provider]
		if len(requests) < 4 {
			t.Fatal("missing native history")
		}
		last := string(requests[len(requests)-1])
		for _, marker := range []string{"unique input 1", "unique input 2", "fixture-tool-turn-" + provider + "-3"} {
			if !strings.Contains(last, marker) {
				t.Fatalf("%s missing native history/reply %s", provider, marker)
			}
		}
	}
}
func nativeKVMMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func nativeKVMProgramArtifact(t *testing.T, path string) *agentv1.SessionProgramArtifact {
	t.Helper()
	file, err := os.Open(path)
	nativeKVMMust(t, err)
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	nativeKVMMust(t, err)
	return &agentv1.SessionProgramArtifact{Digest: fmt.Sprintf("sha256:%x", hash.Sum(nil)), SizeBytes: size}
}
func nativeKVMPublish(t *testing.T, ctx context.Context, store *cass3.ImmutableStore, snapshot vm.SnapshotFile) cas.Object {
	t.Helper()
	// CreateSnapshot has completed these caller-owned files. Seal them before
	// handing read-only descriptors to the immutable remote publisher.
	nativeKVMMust(t, os.Chmod(snapshot.Path, 0400))
	file, err := os.Open(snapshot.Path)
	nativeKVMMust(t, err)
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	nativeKVMMust(t, err)
	_, err = file.Seek(0, io.SeekStart)
	nativeKVMMust(t, err)
	object, err := store.Publish(ctx, cas.Descriptor{Digest: fmt.Sprintf("sha256:%x", hash.Sum(nil)), SizeBytes: size, MediaType: snapshot.MediaType}, file)
	nativeKVMMust(t, err)
	return object
}
func nativeKVMDownload(t *testing.T, ctx context.Context, store cas.Reader, object cas.Object, path string) {
	t.Helper()
	reader, err := store.Get(ctx, object.Digest)
	nativeKVMMust(t, err)
	defer reader.Close()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	nativeKVMMust(t, err)
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, object.SizeBytes+1))
	closeErr := file.Close()
	nativeKVMMust(t, err)
	nativeKVMMust(t, closeErr)
	if size != object.SizeBytes || fmt.Sprintf("sha256:%x", hash.Sum(nil)) != object.Digest {
		t.Fatal("remote snapshot digest/size mismatch")
	}
}

func nativeKVMPrepare(t *testing.T, ctx context.Context, machine *guestMachine, instance, credential string, modelConfig []byte) {
	t.Helper()
	computer := machine.topology.Computer
	rpc := func(kind wire.StreamType, request, response proto.Message) {
		t.Helper()
		stream, err := onlineGuestStream(ctx, machine, kind)
		nativeKVMMust(t, err)
		defer stream.Close()
		nativeKVMMust(t, frameio.WriteProtoFrame(stream, request))
		nativeKVMMust(t, frameio.ReadProtoFrameBounded(stream, 1<<20, response))
	}
	var prepared computerv0.PrepareComputerRuntimeResponse
	rpc(wire.StreamTypeComputerRuntimePrepare, &computerv0.PrepareComputerRuntimeRequest{ComputerId: computer.ComputerID, ComputerInstanceId: instance, WriterGeneration: 1, MountPath: "/workspace", MountedImageConfig: &computerv0.RuntimeImageConfig{User: "1001:1001", WorkingDir: "/workspace", Env: []string{"PATH=/usr/local/bin:/usr/bin:/bin"}}}, &prepared)
	if prepared.Status != "prepared" {
		t.Fatalf("prepare: %v", &prepared)
	}
	var mounted computerv0.MaterializeComputerResponse
	rpc(wire.StreamTypeComputerMaterialize, &computerv0.MaterializeComputerRequest{Envelope: &computerv0.ComputerOperationEnvelope{ComputerId: computer.ComputerID, ComputerInstanceId: instance, WriterGeneration: 1, ChannelCredential: credential}, MountPath: "/workspace", Target: &computerv0.ComputerMountTarget{BaseComputerDiskVersionId: computer.VersionID}}, &mounted)
	if mounted.Status != "running" {
		t.Fatalf("materialize: %v", &mounted)
	}
	// JSON is passed as one argv value, never embedded in shell source.
	body, err := json.Marshal(map[string]any{"command": []string{"/bin/sh", "-ceu", `printf '%s' "$1" > /workspace/native-models.json`, "fixture", string(modelConfig)}, "cwd": "/workspace", "timeout_ms": 30000})
	nativeKVMMust(t, err)
	stream, err := onlineGuestStream(ctx, machine, wire.StreamTypeComputerBasicExec)
	nativeKVMMust(t, err)
	defer stream.Close()
	operation := uuid.NewV7().String()
	authority := &computerv0.ComputerCommandAuthority{OperationId: operation, ComputerId: computer.ComputerID, ComputerInstanceId: instance, ChannelCredential: credential, WriterGeneration: 1, OperationExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano(), RequestFingerprint: operation}
	nativeKVMMust(t, frameio.WriteProtoFrame(stream, &computerv0.ComputerBasicExecRequest{Envelope: authority, RequestJson: string(body)}))
	for {
		var event computerv0.ComputerBasicExecEvent
		nativeKVMMust(t, frameio.ReadProtoFrameBounded(stream, 1<<20, &event))
		if result := event.GetResult(); result != nil {
			if result.Outcome != "exited" || result.ExitCode != 0 {
				t.Fatalf("fixture endpoint write: %v", result)
			}
			// The fixture owns the outcome of this setup command. A terminal
			// reply alone does not release its retained Command receipt.
			var released computerv0.ComputerCommandReleaseResponse
			rpc(wire.StreamTypeComputerCommandRelease, &computerv0.ComputerCommandReleaseRequest{Authority: authority}, &released)
			if !released.GetReleased() {
				t.Fatalf("fixture endpoint command release: %v", &released)
			}
			return
		}
	}
}
