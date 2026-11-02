package controlplane

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"
)

// workerRouteTenancyExempt lists /worker/v1 routes that name no resource of
// another tenant. Each authenticates and addresses only the calling Host.
var workerRouteTenancyExempt = map[string]string{
	"POST /worker/v1/enrollment":              "bootstrap; names a group token, not a resource",
	"POST /worker/v1/instance/credential":     "exchanges the caller's own host secret",
	"POST /worker/v1/instance/recover":        "the caller's own host incarnation",
	"POST /worker/v1/instance/activate":       "the caller's own host incarnation",
	"GET /worker/v1/instance":                 "the caller's own host status",
	"POST /worker/v1/instance/drain/complete": "the caller's own host incarnation",
	"POST /worker/v1/instance/fence":          "the caller's own host incarnation",
	"POST /worker/v1/instance/observations":   "the caller's own host observations",
	"POST /worker/v1/instance/drain":          "the caller's own host incarnation",
	"POST /worker/v1/allocations/list":        "lists only allocations held by the caller; the cursor is a position, not authority",
	"GET /worker/v1/updates":                  "streams only the caller's own updates",
}

// tenancyVictim is organization A: the sessiontest fixture plus every durable
// owner the /worker/v1 routes address, all held by host f.Worker in f.Group.
type tenancyVictim struct {
	f          sessiontest.Fixture
	imageBuild workerapi.PreparationExecutor
	bootSeed   workerapi.BootSeedExecutor
	command    uuid.UUID
	capture    workerapi.AgentComputerCaptureRequest
	saveID     string
	captured   *agentv1.ComputerSessionCapture
	captureRaw []byte
	root       disk.VersionRoot
	baseDigest string
	inspection blockformat.ObjectInspection
	observedAt time.Time
	bootIDs    [2]string
	// leaseEpoch is the Computer lease epoch session bodies name; current is
	// the victim's own current epoch for its positive-control request.
	leaseEpoch, currentEpoch int64
	proxyMarker              string
	bundleRaw                []byte
	bundle                   bundle.Manifest
	keys                     *computerkey.Local
	cas                      testUploadStore
	secrets                  *secret.Store
	// host is the victim host's secret; handler serves its setup requests.
	host    seededHostSecret
	handler http.Handler
	// attachment, turnID and messageID are the Session runtime identities the
	// session bodies name; case setups replace them with current ones.
	attachment                  int64
	turnID, messageID, outputID string
	// restoreHost holds the restore target lease.
	restoreHost uuid.UUID
	// restore is the prepared restore installation and restoreReceipt the
	// guest receipt for the restore stage a case setup reached.
	restore        []byte
	restoreReceipt *agentv1.ComputerSessionReceipt
	restoreTarget  uuid.UUID
}

// configure gives the victim's servers working Program, key and Secret
// dependencies, so requests reach ownership checks instead of a missing
// dependency.
func (v *tenancyVictim) configure(cfg *ServerConfig) {
	if v.secrets != nil {
		cfg.SecretProxy, cfg.SecretDelivery = v.secrets, v.secrets
	}
	if v.keys != nil {
		cfg.ComputerKeys = v.keys
	}
	if v.cas.File != nil {
		cfg.CAS = func(uuid.UUID) cas.UploadStore { return v.cas }
	}
}

// tenancyIntruder is an authenticated host that must not reach the victim's
// resources. Organization-owned worker groups extend here: set the group's
// owner when inserting it so every case runs against that boundary too.
type tenancyIntruder struct {
	name       string
	groupID    uuid.UUID
	hostID     uuid.UUID
	sameGroup  bool
	credential string
}

type workerRouteTenancyCase struct {
	route string
	// name distinguishes several cases of one route, such as operations.
	name string
	// crossHost runs the case with a replacement host holding the restore
	// target lease as the victim; the fenced source host becomes an intruder.
	crossHost bool
	// held names the victim owner the body addresses; its row is checked to
	// exist and be held by the victim host before the comparison.
	held string
	// inBand marks a route that reports refusal as a 200 operation response
	// carrying an error rather than as an HTTP error status.
	inBand bool
	// sameGroupReader marks a resource that every host of the serving group may
	// read by design: a same-group host must succeed, and only the other-group
	// intruder is compared with missing.
	sameGroupReader bool
	// beforeHostLock marks a route that refuses an unheld identifier before it
	// locks the caller's own host rows, so it is also compared while another
	// transaction holds those rows.
	beforeHostLock bool
	// setup moves the per-case victim into the state the route addresses.
	setup func(t *testing.T, v *tenancyVictim)
	// victimStatus is the status of the victim host's own request with the real
	// IDs when that request is expected to succeed. Zero means the victim
	// request needs setup beyond this fixture. Either way its response must
	// differ from every intruder refusal in status, body or refusal audit,
	// proving the case reaches authorization.
	victimStatus int
	// body builds the request from victim IDs. Opaque protobuf fields pass
	// through ids so their embedded identifiers are rewritten with the JSON.
	body func(v *tenancyVictim, ids func([]byte) []byte) any
}

func tenancyNetwork() *workerapi.NetworkCapabilities {
	return &workerapi.NetworkCapabilities{Version: workerapi.NetworkPolicyVersion, Installation: (networkpolicy.Installation{}).Canonical()}
}

func (v *tenancyVictim) lease() workerapi.AllocationIdentity {
	return workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: v.f.Environment.String(), OwnerID: v.f.Computer.String(), Epoch: 1, AllocationID: v.f.Computer.String()}
}

func (v *tenancyVictim) session() workerapi.RuntimeSession {
	return workerapi.RuntimeSession{EnvironmentID: v.f.Environment.String(), SessionID: v.f.Session.String(), ProcessEpoch: 1, ComputerLeaseEpoch: v.leaseEpoch}
}

func (v *tenancyVictim) save() workerapi.AgentSave {
	return workerapi.AgentSave{EnvironmentID: v.f.Environment.String(), SaveID: v.saveID, LeaseEpoch: 1}
}

func (v *tenancyVictim) imageWithNetwork() workerapi.PreparationExecutor {
	e := v.imageBuild
	e.Network = tenancyNetwork()
	return e
}

func (v *tenancyVictim) seedWithNetwork() workerapi.BootSeedExecutor {
	e := v.bootSeed
	e.Network = tenancyNetwork()
	return e
}

func (v *tenancyVictim) receipt(ids func([]byte) []byte) []byte {
	return ids(tenancyProto(&agentv1.ComputerSessionReceipt{CheckpointId: v.captured.CheckpointId, DesiredVersion: v.captured.DesiredVersion, Frozen: true}))
}

func (v *tenancyVictim) installation(ids func([]byte) []byte, sourceAbort bool) workerapi.AgentComputerInstallationRequest {
	envelope := proto.Clone(v.captured.Envelope).(*computerv0.ComputerOperationEnvelope)
	p := &agentv1.ComputerSessionInstallation{Capture: v.captured, Envelope: envelope, DesiredVersion: v.captured.DesiredVersion + 1, SourceAbort: sourceAbort, BaseComputerDiskVersionId: v.baseDigest}
	for _, identity := range v.captured.Sessions {
		p.Grants = append(p.Grants, &agentv1.SessionGrant{Identity: identity, ComputerId: envelope.ComputerId, ComputerInstanceId: envelope.ComputerInstanceId, WriterGeneration: 1, ComputerLeaseEpoch: 1, WorkerHostId: v.f.Worker.String(), AuthorityGeneration: 1, ExpiresAtUnixNano: envelope.OperationExpiresAtUnixNano, ChannelCredential: envelope.ChannelCredential})
	}
	return workerapi.AgentComputerInstallationRequest{EnvironmentID: v.f.Environment.String(), Installation: ids(tenancyProto(p)), Receipt: v.receipt(ids)}
}

func tenancyProto(p proto.Message) []byte {
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(p)
	if err != nil {
		panic(err)
	}
	return raw
}

func (v *tenancyVictim) checkpointManifest() computercheckpoint.Manifest {
	config := []byte(`{"checkpoint":"runtime"}`)
	digest := sha256.Sum256(v.captureRaw)
	platform := "sha256:" + strings.Repeat("1", 64)
	m := computercheckpoint.Manifest{MemoryHeaderDigest: sha256sum.DigestBytes([]byte("header")), CheckpointID: uuid.MustParse(v.capture.CheckpointID), ComputerID: v.f.Computer, InstanceID: v.f.Computer, LeaseEpoch: 1, ControlVersion: v.captured.DesiredVersion, CaptureDigest: digest[:], Config: config, Disk: v.root, Runtime: vm.CheckpointIdentity{RuntimeBackend: "firecracker", RuntimeArch: "x86_64", VMRuntimeContract: "helmr.vm-runtime.v0", RuntimeID: platform, KernelDigest: platform, InitramfsDigest: platform, RootfsDigest: platform, VMConfigDigest: sha256sum.DigestBytes(config), VMVCPUCount: 1, CPUConfigDigest: platform}}
	for _, s := range v.captured.Sessions {
		m.Members = append(m.Members, computercheckpoint.Session{SessionID: uuid.MustParse(s.SessionId), ProcessEpoch: s.ProcessEpoch})
	}
	descriptors := []*computercheckpoint.Object{&m.VMConfig, &m.VMState, &m.Memory, &m.ScratchDisk}
	media := []string{cas.CheckpointVMConfigMediaType, cas.CheckpointVMStateMediaType, cas.CheckpointMemoryMediaType, cas.CheckpointScratchDiskMediaType}
	for i, d := range descriptors {
		raw := []byte("opaque runtime object: " + media[i])
		*d = computercheckpoint.Object{Digest: sha256sum.DigestBytes(raw), SizeBytes: int64(len(raw)), MediaType: media[i]}
	}
	return m
}

func (v *tenancyVictim) bootSeedPublication() workerapi.BootSeedPublication {
	object := func(media, content string) computercheckpoint.Object {
		return computercheckpoint.Object{Digest: sha256sum.DigestBytes([]byte(content)), SizeBytes: int64(len(content)), MediaType: media}
	}
	return v.bootSeedPublicationFor(computercheckpoint.Program{Runtime: object(artifact.RuntimeArtifactMediaType, "runtime"), Artifact: object(artifact.ProgramArtifactMediaType, "program")})
}

