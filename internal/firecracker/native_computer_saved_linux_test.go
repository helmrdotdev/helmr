//go:build linux && computerproof

package firecracker

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
	"testing"
)

// Operator-carried metadata for the split native fixture. Mutable disks and RAM
// remain exclusively in the remote CAS. Keys and authority are synthetic fixture
// values; this is not a Control Plane checkpoint or activation receipt.
type nativeKVMSavedState struct {
	HostIdentity, RemoteStore, KeyID, ComputerID, VersionID string
	Root                                                    disk.VersionRoot
	Snapshot                                                vm.SnapshotArtifact
	Capture                                                 *agentv1.ComputerSessionCapture
	Published                                               []cas.Object
	First                                                   []nativeKVMResult
	Peers                                                   []nativeKVMSavedPeer
	ModelConfig                                             json.RawMessage
	ModelHistory                                            map[string][]json.RawMessage
}
type nativeKVMSavedPeer struct {
	Grant     *agentv1.SessionGrant
	Receipts  map[string]nativeKVMReceipt
	Completed int
}

func TestNativeKVMSavedCaptureRoundTrip(t *testing.T) {
	identity := &agentv1.SessionIdentity{SessionId: "codex", ProcessEpoch: 1}
	grant := &agentv1.SessionGrant{Identity: identity, ComputerId: "computer", ComputerInstanceId: "source-instance", WriterGeneration: 1, WorkerHostId: "source", ComputerLeaseEpoch: 2, AuthorityGeneration: 3, ExpiresAtUnixNano: 1791170000123456789, ChannelCredential: "synthetic"}
	capture := &agentv1.ComputerSessionCapture{Envelope: &computerv0.ComputerOperationEnvelope{OperationId: "capture", ComputerId: "computer", ComputerInstanceId: "source-instance", WriterGeneration: 1, ChannelCredential: "synthetic", OperationExpiresAtUnixNano: 1791170000987654321}, CheckpointId: "checkpoint", DesiredVersion: 1, MembershipRevision: 2, Sessions: []*agentv1.SessionIdentity{identity}}
	original := nativeKVMSavedState{Capture: capture, Peers: []nativeKVMSavedPeer{{Grant: grant, Completed: 1}}}
	data, err := json.Marshal(original)
	nativeKVMMust(t, err)
	var restored nativeKVMSavedState
	nativeKVMMust(t, json.Unmarshal(data, &restored))
	if !proto.Equal(capture, restored.Capture) || len(restored.Peers) != 1 || !proto.Equal(grant, restored.Peers[0].Grant) {
		t.Fatal("saved metadata changed exact capture or source grant")
	}
}
