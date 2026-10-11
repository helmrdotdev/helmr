//go:build linux

package computerhost

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type verifiedSessionStartClient struct {
	retainedSessionStartClient
	startup  workerapi.AgentStartResponse
	ledger   *reservation.Ledger
	releases int
}

func (c *verifiedSessionStartClient) AuthorizeAgentStart(context.Context, workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error) {
	return c.startup, nil
}
func (c *verifiedSessionStartClient) ReleaseAgentStart(_ context.Context, r workerapi.AgentStartReleaseRequest) (workerapi.AgentAuthorityResponse, error) {
	if r.BundleDigest != c.startup.BundleDigest || r.AttachmentSequence != 5 {
		return workerapi.AgentAuthorityResponse{}, errors.New("release identity changed")
	}
	if c.ledger.Snapshot().Used.HostDiskBytes != 14 {
		return workerapi.AgentAuthorityResponse{}, errors.New("released staging before transfer finished")
	}
	c.releases++
	return workerapi.AgentAuthorityResponse{AuthorityGeneration: 4, ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func TestStartAllocatedAgentSessionTransfersVerifiedSnapshots(t *testing.T) {
	index := artifacttest.ProgramMetadata(t)
	store := &fakeCAS{objects: map[string][]byte{}}
	runtimeObject := store.put(artifact.RuntimeArtifactMediaType, []byte("runtime"))
	programObject := store.put(artifact.ProgramArtifactMediaType, []byte("program"))
	index.RuntimeDigest = runtimeObject.Digest
	canonical, err := artifact.CanonicalProgramMetadata(index)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDescriptor := artifact.RuntimeDescriptor{Digest: runtimeObject.Digest, SizeBytes: runtimeObject.SizeBytes, MediaType: runtimeObject.MediaType, Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract, FormatVersion: artifact.RuntimeDescriptorFormatVersion}
	descriptor := artifact.ProgramDescriptor{Digest: programObject.Digest, SizeBytes: programObject.SizeBytes, MediaType: programObject.MediaType}
	ledger, err := reservation.New(reservation.Vector{CPUMillis: 1000, MemoryBytes: 1024, HostDiskBytes: 14})
	if err != nil {
		t.Fatal(err)
	}
	machines := &PreparedMachines{SessionLogLimits: &agentv1.SessionLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 2}, TempDir: t.TempDir(), Reservations: ledger, CAS: store, PlatformStore: store, RuntimeArchitecture: definition.ArchitectureX8664, verifiedRuntimes: map[artifact.RuntimeDescriptor]artifact.RuntimeIndex{runtimeDescriptor: {Architecture: definition.ArchitectureX8664, RuntimeContract: definition.RuntimeContract}}}
	// Only isolated verifier results are memoized. Snapshot sealing, descriptor
	// checks, transfer, release ordering and cleanup use the production operation.
	if _, err = machines.verifyProgram(t.Context(), descriptor, func() (artifact.ProgramMetadata, error) { return index, nil }); err != nil {
		t.Fatal(err)
	}
	client := &verifiedSessionStartClient{retainedSessionStartClient: retainedSessionStartClient{t}, ledger: ledger, startup: workerapi.AgentStartResponse{BundleDigest: sha256sum.DigestBytes([]byte("bundle")), ComputerID: "computer", AgentKey: "agent", Program: workerapi.RuntimeProgram{DeploymentID: "deployment", Runtime: workerapi.CASObject{Digest: runtimeObject.Digest, SizeBytes: 7, MediaType: runtimeObject.MediaType}, Artifact: workerapi.CASObject{Digest: programObject.Digest, SizeBytes: 7, MediaType: programObject.MediaType}, IndexDigest: sha256sum.DigestBytes(canonical)}}}
	client.startup.Secrets = []workerapi.SecretDelivery{{Env: &workerapi.SecretEnv{Name: "RAW_TOKEN"}, Value: []byte("env-value")}, {File: &workerapi.SecretFile{Path: "/secrets/token"}, Value: []byte("file-value")}}
	client.startup.ProtectedEnv = &workerapi.ProtectedEnv{Env: map[string]string{"TOKEN": "hlmr_protected_test"}, CA: []byte("public-ca")}
	var streams atomic.Int32
	machine, done := programTransferMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) error {
		if streams.Add(1) == 1 {
			if r.Start != nil || len(r.Secrets) != 0 || len(r.ProtectedEnv) != 0 || len(r.ProxyCa) != 0 {
				return errors.New("startup preceded absence observation")
			}
			return frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: r.Grant.Identity, AttachmentSequence: r.AttachmentSequence, Message: &agentv1.GuestSessionMessage_Absent{Absent: &agentv1.SessionAbsent{}}})
		}
		if r.GetStart().GetProgram().GetRuntime().GetDigest() != runtimeObject.Digest || r.GetStart().GetProgram().GetArtifact().GetDigest() != programObject.Digest {
			return errors.New("unverified startup descriptor")
		}
		if len(r.Secrets) != 2 || r.Secrets[0].GetEnv() != "RAW_TOKEN" || string(r.Secrets[0].Value) != "env-value" || r.Secrets[1].GetFile() != "/secrets/token" || string(r.Secrets[1].Value) != "file-value" || r.ProtectedEnv["TOKEN"] != "hlmr_protected_test" || string(r.ProxyCa) != "public-ca" {
			return errors.New("startup Secret materials changed in transport")
		}
		for _, part := range []struct {
			kind  agentv1.SessionProgramRequest_Kind
			bytes string
		}{{agentv1.SessionProgramRequest_KIND_RUNTIME, "runtime"}, {agentv1.SessionProgramRequest_KIND_ARTIFACT, "program"}} {
			if err := requestProgramPart(stream, r, part.kind); err != nil {
				return err
			}
			body := make([]byte, len(part.bytes))
			if _, err := io.ReadFull(stream, body); err != nil {
				return err
			}
			if string(body) != part.bytes {
				return errors.New("snapshot bytes changed")
			}
		}
		if err := requestProgramPart(stream, r, agentv1.SessionProgramRequest_KIND_RELEASE); err != nil {
			return err
		}
		released := new(agentv1.SessionGrant)
		if err := frameio.ReadProtoFrameBounded(stream, 64<<10, released); err != nil {
			return err
		}
		if released.AuthorityGeneration != 4 {
			return errors.New("invalid release")
		}
		return attachProgramReceipt(stream, r)
	})
	connection, _, err := machines.StartAllocatedAgentSession(t.Context(), machine, "env", sessionTransportGrant(), client)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if streams.Load() != 2 || client.releases != 2 || ledger.Snapshot().Used.HostDiskBytes != 0 {
		t.Fatalf("startup ownership: streams=%d releases=%d ledger=%+v", streams.Load(), client.releases, ledger.Snapshot())
	}
	for _, delivery := range client.startup.Secrets {
		for _, b := range delivery.Value {
			if b != 0 {
				t.Fatal("host retained plaintext startup material")
			}
		}
	}
	files, err := os.ReadDir(machines.TempDir)
	if err != nil || len(files) != 0 {
		t.Fatalf("staging remains: %v %v", files, err)
	}
}