func (v *tenancyVictim) bootSeedPublicationFor(program computercheckpoint.Program) workerapi.BootSeedPublication {
	object := func(media, content string) cas.Descriptor {
		return cas.Descriptor{Digest: sha256sum.DigestBytes([]byte(content)), SizeBytes: int64(len(content)), MediaType: media}
	}
	rootDigest, err := v.root.Digest()
	if err != nil {
		panic(err)
	}
	platform := "sha256:" + strings.Repeat("1", 64)
	config, err := json.Marshal(bootseed.Content{Contract: "helmr.boot-seed.v1", RuntimeID: platform, CPUConfig: platform, VCPUs: 1, MemoryMiB: 512, ScratchMiB: 1024, ComputerRoot: rootDigest, ComputerBytes: v.root.LogicalBytes, Program: &program})
	if err != nil {
		panic(err)
	}
	m := bootseed.Manifest{EnvironmentID: v.f.Environment, BuildID: uuid.MustParse(v.bootSeed.Identity.OwnerID), Config: config, MemoryBytes: 536870912, ScratchBytes: 1073741824, MemoryHeaderDigest: sha256sum.DigestBytes([]byte("header")), VMState: object(cas.BootSeedVMStateMediaType, "state"), Memory: object(cas.BootSeedMemoryMediaType, "memory"), ScratchDisk: object(cas.BootSeedScratchMediaType, "scratch")}
	proof := bootseed.Qualification{Clones: [2]vm.BootSeedIdentity{{BootID: v.bootIDs[0], RandomDigest: sha256sum.DigestBytes([]byte("one"))}, {BootID: v.bootIDs[1], RandomDigest: sha256sum.DigestBytes([]byte("two"))}}}
	return workerapi.BootSeedPublication{Executor: v.bootSeed, Manifest: m, Qualification: proof}
}

// workerRouteTenancyCases holds one case per /worker/v1 route that names a
// resource. Add a case with every new route; the route guard enforces it.
var workerRouteTenancyCases = []workerRouteTenancyCase{
	{route: "/worker/v1/preparation-sources", held: "eligible_boot_seed", setup: qualifyTenancyBootSeed, victimStatus: http.StatusOK,
		// Preparation sources are group-wide by design: the operator fleet of a
		// region serves every organization whose project defaults to it, so a
		// same-group host is an intended reader. Only other groups are refused.
		sameGroupReader: true,
		body: func(v *tenancyVictim, _ func([]byte) []byte) any {
			return workerapi.PreparationSourceRequest{Network: tenancyNetwork(), EnvironmentID: v.f.Environment.String(), BuildID: v.bootSeed.Identity.OwnerID}
		}},
	{route: "/worker/v1/allocations/boot-seed/deliver", victimStatus: http.StatusOK, held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AllocationRequest{AllocationIdentity: v.bootSeed.Identity, Network: tenancyNetwork()}
	}},
	{route: "/worker/v1/allocations/boot-seed/start", held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.seedWithNetwork() }},
	{route: "/worker/v1/allocations/boot-seed/renew", held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.seedWithNetwork() }},
	{route: "/worker/v1/allocations/boot-seed/publish", held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.bootSeedPublication() }},
	{route: "/worker/v1/allocations/boot-seed/fail", held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.BootSeedFailure{Executor: v.bootSeed, Code: "boot_seed_failed"}
	}},
	{route: "/worker/v1/allocations/boot-seed/reclaim", setup: reclaimableTenancyBootSeed, victimStatus: http.StatusNoContent, held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.bootSeed }},
	{route: "/worker/v1/allocations/boot-seed/stopped", held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.bootSeed.Identity }},
	{route: "/worker/v1/allocations/image-build/deliver", victimStatus: http.StatusOK, held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AllocationRequest{AllocationIdentity: v.imageBuild.Identity, Network: tenancyNetwork()}
	}},
	{route: "/worker/v1/allocations/image-build/stopped", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.imageBuild.Identity }},
	{route: "/worker/v1/allocations/image-build/renew", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.imageWithNetwork() }},
	{route: "/worker/v1/allocations/image-build/key", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.imageBuild }},
	{route: "/worker/v1/allocations/image-build/secrets", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.imageBuild }},
	{route: "/worker/v1/allocations/image-build/start", setup: startableTenancyImageBuild, victimStatus: http.StatusOK, held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.imageWithNetwork() }},
	{route: "/worker/v1/allocations/image-build/fail", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.PreparationFailure{Executor: v.imageBuild, Code: "authored_preparation_failed"}
	}},
	{route: "/worker/v1/allocations/image-build/capture/begin", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.PreparationCaptureBegin{Executor: v.imageBuild, LogicalBytes: disk.SeedCapacity}
	}},
	{route: "/worker/v1/allocations/image-build/objects/register", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.PreparationObject{Executor: v.imageBuild, Inspections: []blockformat.ObjectInspection{v.inspection}}
	}},
	{route: "/worker/v1/allocations/image-build/objects/certify", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.PreparationObject{Executor: v.imageBuild, Inspections: []blockformat.ObjectInspection{v.inspection}}
	}},
	{route: "/worker/v1/allocations/image-build/capture", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.PreparationPublication{Executor: v.imageBuild, Root: v.root, Evidence: "coherent preparation cut"}
	}},
	{route: "/worker/v1/allocations/image-build/publish", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.PreparationPublication{Executor: v.imageBuild, Root: v.root, Evidence: "certified preparation root"}
	}},
	{route: "/worker/v1/allocations/image-build/logs", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.PreparationLogRequest{Executor: v.imageBuild, Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: v.observedAt.UnixNano(), Data: []byte("output")}
	}},
	{route: "/worker/v1/agent-computers/checkpoint/key", setup: sealTenancyCapture, held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.CheckpointKeyRequest{Identity: v.lease(), CheckpointID: v.capture.CheckpointID}
	}},
	{route: "/worker/v1/boot-seeds/key", held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.BootSeedKeyRequest{Scope: workerapi.StorageScope{BootSeed: &v.bootSeed}, BuildID: v.bootSeed.Identity.OwnerID}
	}},
	{route: "/worker/v1/boot-seeds/key", name: "preparation source", setup: offerTenancyBootSeed, held: "eligible_boot_seed", victimStatus: http.StatusOK,
		// Like the preparation source itself, its seed key goes to every host of
		// the serving group; only other groups are refused.
		sameGroupReader: true,
		body: func(v *tenancyVictim, _ func([]byte) []byte) any {
			return workerapi.BootSeedKeyRequest{Scope: workerapi.StorageScope{PreparationSource: &workerapi.PreparationSourceID{EnvironmentID: v.f.Environment.String(), BuildID: v.bootSeed.Identity.OwnerID}}, BuildID: v.bootSeed.Identity.OwnerID}
		}},
	{route: "/worker/v1/boot-seeds/key", name: "computer", setup: selectTenancyBootSeed, held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.BootSeedKeyRequest{Scope: v.storageScope(), BuildID: v.bootSeed.Identity.OwnerID}
	}},
	{route: "/worker/v1/storage/reads", setup: receiptTenancyUpload, held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.StorageReadRequest{Scope: v.storageScope(), Store: workerapi.StorageCAS, Digest: tenancyUpload.Digest}
	}},
	{route: "/worker/v1/storage/writes", held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.StorageWriteRequest{Scope: v.storageScope(), Object: tenancyUpload}
	}},
	{route: "/worker/v1/storage/writes", name: "image build", held: "image_build", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.StorageWriteRequest{Scope: workerapi.StorageScope{Preparation: &v.imageBuild}, Object: tenancyUpload}
	}},
	{route: "/worker/v1/storage/writes", name: "boot seed", held: "boot_seed", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.StorageWriteRequest{Scope: workerapi.StorageScope{BootSeed: &v.bootSeed}, Object: tenancyUpload}
	}},
	{route: "/worker/v1/storage/writes/parts", setup: receiptTenancyUpload, held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.StoragePartsRequest{Scope: v.storageScope(), Object: tenancyUpload, UploadID: tenancyUploadID, Parts: []int32{1}}
	}},
	{route: "/worker/v1/storage/writes/complete", setup: receiptTenancyUpload, held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.StorageCompleteRequest{Scope: v.storageScope(), Object: tenancyUpload, UploadID: tenancyUploadID, Parts: []cas.CompletedPart{{Number: 1, ETag: "part"}}}
	}},
	{route: "/worker/v1/storage/writes/abort", setup: receiptTenancyUpload, held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.StorageAbortRequest{Scope: v.storageScope(), Object: tenancyUpload, UploadID: tenancyUploadID}
	}},
	{route: "/worker/v1/computer-saves/next", held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentSaveDiscovery{AllocationIdentity: v.lease(), Admit: true}
	}},
	{route: "/worker/v1/computer-storage/pressure", held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentStoragePressure{AllocationIdentity: v.lease(), Active: true}
	}},
	{route: "/worker/v1/computer-saves/objects/register", setup: bindTenancyLeaseDisk, held: "save", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentSaveObject{Save: v.save(), Inspections: []blockformat.ObjectInspection{v.inspection}}
	}},
	{route: "/worker/v1/computer-saves/objects/certify", setup: bindTenancyLeaseDisk, held: "save", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentSaveObject{Save: v.save(), Inspections: []blockformat.ObjectInspection{v.inspection}}
	}},
	{route: "/worker/v1/computer-saves/capture", held: "save", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentSavePublication{Save: v.save(), Root: v.root, Evidence: "coherent cut"}
	}},
	{route: "/worker/v1/computer-saves/publish", held: "save", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentSavePublication{Save: v.save(), Root: v.root, Evidence: "certified root"}
	}},
	{route: "/worker/v1/allocations/computer/deliver", setup: deliverableTenancyLease, victimStatus: http.StatusOK, held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AllocationRequest{AllocationIdentity: v.lease(), Network: tenancyNetwork()}
	}},
	{route: "/worker/v1/allocations/computer/ready", held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		platform := "sha256:" + strings.Repeat("1", 64)
		return workerapi.ComputerAllocationReady{Identity: v.lease(), Shape: workerapi.AllocationShape{CPUMillis: 1000, MemoryBytes: 536870912, ScratchBytes: 1073741824, VMPlatformID: platform, VCPUCount: 1, CPUConfigDigest: platform}, BaseVersion: v.baseDigest}
	}},
	{route: "/worker/v1/allocations/computer/processes", held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.ComputerProcessesRequest{Identity: v.lease()}
	}},
	{route: "/worker/v1/allocations/computer/source", held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AllocationRequest{AllocationIdentity: v.lease(), Network: tenancyNetwork()}
	}},
	{route: "/worker/v1/agent-computers/checkpoint/register", setup: sealTenancyCapture, held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentCheckpointPublication{EnvironmentID: v.f.Environment.String(), Manifest: v.checkpointManifest()}
	}},
	{route: "/worker/v1/agent-computers/checkpoint/complete", setup: sealTenancyCapture, held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentCheckpointPublication{EnvironmentID: v.f.Environment.String(), Manifest: v.checkpointManifest()}
	}},
	// The restore target is the allocation entitled to read the ready image. The
	// victim's read also needs the source's physical stop and a completed
	// checkpoint, which this fixture does not reach; its not-ready answer
	// still differs from the refusal.
	{route: "/worker/v1/agent-computers/checkpoint/read", crossHost: true, setup: tenancyRestoreStage(0), held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: v.f.Environment.String(), OwnerID: v.f.Computer.String(), Epoch: 2, AllocationID: v.restoreTarget.String()}
	}},
	{route: "/worker/v1/agent-computers/capture/begin", held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.capture }},
	{route: "/worker/v1/agent-computers/capture/seal", held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any {
		return workerapi.AgentComputerReceiptRequest{EnvironmentID: v.capture.EnvironmentID, CheckpointID: v.capture.CheckpointID, Receipt: v.receipt(ids)}
	}},
	{route: "/worker/v1/agent-computers/capture/cancel", held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentComputerUnsealedRequest{EnvironmentID: v.capture.EnvironmentID, CheckpointID: v.capture.CheckpointID, Rejected: true}
	}},
	{route: "/worker/v1/agent-computers/capture/save-absence", held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentComputerSaveAbsenceRequest{EnvironmentID: v.capture.EnvironmentID, CheckpointID: v.capture.CheckpointID, Evidence: "owned disk pipeline joined without a cut"}
	}},
	{route: "/worker/v1/agent-computers/restore/prepare", crossHost: true, setup: tenancyRestoreStage(0), victimStatus: http.StatusOK, held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentComputerRestoreRequest{EnvironmentID: v.capture.EnvironmentID, CheckpointID: v.capture.CheckpointID, LeaseEpoch: 2, ChannelCredential: tenancyRestoreCredential}
	}},
	{route: "/worker/v1/agent-computers/controls", crossHost: true, setup: tenancyRestoreStage(3), victimStatus: http.StatusOK, held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any { return v.restoreInstallation(ids) }},
	{route: "/worker/v1/agent-computers/source-abort/prepare", held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any {
		return workerapi.AgentComputerSourceAbortRequest{EnvironmentID: v.capture.EnvironmentID, CheckpointID: v.capture.CheckpointID, LeaseEpoch: 1, ChannelCredential: sessiontest.ChannelCredential, Receipt: v.receipt(ids)}
	}},
	{route: "/worker/v1/agent-computers/source-abort/validate", held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any { return v.installation(ids, true) }},
	{route: "/worker/v1/agent-computers/source-abort/commit", held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any { return v.installation(ids, true) }},
	{route: "/worker/v1/agent-computers/source-abort/complete", held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any { return v.installation(ids, true) }},
	{route: "/worker/v1/agent-computers/restore/validate", crossHost: true, victimStatus: http.StatusNoContent, setup: tenancyRestoreStage(1), held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any { return v.restoreInstallation(ids) }},
	{route: "/worker/v1/agent-computers/restore/commit", crossHost: true, victimStatus: http.StatusNoContent, setup: tenancyRestoreStage(2), held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any { return v.restoreInstallation(ids) }},
	{route: "/worker/v1/agent-computers/restore/complete", crossHost: true, victimStatus: http.StatusNoContent, setup: tenancyRestoreStage(4), held: "checkpoint", body: func(v *tenancyVictim, ids func([]byte) []byte) any { return v.restoreInstallation(ids) }},
	{route: "/worker/v1/agent-computers/lease/renew", held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentComputerLeaseRequest{Network: tenancyNetwork(), EnvironmentID: v.f.Environment.String(), ComputerID: v.f.Computer.String(), AllocationID: v.f.Computer.String(), LeaseEpoch: 1}
	}},
	{route: "/worker/v1/agent-computers/stopped", held: "lease", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: workerapi.AgentComputerLeaseRequest{EnvironmentID: v.f.Environment.String(), ComputerID: v.f.Computer.String(), AllocationID: v.f.Computer.String(), LeaseEpoch: 1}}
	}},
	{route: "/worker/v1/sessions/authority", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.session() }},
	{route: "/worker/v1/sessions/attachment", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.session() }},
	{route: "/worker/v1/sessions/control", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentControlRequest{Session: v.session(), AttachmentSequence: v.attachment}
	}},
	{route: "/worker/v1/sessions/control/receipt", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentControlReceipt{Session: v.session(), AttachmentSequence: v.attachment, Sequence: 1, AuthorityGeneration: 1, Kind: "resume"}
	}},
	{route: "/worker/v1/sessions/ready", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentControlRequest{Session: v.session(), AttachmentSequence: v.attachment}
	}},
	{route: "/worker/v1/sessions/turn", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentTurnRequest{Session: v.session(), AttachmentSequence: v.attachment, AuthorityGeneration: 1}
	}},
	{route: "/worker/v1/sessions/turn/receipt", setup: cancelTenancyTurn, held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentTurnReceipt{Session: v.session(), AttachmentSequence: v.attachment, TurnID: v.turnID, Outcome: json.RawMessage(`{"status":"cancelled"}`)}
	}},
	{route: "/worker/v1/sessions/message", setup: claimTenancyTurn, held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentMessageRequest{Session: v.session(), AttachmentSequence: v.attachment, AuthorityGeneration: 1, TurnID: v.turnID}
	}},
	{route: "/worker/v1/sessions/message/receipt", setup: claimTenancyMessage, held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentMessageReceipt{Session: v.session(), AttachmentSequence: v.attachment, TurnID: v.turnID, MessageID: v.messageID}
	}},
	{route: "/worker/v1/sessions/start", setup: startingTenancySession, victimStatus: http.StatusOK, held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentControlRequest{Session: v.session(), AttachmentSequence: v.attachment}
	}},
	{route: "/worker/v1/sessions/start/release", setup: startedTenancySession, held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentStartReleaseRequest{Session: v.session(), AttachmentSequence: v.attachment, BundleDigest: v.bundleDigest()}
	}},
	{route: "/worker/v1/sessions/stopped", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentControlRequest{Session: v.session(), AttachmentSequence: v.attachment}
	}},
	{route: "/worker/v1/sessions/failed", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentControlRequest{Session: v.session(), AttachmentSequence: v.attachment}
	}},
	// Hold is the operation that needs no originating Turn, so it reaches the
	// Session authority check rather than argument validation.
	{route: "/worker/v1/sessions/operations", held: "session", inBand: true, body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentOperationRequest{Session: v.session(), RequestID: "tenancy-hold", AuthorityGeneration: 1, Method: workerapi.AgentMethodHold, Payload: json.RawMessage(`{"reason":"runtime hold"}`)}
	}},
	// Session MCP admission authorizes the calling process before any tool.
	{route: "/worker/v1/sessions/operations", name: "mcp list_agents", victimStatus: http.StatusOK, held: "session", inBand: true, body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentOperationRequest{Session: v.session(), RequestID: "tenancy-mcp", AuthorityGeneration: 1, Method: workerapi.AgentMethodRuntimeMCP, MCPScopeID: v.f.MCPScope.String(), Payload: json.RawMessage(`{"tool":"list_agents","arguments":{}}`)}
	}},
	{route: "/worker/v1/sessions/operations", name: "mcp enqueue", victimStatus: http.StatusOK, held: "session", inBand: true, body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		payload, _ := json.Marshal(map[string]any{"tool": "enqueue", "arguments": map[string]any{"sessionId": v.f.Session.String(), "input": json.RawMessage(`[{"type":"text","text":"hello"}]`), "idempotencyKey": "tenancy-mcp-enqueue"}})
		return workerapi.AgentOperationRequest{Session: v.session(), RequestID: "tenancy-mcp", AuthorityGeneration: 1, Method: workerapi.AgentMethodRuntimeMCP, MCPScopeID: v.f.MCPScope.String(), Payload: payload}
	}},
	// Output and finalization publish to the Session's Slack thread, so they
	// also reach its channel binding; the Turn is claimed by the victim host.
	{route: "/worker/v1/sessions/operations", name: "slack output", setup: slackTenancyTurn, victimStatus: http.StatusOK, held: "session", inBand: true, body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		payload, _ := json.Marshal(map[string]any{"outputId": v.outputID, "value": "progress"})
		return workerapi.AgentOperationRequest{Session: v.session(), RequestID: "tenancy-output", AuthorityGeneration: 1, TurnID: v.turnID, Method: workerapi.AgentMethodOutput, Payload: payload}
	}},
	{route: "/worker/v1/sessions/operations", name: "slack finalize", setup: slackTenancyTurn, held: "session", inBand: true, body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentOperationRequest{Session: v.session(), RequestID: "tenancy-finalize", AuthorityGeneration: 1, TurnID: v.turnID, Method: workerapi.AgentMethodFinalize, Payload: json.RawMessage(`{"result":{"saved":true}}`), DrainEvidence: "native scopes joined"}
	}},
	{route: "/worker/v1/sessions/logs", held: "session", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.SessionLogRequest{Session: v.session(), AttachmentSequence: v.attachment, Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: v.observedAt.UnixNano(), Data: []byte("output")}
	}},
	// The running image build holds proxy trust and an exposed protected Secret,
	// so the victim's own preparation and resolution succeed.
	{route: "/worker/v1/run/secret-proxy/prepare", held: "image_build", victimStatus: http.StatusOK, body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.SecretProxyRequest{AllocationID: v.imageBuild.Identity.AllocationID}
	}},
	{route: "/worker/v1/run/secret-proxy/resolve", held: "image_build", victimStatus: http.StatusOK, body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.SecretProxyRequest{AllocationID: v.imageBuild.Identity.AllocationID, Origin: tenancyProxyOrigin, Placeholders: []string{v.proxyMarker}}
	}},
	{route: "/worker/v1/computer-commands/claim", held: "claimed_command", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.ComputerCommandClaimRequest{ActiveCommandIDs: []string{v.command.String()}, EnvironmentID: v.f.Environment.String(), AllocationID: v.f.Computer.String(), WriterGeneration: 1}
	}},
	{route: "/worker/v1/computer-commands/reconcile", beforeHostLock: true, setup: completeTenancyCommand, victimStatus: http.StatusOK, held: "claimed_command", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.commandCompletion() }},
	{route: "/worker/v1/computer-commands/complete", beforeHostLock: true, setup: appendTenancyCommandLogs, held: "claimed_command", body: func(v *tenancyVictim, _ func([]byte) []byte) any { return v.commandCompletion() }},
	{route: "/worker/v1/computer-commands/logs/append", beforeHostLock: true, held: "claimed_command", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.CommandLogAppendRequest{EnvironmentID: v.f.Environment.String(), CommandID: v.command.String(), AllocationID: v.f.Computer.String(), WriterGeneration: 1, Stream: workerapi.LogStreamStdout, Kind: "data", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: v.observedAt, Content: []byte("output")}
	}},
}

func (v *tenancyVictim) commandCompletion() workerapi.ComputerCommandCompleteRequest {
	code := int32(0)
	return workerapi.ComputerCommandCompleteRequest{EnvironmentID: v.f.Environment.String(), CommandID: v.command.String(), AllocationID: v.f.Computer.String(), WriterGeneration: 1, Outcome: "exited", ExitCode: &code, Stdout: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}, Stderr: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}}
}

// heldTenancyOwner proves that the victim row a case names exists and is held
// by the victim host, so the foreign request addresses a real resource.
var heldTenancyOwner = map[string]string{
	"lease":              `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1 AND allocation_id=$2 AND worker_host_id=$3 AND status='active')`,
	"session":            `SELECT EXISTS(SELECT 1 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.session_id=$4 AND p.epoch=1 AND l.worker_host_id=$3)`,
	"image_build":        `SELECT EXISTS(SELECT 1 FROM image_builds WHERE environment_id=$1 AND id=$5 AND worker_host_id=$3 AND status='running')`,
	"eligible_boot_seed": `SELECT EXISTS(SELECT 1 FROM boot_seed_builds WHERE environment_id=$1 AND id=$6 AND status='succeeded' AND manifest IS NOT NULL)`,
	"boot_seed":          `SELECT EXISTS(SELECT 1 FROM boot_seed_builds WHERE environment_id=$1 AND id=$6 AND worker_host_id=$3 AND status='running')`,
	"claimed_command":    `SELECT EXISTS(SELECT 1 FROM commands WHERE environment_id=$1 AND id=$7 AND computer_id=$2 AND computer_lease_epoch=1)`,
	"checkpoint":         `SELECT EXISTS(SELECT 1 FROM checkpoints WHERE environment_id=$1 AND id=$8 AND computer_id=$2)`,
	"save":               `SELECT EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND id=$9)`,
}

const tenancyProxyOrigin = "https://api.example.com"

// newTenancySessionVictim is organization A with only the sessiontest
// fixture: a Session process on the victim host's active lease.
func newTenancySessionVictim(t *testing.T) *tenancyVictim {
	t.Helper()
	v := &tenancyVictim{f: sessiontest.New(t), observedAt: time.Now().UTC().Truncate(time.Millisecond), leaseEpoch: 1, currentEpoch: 1, attachment: sessiontest.SeededAttachment, turnID: uuid.NewV7().String(), messageID: uuid.NewV7().String(), outputID: uuid.NewV7().String()}
	v.restoreHost = v.f.Worker
	// The deployment's executable bundle is readable.
	v.bundleRaw, v.bundle = controlPlaneDeploymentBundle(t)
	digest, err := bundle.Digest(v.bundleRaw)
	if err != nil {
		t.Fatal(err)
	}
	v.cas = newTestUploadStore(t)
	if stored, err := v.cas.Put(t.Context(), bundle.MediaType, bytes.NewReader(v.bundleRaw)); err != nil || stored.Digest != digest {
		t.Fatalf("bundle store: %v %v", stored, err)
	}
	dbtest.MustExec(t, t.Context(), v.f.Pool, `UPDATE deployments SET bundle_digest=$3 WHERE environment_id=$1 AND id=$2`, v.f.Environment, v.f.Deployment, digest)
	return v
}

func (v *tenancyVictim) bundleDigest() string {
	digest, err := bundle.Digest(v.bundleRaw)
	if err != nil {
		panic(err)
	}
	return digest
}

// tenancyChannel is the channel credential the test allocator (a zero key)
// derives for an executor, so delivery of the fixture rows succeeds.
func tenancyChannel(domain string, env, owner, instance, host uuid.UUID) []byte {
	mac := hmac.New(sha256.New, make([]byte, 32))
	_, _ = mac.Write([]byte(domain))
	for _, id := range []uuid.UUID{env, owner, instance, host} {
		_, _ = mac.Write(id[:])
	}
	var epochs [16]byte
	binary.BigEndian.PutUint64(epochs[:8], 1)
	binary.BigEndian.PutUint64(epochs[8:], 1)
	_, _ = mac.Write(epochs[:])
	return mac.Sum(nil)
}

// newTenancyVictim builds every owner the routes address. With claimCommand
// the Command is claimed by the victim host, and no capture is begun because a
// running Command blocks it.
func newTenancyVictim(t *testing.T, claimCommand bool) *tenancyVictim {
	t.Helper()
	v := newTenancySessionVictim(t)
	f := v.f
	v.bootIDs = [2]string{uuid.NewV7().String(), uuid.NewV7().String()}
	ctx := t.Context()
	var org uuid.UUID
	if err := f.Pool.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
		t.Fatal(err)
	}
	// The fixture disk keys are wrapped by the servers' key wrapper.
	var err error
	if v.keys, err = computerkey.NewLocal("tenancy", bytes.Repeat([]byte{9}, 32)); err != nil {
		t.Fatal(err)
	}
	scope, err := computerkey.EncryptionScope(org.String(), f.Environment.String())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.Pool.Query(ctx, `SELECT id FROM disk_encryption_keys WHERE environment_id=$1 AND wrapped_key IS NOT NULL`, f.Environment)
	if err != nil {
		t.Fatal(err)
	}
	keyIDs, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range keyIDs {
		// sessiontest encrypts its initial root with this fixture key.
		envelope, err := v.keys.Wrap(ctx, scope, id.String(), bytes.Repeat([]byte{7}, 32))
		if err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, ctx, f.Pool, `UPDATE disk_encryption_keys SET wrapping_key_id=$2,wrapped_key=$3 WHERE id=$1`, id, envelope.WrappingKeyID, envelope.Ciphertext)
	}
	if v.secrets, err = secret.New(db.New(f.Pool), f.Pool, bytes.Repeat([]byte{17}, 32), auditpolicy.Policy{RetentionSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	// Running authored preparation held by the victim host, on its own image
	// spec so its protected Secret binding leaves the published image usable.
	spec := uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO image_specs(environment_id,id,spec_digest,spec,seed) VALUES($1,$2,$3,'{}','{}')`, f.Environment, spec, "sha256:"+strings.Repeat("2", 64))
	imageOwner, imageInstance := uuid.NewV7(), uuid.NewV7()
	v.imageBuild = workerapi.PreparationExecutor{Identity: workerapi.AllocationIdentity{Kind: "preparation", EnvironmentID: f.Environment.String(), OwnerID: imageOwner.String(), AllocationID: imageInstance.String(), Epoch: 1}, ChannelCredential: tenancyChannel("helmr.preparation-channel.v1\x00", f.Environment, imageOwner, imageInstance, f.Worker)}
	imageDigest := sha256.Sum256(v.imageBuild.ChannelCredential)
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,status,deadline_at,executor_epoch,worker_host_id,worker_epoch,allocation_id,channel_credential_digest,executor_expires_at,delivered_at,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
 SELECT $1,$2,$3,'tenancy','running',clock_timestamp()+interval '10 minutes',1,id,1,$4,$5,clock_timestamp()+interval '10 minutes',clock_timestamp(),536870912,1073741824,vm_platform_id,1,cpu_environment_digest FROM worker_hosts WHERE id=$6`, f.Environment, v.imageBuild.Identity.OwnerID, spec, v.imageBuild.Identity.AllocationID, imageDigest[:], f.Worker)
	// Its Secret proxy holds trust for one protected Secret.
	record, err := v.secrets.Create(ctx, dbtest.SecretManager(t, f.Pool, f.Environment), f.Environment, "BUILD_TOKEN", []byte("protected-value"), "create-protected")
	if err != nil {
		t.Fatal(err)
	}
	if v.proxyMarker, err = secretbinding.Placeholder("protected"); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO secret_bindings(environment_id,image_spec_id,secret_id,placement_kind,placement_target,mode,allowed_origins,placeholder) VALUES($1,$2,$3,'env','TOKEN','protected',ARRAY[$4],$5)`, f.Environment, spec, pgvalue.MustUUIDValue(record.ID), tenancyProxyOrigin, v.proxyMarker)
	var now, deadline time.Time
	if err := f.Pool.QueryRow(ctx, `SELECT clock_timestamp(),deadline_at FROM image_builds WHERE environment_id=$1 AND id=$2`, f.Environment, v.imageBuild.Identity.OwnerID).Scan(&now, &deadline); err != nil {
		t.Fatal(err)
	}
	trust, err := v.secrets.GeneratePreparationProxyTrust(f.Environment, uuid.MustParse(v.imageBuild.Identity.OwnerID), now, deadline)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE image_builds SET proxy_ca_certificate=$3,proxy_ca_private_key_nonce=$4,proxy_ca_private_key_ciphertext=$5,proxy_ca_not_after=$6 WHERE environment_id=$1 AND id=$2`, f.Environment, v.imageBuild.Identity.OwnerID, trust.Certificate, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext, trust.NotAfter)
	// A succeeded image publication and its running boot seed derivation, held
	// by the victim host.
	seedOwner, seedInstance := uuid.NewV7(), uuid.NewV7()
	v.bootSeed = workerapi.BootSeedExecutor{Identity: workerapi.AllocationIdentity{Kind: "seed", EnvironmentID: f.Environment.String(), OwnerID: seedOwner.String(), AllocationID: seedInstance.String(), Epoch: 1}, ChannelCredential: tenancyChannel("helmr.boot-seed-build.v1\x00", f.Environment, seedOwner, seedInstance, f.Worker)}
	seedDigest := sha256.Sum256(v.bootSeed.ChannelCredential)
	published, writeKey := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, ctx, f.Pool, `
 INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,status,error_code,deadline_at) VALUES($1,$2,$3,'tenancy-published','failed','fixture_pending',clock_timestamp()+interval '10 minutes');
 INSERT INTO disk_encryption_keys(id,environment_id,writer_image_build_id,wrapping_key_id,wrapped_key) VALUES($9,$1,$2,'test',decode('01','hex'));
 UPDATE image_builds b SET status='succeeded',error_code=NULL,logical_bytes=1048576,capture_root='sha256:'||encode(c.initial_root_digest,'hex'),capture_evidence='fixture capture',write_key_id=$9 FROM computers c WHERE b.environment_id=$1 AND b.id=$2 AND c.environment_id=$1 AND c.id=$4;
 INSERT INTO images(environment_id,id,image_spec_id,image_build_id,seq,root_id,publication_evidence) SELECT environment_id,$2,$3,$2,1,initial_root_id,'fixture' FROM computers WHERE environment_id=$1 AND id=$4;
 INSERT INTO boot_seed_builds(environment_id,id,deployment_id,image_spec_id,image_id,cpu_millis,memory_bytes,status,deadline_at,worker_host_id,worker_epoch,allocation_id,resource_policy_revision,executor_epoch,channel_credential_digest,delivered_at,executor_expires_at,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
 SELECT $1,$5::uuid,$3,$3,$2,1000,536870912,'running',clock_timestamp()+interval '10 minutes',id,1,$6::uuid,1,1,decode($7,'hex'),clock_timestamp(),clock_timestamp()+interval '10 minutes',536870912,1073741824,vm_platform_id,1,cpu_environment_digest FROM worker_hosts WHERE id=$8;
 `, pgx.QueryExecModeSimpleProtocol, f.Environment, published, f.Deployment, f.Computer, v.bootSeed.Identity.OwnerID, v.bootSeed.Identity.AllocationID, hexBytes(seedDigest[:]), f.Worker, writeKey)
	server := httptest.NewServer(newPostgresServer(t, f.Pool, v.configure))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	// The protected Secret is exposed to the running preparation.
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE worker_hosts SET observed_at=clock_timestamp() WHERE id=$1`, f.Worker)
	if _, err := client.PreparationSecrets(ctx, v.imageBuild); err != nil {
		t.Fatalf("expose protected Secret: %v", err)
	}
	if claimCommand {
		v.command, _, _ = runningComputerCommand(t, f)
	} else {
		// A begun capture provides the checkpoint, its save and the session
		// capture. A Command would block its seal, so these victims have none.
		request, result, captured := beginWorkerAgentCapture(t, f, client)
		v.capture, v.saveID, v.captured, v.captureRaw = request, result.SaveID, captured, result.Capture
	}
	var rawInspection []byte
	var rootDigest []byte
	if err := f.Pool.QueryRow(ctx, `SELECT `+computer.DiskRootColumns+`,o.inspection::text,c.initial_root_digest FROM computers c JOIN disk_roots r ON (r.environment_id,r.id)=(c.environment_id,c.initial_root_id)
 JOIN disk_objects o ON (o.environment_id,o.digest)=(r.environment_id,r.pack_digest) WHERE c.environment_id=$1 AND c.id=$2`, f.Environment, f.Computer).Scan(append(computer.DiskRootFields(&v.root), &rawInspection, &rootDigest)...); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawInspection, &v.inspection); err != nil {
		t.Fatal(err)
	}
	v.baseDigest = "sha256:" + hexBytes(rootDigest)
	return v
}

// qualifyTenancyBootSeed publishes the victim's boot seed build for the
// deployment's Program and acknowledges current resource policy on every
// victim-group host, making it an eligible preparation source in region test.
// offerTenancyBootSeed has the running seed allocation create its data key and
// qualifies the seed, so its preparation source may receive that key.
func offerTenancyBootSeed(t *testing.T, v *tenancyVictim) {
	t.Helper()
	v.post(t, "/worker/v1/boot-seeds/key", workerapi.BootSeedKeyRequest{Scope: workerapi.StorageScope{BootSeed: &v.bootSeed}, BuildID: v.bootSeed.Identity.OwnerID}, nil)
	qualifyTenancyBootSeed(t, v)
}

// selectTenancyBootSeed offers the victim's boot seed and selects it for the
// victim lease, which may then receive its data key.
func selectTenancyBootSeed(t *testing.T, v *tenancyVictim) {
	t.Helper()
	offerTenancyBootSeed(t, v)
	dbtest.MustExec(t, t.Context(), v.f.Pool, `UPDATE computer_leases SET boot_seed_build_id=$3 WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, v.f.Environment, v.f.Computer, uuid.MustParse(v.bootSeed.Identity.OwnerID))
}

func qualifyTenancyBootSeed(t *testing.T, v *tenancyVictim) {
	t.Helper()
	program := computercheckpoint.Program{
		Runtime:  computercheckpoint.Object{Digest: v.bundle.Runtime.Artifact.Digest, SizeBytes: v.bundle.Runtime.Artifact.SizeBytes, MediaType: v.bundle.Runtime.Artifact.MediaType},
		Artifact: computercheckpoint.Object{Digest: v.bundle.Program.Artifact.Digest, SizeBytes: v.bundle.Program.Artifact.SizeBytes, MediaType: v.bundle.Program.Artifact.MediaType}}
	publication := v.bootSeedPublicationFor(program)
	manifest, err := json.Marshal(publication.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := json.Marshal(publication.Qualification)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), v.f.Pool, `
 UPDATE boot_seed_builds SET status='succeeded',manifest=$3,qualification_evidence=$4 WHERE environment_id=$1 AND id=$2;
 UPDATE worker_pools SET optional_cpu_millis=capacity_cpu_millis,optional_memory_bytes=capacity_memory_bytes,optional_vm_slots=max_vm_slots,optional_max_tasks=max_vm_slots,optional_disk_bytes=1073741824,optional_io_bytes_per_second=1048576 WHERE worker_group_id=$5;
 UPDATE worker_hosts h SET resource_policy_revision=p.resource_policy_revision,resource_observed_at=clock_timestamp() FROM worker_pools p WHERE p.id=h.worker_pool_id AND h.worker_group_id=$5;
 `, pgx.QueryExecModeSimpleProtocol, v.f.Environment, v.bootSeed.Identity.OwnerID, manifest, string(evidence), v.f.Group)
}

func hexBytes(raw []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(raw)*2)
	for _, b := range raw {
		out = append(out, digits[b>>4], digits[b&15])
	}
	return string(out)
}

// otherGroupIntruder is an active host of another worker group in another
// region. At most one active group exists per region.
func otherGroupIntruder(t *testing.T, v *tenancyVictim, handler http.Handler) tenancyIntruder {
	t.Helper()
	in := tenancyIntruder{name: "other group", groupID: uuid.NewV7(), hostID: uuid.NewV7()}
	token, pool := uuid.NewV7(), uuid.NewV7()
	tokenHash := sha256.Sum256([]byte(token.String()))
	dbtest.MustExec(t, t.Context(), v.f.Pool, `
 INSERT INTO regions(id,display_name) VALUES('tenancy-other','Tenancy other');
 INSERT INTO worker_group_tokens(id,token_hash) VALUES($1,$2);
 INSERT INTO worker_groups SELECT (jsonb_populate_record(NULL::worker_groups,to_jsonb(g)||jsonb_build_object('id',$3::uuid,'token_id',$1::uuid,'region_id','tenancy-other','name','tenancy-other','primary_pool_id',NULL,'capacity_observed_at',NULL,'capacity_supply_fingerprint',NULL,'capacity_pools',NULL))).* FROM worker_groups g WHERE id=$4;
 INSERT INTO worker_pools SELECT (jsonb_populate_record(NULL::worker_pools,to_jsonb(p)||jsonb_build_object('id',$5::uuid,'worker_group_id',$3::uuid))).* FROM worker_pools p WHERE id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$6);
 INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) SELECT $5,vcpu_count,cpu_config_digest FROM worker_pool_cpu_shapes WHERE worker_pool_id=(SELECT worker_pool_id FROM worker_hosts WHERE id=$6);
 INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$7::uuid,'resource_id',$7::text,'current_service_id',$7::uuid,'worker_group_id',$3::uuid,'worker_pool_id',$5::uuid))).* FROM worker_hosts h WHERE id=$6;
 `, pgx.QueryExecModeSimpleProtocol, token, tokenHash[:], in.groupID, v.f.Group, pool, v.f.Worker, in.hostID)
	in.credential = seedHostSecret(t, v.f.Pool, in.hostID).issue(t, handler)
	return in
}

// sameGroupIntruder is another active host of the victim's own group.
func sameGroupIntruder(t *testing.T, v *tenancyVictim, handler http.Handler) tenancyIntruder {
	t.Helper()
	in := tenancyIntruder{name: "same group", groupID: v.f.Group, hostID: uuid.NewV7(), sameGroup: true}
	dbtest.MustExec(t, t.Context(), v.f.Pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id',$2::text,'current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, v.f.Worker, in.hostID)
	in.credential = seedHostSecret(t, v.f.Pool, in.hostID).issue(t, handler)
	return in
}

var tenancyUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// tenancyRewriter maps every identifier except the preserved host IDs to a
// fresh, never-inserted one. The mapping is stable within a test, and the
// equal-length replacement keeps protobuf length prefixes valid.
type tenancyRewriter struct {
	preserve map[string]bool
	mapped   map[string]string
}

func (r *tenancyRewriter) rewrite(raw []byte) []byte {
	return tenancyUUID.ReplaceAllFunc(raw, func(id []byte) []byte {
		if r.preserve[string(id)] {
			return id
		}
		fresh, ok := r.mapped[string(id)]
		if !ok {
			fresh = uuid.NewV7().String()
			r.mapped[string(id)] = fresh
		}
		return []byte(fresh)
	})
}

// tenancyDurableState snapshots every environment-scoped row of the victim.
func tenancyDurableState(t *testing.T, v *tenancyVictim) string {
	t.Helper()
	var tables []string
	rows, err := v.f.Pool.Query(t.Context(), `SELECT DISTINCT c.table_name FROM information_schema.columns c JOIN information_schema.tables tb USING (table_schema,table_name)
 WHERE c.column_name='environment_id' AND c.table_schema=current_schema() AND tb.table_type='BASE TABLE' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{`COALESCE((SELECT to_jsonb(e)::text FROM environments e WHERE id=$1),'')`}
	for _, table := range tables {
		parts = append(parts, `'`+table+`:'||COALESCE((SELECT jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text)::text FROM "`+table+`" t WHERE environment_id=$1),'')`)
	}
	var state string
	if err := v.f.Pool.QueryRow(t.Context(), `SELECT `+strings.Join(parts, `||'|'||`), v.f.Environment).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func tenancyAuditCount(t *testing.T, v *tenancyVictim) int {
	t.Helper()
	var count int
	if err := v.f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

type tenancyResponse struct {
	status int
	body   []byte
	audits int
}

// tenancyDeadline bounds every request; a request blocked on a victim row lock
// fails instead of hanging the test.
const tenancyDeadline = 3 * time.Second

// sendTenancyRequest sends body from a host. With checkState, the request must
// leave organization A's durable state unchanged.
func sendTenancyRequest(t *testing.T, v *tenancyVictim, handler http.Handler, in tenancyIntruder, route string, body []byte, checkState bool) tenancyResponse {
	t.Helper()
	var before string
	if checkState {
		before = tenancyDurableState(t, v)
	}
	audits := tenancyAuditCount(t, v)
	ctx, cancel := context.WithTimeout(context.Background(), tenancyDeadline)
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, route, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+in.credential)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		handler.ServeHTTP(response, request)
	}()
	select {
	case <-done:
	case <-time.After(tenancyDeadline + 2*time.Second):
		t.Fatalf("%s from %s did not return within its deadline", route, in.name)
	}
	if elapsed := time.Since(started); elapsed >= tenancyDeadline {
		t.Fatalf("%s from %s blocked for %s: %d %s", route, in.name, elapsed, response.Code, response.Body.String())
	}
	if checkState {
		if after := tenancyDurableState(t, v); after != before {
			t.Fatalf("%s from %s changed organization A durable state: %d %s", route, in.name, response.Code, response.Body.String())
		}
	}
	return tenancyResponse{status: response.Code, body: response.Body.Bytes(), audits: tenancyAuditCount(t, v) - audits}
}

// tenancyLockedTables are the owners a /worker/v1 route may lock first.
var tenancyLockedTables = []string{"environments", "computers", "computer_leases", "image_builds", "boot_seed_builds", "checkpoints", "computer_saves", "sessions", "session_processes", "commands",
	"slack_connections", "slack_channel_bindings", "slack_threads", "slack_thread_sources"}

// tenancyLockedOrgTables are organization-scoped Slack owners a Session
// publication reaches through its channel binding.
var tenancyLockedOrgTables = []string{"slack_app_registrations", "slack_installations"}

// lockTenancyVictim holds FOR UPDATE on every victim owner row until the
// returned release runs. A foreign request must not wait for any of them.
func lockTenancyVictim(t *testing.T, v *tenancyVictim) func() {
	t.Helper()
	tx, err := v.f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tenancyLockedTables {
		column := "environment_id"
		if table == "environments" {
			column = "id"
		}
		if _, err := tx.Exec(t.Context(), `SELECT 1 FROM `+table+` WHERE `+column+`=$1 FOR UPDATE`, v.f.Environment); err != nil {
			_ = tx.Rollback(context.Background())
			t.Fatal(err)
		}
	}
	for _, table := range tenancyLockedOrgTables {
		if _, err := tx.Exec(t.Context(), `SELECT 1 FROM `+table+` WHERE org_id=(SELECT org_id FROM environments WHERE id=$1) FOR UPDATE`, v.f.Environment); err != nil {
			_ = tx.Rollback(context.Background())
			t.Fatal(err)
		}
	}
	return func() { _ = tx.Rollback(context.Background()) }
}

// lockTenancyIntruderHost holds FOR UPDATE on the intruder's own worker group
// and host rows until the returned release runs. A foreign identifier must not
// reach a lock of the caller's own rows that a missing one skips.
func lockTenancyIntruderHost(t *testing.T, v *tenancyVictim, in tenancyIntruder) func() {
	t.Helper()
	tx, err := v.f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM worker_groups WHERE id=$1 FOR UPDATE`, in.groupID); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM worker_hosts WHERE id=$1 FOR UPDATE`, in.hostID); err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback(context.Background()) }
}

// compareTenancy asserts that the foreign request reached authorization, was
// refused and reads exactly like the missing request.
func compareTenancy(t *testing.T, c workerRouteTenancyCase, got, want tenancyResponse) {
	t.Helper()
	switch got.status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusRequestEntityTooLarge, http.StatusTooManyRequests:
		t.Fatalf("%s: request did not reach authorization: %d %s", t.Name(), got.status, got.body)
	}
	if bytes.Contains(got.body, []byte(`"network_policy_unsupported"`)) {
		t.Fatalf("request stopped at network capability admission: %s", got.body)
	}
	refused := got.status >= 300 && got.status < 500
	if c.inBand {
		var operation workerapi.AgentOperationResponse
		refused = got.status == http.StatusOK && json.Unmarshal(got.body, &operation) == nil && operation.Error != nil
	}
	if !refused {
		t.Fatalf("%s: foreign request was not refused: %d %s", t.Name(), got.status, got.body)
	}
	if got.status != want.status || !bytes.Equal(got.body, want.body) || got.audits != want.audits {
		t.Fatalf("%s: foreign differs from missing:\nforeign %d audits=%d %s\nmissing %d audits=%d %s", t.Name(), got.status, got.audits, got.body, want.status, want.audits, want.body)
	}
}

// runTenancyCase compares each intruder's foreign request with a missing one,
// idle and while the victim rows are locked, then sends the victim host's
// own request as a positive control. v is this case's own victim.
func runTenancyCase(t *testing.T, v *tenancyVictim, c workerRouteTenancyCase, intruders []tenancyIntruder, victim tenancyIntruder) {
	t.Helper()
	var held bool
	if err := v.f.Pool.QueryRow(t.Context(), heldTenancyOwner[c.held]+` AND num_nonnulls($1::uuid,$2::uuid,$3::uuid,$4::uuid,$5::uuid,$6::uuid,$7::uuid,$8::uuid,$9::uuid)=9`, v.f.Environment, v.f.Computer, v.f.Worker, v.f.Session, tenancyIDOrNil(v.imageBuild.Identity.OwnerID), tenancyIDOrNil(v.bootSeed.Identity.OwnerID), v.command, tenancyIDOrNil(v.capture.CheckpointID), tenancyIDOrNil(v.saveID)).Scan(&held); err != nil || !held {
		t.Fatalf("victim %s is not held by the victim host: %v", c.held, err)
	}
	foreign, err := json.Marshal(c.body(v, func(raw []byte) []byte { return raw }))
	if err != nil {
		t.Fatal(err)
	}
	// The first missing request replaces every identifier. The second keeps the
	// context an intruder can know (the Environment, and the Computer when it is
	// not itself the target) and replaces only the targeted resource IDs.
	known := []string{v.f.Environment.String()}
	switch c.held {
	case "session", "checkpoint", "save", "claimed_command":
		known = append(known, v.f.Computer.String())
	}
	var missing [][]byte
	for _, preserve := range [][]string{nil, known} {
		rw := &tenancyRewriter{preserve: map[string]bool{v.f.Worker.String(): true}, mapped: map[string]string{}}
		for _, id := range preserve {
			rw.preserve[id] = true
		}
		body, err := json.Marshal(c.body(v, rw.rewrite))
		if err != nil {
			t.Fatal(err)
		}
		body = rw.rewrite(body)
		if bytes.Equal(foreign, body) || (preserve == nil && bytes.Contains(body, []byte(v.f.Environment.String()))) {
			t.Fatal("missing request still names the targeted victim identifiers")
		}
		missing = append(missing, body)
	}
	if c.held == "session" {
		// A missing process of the real Session: every identifier is the
		// victim's, but no process has this epoch.
		body := bytes.ReplaceAll(foreign, []byte(`"process_epoch":1`), []byte(`"process_epoch":7`))
		if bytes.Equal(body, foreign) {
			t.Fatal("session case names no process epoch")
		}
		missing = append(missing, body)
	}
	compareMissing := func(t *testing.T, handler http.Handler, in tenancyIntruder) tenancyResponse {
		t.Helper()
		got := sendTenancyRequest(t, v, handler, in, c.route, foreign, true)
		for _, body := range missing {
			compareTenancy(t, c, got, sendTenancyRequest(t, v, handler, in, c.route, body, true))
		}
		return got
	}
	// Refusals are admission-limited per host; a server per case keeps every
	// request below that budget.
	handler := newPostgresServer(t, v.f.Pool, v.configure)
	var refusals []tenancyResponse
	for _, in := range intruders {
		if in.sameGroup && c.sameGroupReader {
			t.Run(in.name+" reads by design", func(t *testing.T) {
				got := sendTenancyRequest(t, v, handler, in, c.route, foreign, false)
				if got.status != http.StatusOK {
					t.Fatalf("same-group reader was refused: %d %s", got.status, got.body)
				}
			})
			continue
		}
		t.Run(in.name, func(t *testing.T) {
			refusals = append(refusals, compareMissing(t, handler, in))
		})
		t.Run(in.name+" under contention", func(t *testing.T) {
			release := lockTenancyVictim(t, v)
			defer release()
			compareMissing(t, handler, in)
		})
		if c.beforeHostLock {
			t.Run(in.name+" under own host contention", func(t *testing.T) {
				release := lockTenancyIntruderHost(t, v, in)
				defer release()
				compareMissing(t, handler, in)
			})
		}
	}
	if victim.credential == "" {
		// The case documents why it has no victim positive control.
		return
	}
	// The victim's own request runs last because it may change the victim.
	t.Run("victim host", func(t *testing.T) {
		own := *v
		own.leaseEpoch = v.currentEpoch
		body, err := json.Marshal(c.body(&own, func(raw []byte) []byte { return raw }))
		if err != nil {
			t.Fatal(err)
		}
		got := sendTenancyRequest(t, v, handler, victim, c.route, body, false)
		if c.victimStatus != 0 && got.status != c.victimStatus {
			t.Fatalf("%s: victim host status = %d, want %d: %s", t.Name(), got.status, c.victimStatus, got.body)
		}
		for _, refusal := range refusals {
			// A not-ready answer may share the refusal body by design; it then
			// still records no refusal audit, which the intruder's request does.
			if got.status == refusal.status && bytes.Equal(got.body, refusal.body) && got.audits == refusal.audits {
				t.Fatalf("%s: victim host got the intruder refusal, so the case does not reach authorization: %d audits=%d/%d %s", t.Name(), got.status, got.audits, refusal.audits, got.body)
			}
		}
	})
}

func tenancyIDOrNil(id string) uuid.UUID {
	if id == "" {
		return uuid.Nil()
	}
	return uuid.MustParse(id)
}

func tenancyHosts(t *testing.T, v *tenancyVictim) ([]tenancyIntruder, tenancyIntruder) {
	t.Helper()
	setup := newPostgresServer(t, v.f.Pool, v.configure)
	intruders := []tenancyIntruder{otherGroupIntruder(t, v, setup), sameGroupIntruder(t, v, setup)}
	return intruders, v.victimHost(t, setup)
}

// victimHost issues the victim host's credential and keeps it for setups.
func (v *tenancyVictim) victimHost(t *testing.T, handler http.Handler) tenancyIntruder {
	t.Helper()
	return v.victimHostFor(t, handler, v.f.Worker)
}

func (v *tenancyVictim) victimHostFor(t *testing.T, handler http.Handler, host uuid.UUID) tenancyIntruder {
	t.Helper()
	v.host, v.handler = seedHostSecret(t, v.f.Pool, host), handler
	victim := tenancyIntruder{name: "victim host", groupID: v.f.Group, hostID: host}
	victim.credential = v.host.issue(t, handler)
	return victim
}

func tenancyCaseName(c workerRouteTenancyCase) string {
	if c.name == "" {
		return c.route
	}
	return c.route + "/" + c.name
}

// post sends a setup request from the victim host and decodes a 200 body.
func (v *tenancyVictim) post(t *testing.T, route string, body any, out any) {
	t.Helper()
	response := (workerHTTPClient{handler: v.handler, hostCredential: v.host.issue(t, v.handler)}).send(t, route, body)
	if response.Code != http.StatusOK && response.Code != http.StatusNoContent {
		t.Fatalf("%s: setup %s: %d %s", t.Name(), route, response.Code, response.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
}

// Every /worker/v1 identifier owned by another organization, or reachable only
// through another worker group or host, reads exactly like a missing one, also
// while the victim's rows are locked. Each case owns a fresh victim so the
// victim host's positive control cannot disturb another case.
func TestWorkerRouteTenancyForeignMatchesMissing(t *testing.T) {
	for _, c := range workerRouteTenancyCases {
		t.Run(tenancyCaseName(c), func(t *testing.T) {
			t.Parallel()
			// Session cases need only the Session fixture; a begun capture would
			// hold their process.
			v := newTenancySessionVictim(t)
			if c.held != "session" {
				v = newTenancyVictim(t, c.held == "claimed_command")
			}
			intruders, victim := tenancyHosts(t, v)
			if c.crossHost {
				// A replacement host restores the Computer. The source host's lease
				// is fenced, so it is an intruder naming the target epoch.
				source := victim
				source.name = "fenced source host"
				replacement := sameGroupIntruder(t, v, v.handler)
				v.restoreHost = replacement.hostID
				victim = v.victimHostFor(t, v.handler, replacement.hostID)
				intruders = append(intruders, source)
			}
			if c.setup != nil {
				c.setup(t, v)
			}
			runTenancyCase(t, v, c, intruders, victim)
		})
	}
}

// A host that held an earlier lease of the victim Computer has no authority
// over the Session process now running on the victim's current lease.
func TestWorkerRouteTenancyFormerHostSessions(t *testing.T) {
	for _, c := range workerRouteTenancyCases {
		if c.held != "session" {
			continue
		}
		for _, epoch := range []int64{1, 2} {
			t.Run(fmt.Sprintf("%s/names lease %d", tenancyCaseName(c), epoch), func(t *testing.T) {
				t.Parallel()
				v := newTenancySessionVictim(t)
				setup := newPostgresServer(t, v.f.Pool, v.configure)
				former := sameGroupIntruder(t, v, setup)
				former.name = "former host"
				moveTenancyLease(t, v, former.hostID)
				v.leaseEpoch, v.currentEpoch = epoch, 2
				victim := v.victimHost(t, setup)
				if c.setup != nil {
					c.setup(t, v)
				}
				runTenancyCase(t, v, c, []tenancyIntruder{former}, victim)
			})
		}
	}
}

// Every mounted /worker/v1 route either has a tenancy case or an explicit
// exemption, so a new identifier-taking route cannot skip the comparison.
func TestWorkerRouteTenancyCoversMountedRoutes(t *testing.T) {
	router := chi.NewRouter()
	(&Server{}).mountWorkerRoutes(router)
	covered, names := map[string]bool{}, map[string]bool{}
	for _, c := range workerRouteTenancyCases {
		if names[tenancyCaseName(c)] {
			t.Errorf("duplicate tenancy case %s", tenancyCaseName(c))
		}
		names[tenancyCaseName(c)] = true
		covered["POST "+c.route] = true
		if _, ok := heldTenancyOwner[c.held]; !ok {
			t.Errorf("%s names unknown owner %q", c.route, c.held)
		}
	}
	var mounted []string
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		mounted = append(mounted, method+" "+route)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(mounted)
	seen := map[string]bool{}
	for _, route := range mounted {
		seen[route] = true
		_, exempt := workerRouteTenancyExempt[route]
		switch {
		case covered[route] && exempt:
			t.Errorf("%s is both covered and exempt", route)
		case !covered[route] && !exempt:
			t.Errorf("%s has no tenancy case and no exemption", route)
		}
	}
	for route := range covered {
		if !seen[route] {
			t.Errorf("tenancy case %s is not mounted", route)
		}
	}
	for route := range workerRouteTenancyExempt {
		if !seen[route] {
			t.Errorf("exemption %s is not mounted", route)
		}
	}
}

const tenancyRestoreCredential = "new-target-channel"

func (v *tenancyVictim) currentSession() workerapi.RuntimeSession {
	own := *v
	own.leaseEpoch = v.currentEpoch
	return own.session()
}

func (v *tenancyVictim) restoreInstallation(ids func([]byte) []byte) workerapi.AgentComputerInstallationRequest {
	return workerapi.AgentComputerInstallationRequest{EnvironmentID: v.f.Environment.String(), Installation: ids(v.restore), Receipt: ids(tenancyProto(v.restoreReceipt))}
}

// tenancyRestoreStage drives the victim's restore through its own host:
// 0 seeds a ready checkpoint and target lease, 1 prepares the installation,
// 2 validates it, 3 commits it and 4 acknowledges the restored members.
func tenancyRestoreStage(stage int) func(*testing.T, *tenancyVictim) {
	return func(t *testing.T, v *tenancyVictim) {
		t.Helper()
		// The source lease is fenced and the target lease is acquired by the
		// restore host.
		_, v.restoreTarget = seedWorkerAgentRestoreTarget(t, v.f, v.capture, workerapi.AgentComputerCaptureResponse{SaveID: v.saveID})
		dbtest.MustExec(t, t.Context(), v.f.Pool, `UPDATE computer_leases SET worker_host_id=$3 WHERE environment_id=$1 AND computer_id=$2 AND epoch=2`, v.f.Environment, v.f.Computer, v.restoreHost)
		if stage < 1 {
			return
		}
		var prepared workerapi.AgentComputerInstallationResponse
		v.post(t, "/worker/v1/agent-computers/restore/prepare", workerapi.AgentComputerRestoreRequest{EnvironmentID: v.capture.EnvironmentID, CheckpointID: v.capture.CheckpointID, LeaseEpoch: 2, ChannelCredential: tenancyRestoreCredential}, &prepared)
		p := new(agentv1.ComputerSessionInstallation)
		if err := proto.Unmarshal(prepared.Installation, p); err != nil {
			t.Fatal(err)
		}
		v.restore = prepared.Installation
		v.restoreReceipt = &agentv1.ComputerSessionReceipt{CheckpointId: p.Capture.CheckpointId, DesiredVersion: p.Capture.DesiredVersion, Frozen: true}
		if stage < 2 {
			return
		}
		v.post(t, "/worker/v1/agent-computers/restore/validate", v.restoreInstallation(func(raw []byte) []byte { return raw }), nil)
		v.restoreReceipt.Installed, v.restoreReceipt.DesiredVersion = true, p.DesiredVersion
		if stage < 3 {
			return
		}
		v.post(t, "/worker/v1/agent-computers/restore/commit", v.restoreInstallation(func(raw []byte) []byte { return raw }), nil)
		if stage < 4 {
			return
		}
		server := httptest.NewServer(v.handler)
		defer server.Close()
		acknowledgeWorkerComputerMembers(t, v.host.client(t, server.URL), v.f.Environment.String(), p)
		v.restoreReceipt.Frozen, v.restoreReceipt.ActivationStarted, v.restoreReceipt.Activated = false, true, true
	}
}

// sealTenancyCapture seals the victim's capture so checkpoint publication is
// the next step of the victim's own host.
func sealTenancyCapture(t *testing.T, v *tenancyVictim) {
	t.Helper()
	v.post(t, "/worker/v1/agent-computers/capture/seal", workerapi.AgentComputerReceiptRequest{EnvironmentID: v.capture.EnvironmentID, CheckpointID: v.capture.CheckpointID, Receipt: v.receipt(func(raw []byte) []byte { return raw })}, nil)
}

// deliverableTenancyLease gives the lease the channel credential the test
// allocator derives, so its delivery can be repeated.
func deliverableTenancyLease(t *testing.T, v *tenancyVictim) {
	t.Helper()
	credential := base64.RawURLEncoding.EncodeToString(tenancyChannel("helmr.computer-channel.v1\x00", v.f.Environment, v.f.Computer, v.f.Computer, v.f.Worker))
	digest := sha256.Sum256([]byte(credential))
	dbtest.MustExec(t, t.Context(), v.f.Pool, `UPDATE computer_leases SET channel_credential_digest=$3 WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, v.f.Environment, v.f.Computer, digest[:])
}

// reclaimableTenancyBootSeed reports reclaim pressure on the victim host.
func reclaimableTenancyBootSeed(t *testing.T, v *tenancyVictim) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), v.f.Pool, `UPDATE worker_hosts SET optional_reclaim_pending=true,resource_observed_at=clock_timestamp() WHERE id=$1`, v.f.Worker)
}

// startableTenancyImageBuild gives the running image build a seed-only
// preparation of the deployment's Program and a template that uses it.
func startableTenancyImageBuild(t *testing.T, v *tenancyVictim) {
	t.Helper()
	specs, err := artifact.BuildComputerPreparationSpecs(v.bundle.Program)
	if err != nil || len(specs) != 1 {
		t.Fatalf("specs: %v", err)
	}
	raw, err := artifact.CanonicalComputerPreparationSpec(specs[0])
	if err != nil {
		t.Fatal(err)
	}
	template := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), v.f.Pool, `
 UPDATE image_specs SET spec=$3 WHERE environment_id=$1 AND id=(SELECT image_spec_id FROM image_builds WHERE environment_id=$1 AND id=$2);
 INSERT INTO templates(environment_id,id,key) VALUES($1,$4,'tenancy-template');
 INSERT INTO template_definitions(environment_id,deployment_id,template_id,image_spec_id,cpu_millis,memory_bytes,disk_bytes,network_policy,prepare_network_policy)
 SELECT $1,$5,$4,image_spec_id,1000,536870912,NULL,'{"kind":"ordinary"}','{"kind":"block"}' FROM image_builds WHERE environment_id=$1 AND id=$2;
 `, pgx.QueryExecModeSimpleProtocol, v.f.Environment, v.imageBuild.Identity.OwnerID, string(raw), template, v.f.Deployment)
}

// startingTenancySession returns the Session process to its starting state.
func startingTenancySession(t *testing.T, v *tenancyVictim) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), v.f.Pool, `UPDATE session_processes SET status='starting' WHERE environment_id=$1 AND session_id=$2`, v.f.Environment, v.f.Session)
	var attachment workerapi.AgentAttachmentResponse
	v.post(t, "/worker/v1/sessions/attachment", v.currentSession(), &attachment)
	v.attachment = attachment.AttachmentSequence
}

// startedTenancySession authorizes the starting process's start.
func startedTenancySession(t *testing.T, v *tenancyVictim) {
	t.Helper()
	startingTenancySession(t, v)
	v.post(t, "/worker/v1/sessions/start", workerapi.AgentControlRequest{Session: v.currentSession(), AttachmentSequence: v.attachment}, nil)
}

// appendTenancyCommandLogs records the claimed Command's final output.
func appendTenancyCommandLogs(t *testing.T, v *tenancyVictim) {
	t.Helper()
	for _, stream := range []workerapi.LogStream{workerapi.LogStreamStdout, workerapi.LogStreamStderr} {
		v.post(t, "/worker/v1/computer-commands/logs/append", workerapi.CommandLogAppendRequest{EnvironmentID: v.f.Environment.String(), CommandID: v.command.String(), AllocationID: v.f.Computer.String(), WriterGeneration: 1, Stream: stream, Kind: "end", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: v.observedAt, Complete: true}, nil)
	}
}

// completeTenancyCommand records the claimed Command's result, so the victim
// host may reconcile its released process scope.
func completeTenancyCommand(t *testing.T, v *tenancyVictim) {
	t.Helper()
	appendTenancyCommandLogs(t, v)
	v.post(t, "/worker/v1/computer-commands/complete", v.commandCompletion(), nil)
}

// claimTenancyMessage steers the claimed Turn and claims the message through
// the victim host.
func claimTenancyMessage(t *testing.T, v *tenancyVictim) {
	t.Helper()
	claimTenancyTurn(t, v)
	var registered workerapi.AgentOperationResponse
	v.post(t, "/worker/v1/sessions/operations", workerapi.AgentOperationRequest{Session: v.currentSession(), RequestID: "register", AuthorityGeneration: 1, TurnID: v.turnID, Method: workerapi.AgentMethodRegisterMessages, Payload: json.RawMessage(`null`)}, &registered)
	if registered.Error != nil {
		t.Fatalf("register messages: %+v", registered.Error)
	}
	if _, err := session.SendTurn(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), v.f.Pool, session.UserCaller(v.f.Principal(t)), session.EnqueueRequest{EnvironmentID: v.f.Environment, SessionID: v.f.Session, IdempotencyKey: "tenancy-steer", Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}, uuid.MustParse(v.turnID)); err != nil {
		t.Fatal(err)
	}
	var message *workerapi.AgentMessageDispatch
	v.post(t, "/worker/v1/sessions/message", workerapi.AgentMessageRequest{Session: v.currentSession(), AttachmentSequence: v.attachment, AuthorityGeneration: 1, TurnID: v.turnID}, &message)
	if message == nil {
		t.Fatal("message was not dispatched")
	}
	v.messageID = message.MessageID
}

// cancelTenancyTurn claims a Turn and cancels the Session, so the Turn has a
// terminal outcome its host acknowledges.
func cancelTenancyTurn(t *testing.T, v *tenancyVictim) {
	t.Helper()
	claimTenancyTurn(t, v)
	if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), v.f.Pool, session.UserCaller(v.f.Principal(t)), session.SessionControlRequest{EnvironmentID: v.f.Environment, SessionID: v.f.Session, Kind: "cancel", IdempotencyKey: "tenancy-cancel"}); err != nil {
		t.Fatal(err)
	}
}

// claimTenancyTurn admits a Turn and claims it through the victim host.
func claimTenancyTurn(t *testing.T, v *tenancyVictim) {
	t.Helper()
	admitted, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), v.f.Pool, nil, session.UserCaller(v.f.Principal(t)), session.EnqueueRequest{EnvironmentID: v.f.Environment, SessionID: v.f.Session, Input: json.RawMessage(`[]`)})
	if err != nil {
		t.Fatal(err)
	}
	var dispatched *workerapi.AgentTurnDispatch
	v.post(t, "/worker/v1/sessions/turn", workerapi.AgentTurnRequest{Session: v.currentSession(), AttachmentSequence: v.attachment, AuthorityGeneration: 1}, &dispatched)
	if dispatched == nil || dispatched.TurnID != admitted.TurnID.String() {
		t.Fatalf("turn was not dispatched: %+v", dispatched)
	}
	v.turnID = admitted.TurnID.String()
}

// bindTenancyLeaseDisk binds the lease's disk write key through the key
// broker, as the victim host does before writing a save.
// tenancyUpload is an object the victim lease uploads under tenancyUploadID.
var tenancyUpload = workerapi.CASObject{Digest: sha256sum.DigestBytes([]byte("tenancy upload")), SizeBytes: int64(len("tenancy upload")), MediaType: cas.CheckpointMemoryMediaType}

const tenancyUploadID = "tenancy-upload"

func (v *tenancyVictim) storageScope() workerapi.StorageScope {
	lease := v.lease()
	return workerapi.StorageScope{Computer: &lease}
}

// receiptTenancyUpload records the victim lease's upload receipt for
// tenancyUpload, so the victim host may read it back and continue it.
func receiptTenancyUpload(t *testing.T, v *tenancyVictim) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), v.f.Pool, `WITH blob AS (INSERT INTO cas_blobs(org_id,digest,size_bytes) SELECT org_id,$2,$3 FROM environments WHERE id=$1 RETURNING org_id)
 INSERT INTO allocation_upload_receipts(worker_host_id,worker_epoch,allocation_id,org_id,digest,size_bytes,media_type,upload_id)
 SELECT $4,1,$5,org_id,$2,$3,$6,$7 FROM blob`, v.f.Environment, tenancyUpload.Digest, tenancyUpload.SizeBytes, v.f.Worker, v.f.Computer, tenancyUpload.MediaType, tenancyUploadID)
}

func bindTenancyLeaseDisk(t *testing.T, v *tenancyVictim) {
	t.Helper()
	bindFixtureLeaseDisk(t, v.f.Pool, workergroup.HostPrincipal{HostID: v.f.Worker, GroupID: v.f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}, v.f.Environment, v.f.Computer, 1)
}

// slackTenancyTurn routes the Session through a Slack thread on an active
// channel binding and claims a Turn through the victim host.
func slackTenancyTurn(t *testing.T, v *tenancyVictim) {
	t.Helper()
	installation, channel, registration, connection, thread := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), v.f.Pool, `INSERT INTO slack_app_registrations(transport,id,org_id,app_id,client_id,credential_revision,credential_ciphertext,credential_nonce,created_by_user_id)
 SELECT 'http',$4,org_id,'app','client',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),$6 FROM environments WHERE id=$1;
 INSERT INTO slack_installations(id,app_registration_id,org_id,app_id,team_id,bot_user_id,credential_revision,credential_ciphertext,credential_nonce,granted_scopes,connected_by_user_id)
 SELECT $2,$4,org_id,'app','team','bot',1,decode(repeat('11',16),'hex'),decode(repeat('22',12),'hex'),ARRAY['app_mentions:read','channels:read','channels:history','groups:read','groups:history','chat:write','assistant:write'],$6 FROM environments WHERE id=$1;
 INSERT INTO slack_connections(id,environment_id,agent_id,slack_app_registration_id,slack_installation_id,created_by_user_id,org_id) VALUES($5,$1,$7,$4,$2,$6,(SELECT org_id FROM slack_app_registrations WHERE id=$4));
 INSERT INTO slack_channel_bindings(id,environment_id,connection_id,installation_id,org_id,team_id,slack_channel_id) SELECT $3,$1,$5,$2,org_id,'team','C1' FROM environments WHERE id=$1;
 INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts,recipient_team_id,recipient_user_id)
 SELECT $8,$1,$9,id,org_id,team_id,slack_channel_id,'123.456',team_id,'human' FROM slack_channel_bindings WHERE id=$3;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($10,$8,$1,$9);
 `, pgx.QueryExecModeSimpleProtocol, v.f.Environment, installation, channel, registration, connection, v.f.User, v.f.Agent, thread, v.f.Session, uuid.NewV7())
	claimTenancyTurn(t, v)
}

// moveTenancyLease moves the Computer from a former host, whose epoch 1 lease
// is fenced, to the victim host's active epoch 2 lease and moves the Session
// process onto it.
func moveTenancyLease(t *testing.T, v *tenancyVictim, former uuid.UUID) {
	t.Helper()
	allocation := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), v.f.Pool, `
 UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='fixture former host closure',worker_host_id=$3 WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,allocation_id,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,computer_id,2,$5,worker_epoch,clock_timestamp()+interval '1 hour','active',$4,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,clock_timestamp(),clock_timestamp() FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1;
 UPDATE session_processes SET computer_lease_epoch=2 WHERE environment_id=$1 AND session_id=$6 AND epoch=1;
 `, pgx.QueryExecModeSimpleProtocol, v.f.Environment, v.f.Computer, former, allocation, v.f.Worker, v.f.Session)
}

// After a Computer moved from host A to host B and B began a later
// checkpoint, A naming its own historical lease epoch cannot prepare a restore
// of B's checkpoint: it reads exactly like a missing checkpoint.
func TestWorkerRouteTenancyFormerHostRestorePrepare(t *testing.T) {
	v := newTenancySessionVictim(t)
	setup := newPostgresServer(t, v.f.Pool, v.configure)
	former := sameGroupIntruder(t, v, setup)
	former.name = "former host"
	moveTenancyLease(t, v, former.hostID)
	v.leaseEpoch, v.currentEpoch = 1, 2
	v.victimHost(t, setup)
	checkpoint := uuid.NewV7().String()
	if _, err := session.ControlComputerExecution(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, sessiontest.ExecutionLimits(), v.f.Pool, session.UserCaller(v.f.Principal(t)), session.ComputerExecutionRequest{EnvironmentID: v.f.Environment, ComputerID: v.f.Computer, Kind: "hibernate", IdempotencyKey: checkpoint}); err != nil {
		t.Fatal(err)
	}
	v.capture = workerapi.AgentComputerCaptureRequest{EnvironmentID: v.f.Environment.String(), ComputerID: v.f.Computer.String(), CheckpointID: checkpoint, LeaseEpoch: 2, ChannelCredential: sessiontest.ChannelCredential}
	var captured workerapi.AgentComputerCaptureResponse
	v.post(t, "/worker/v1/agent-computers/capture/begin", v.capture, &captured)
	v.saveID = captured.SaveID
	c := workerRouteTenancyCase{route: "/worker/v1/agent-computers/restore/prepare", held: "checkpoint", body: func(v *tenancyVictim, _ func([]byte) []byte) any {
		return workerapi.AgentComputerRestoreRequest{EnvironmentID: v.f.Environment.String(), CheckpointID: v.capture.CheckpointID, LeaseEpoch: 1, ChannelCredential: sessiontest.ChannelCredential}
	}}
	// B's own restore needs a target lease; the restore cases above cover it.
	runTenancyCase(t, v, c, []tenancyIntruder{former}, tenancyIntruder{})
}
