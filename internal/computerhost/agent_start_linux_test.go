//go:build linux

package computerhost

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/cas"
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
	startup   workerapi.AgentStartResponse
	ledger    *reservation.Ledger
	releases  int
	delivered []workerapi.SecretDelivery
}

func (c *verifiedSessionStartClient) AuthorizeAgentStart(context.Context, workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error) {
	result := c.startup
	result.Secrets = make([]workerapi.SecretDelivery, len(c.startup.Secrets))
	for i, v := range c.startup.Secrets {
		result.Secrets[i] = v
		result.Secrets[i].Value = append([]byte(nil), v.Value...)
	}
	c.delivered = result.Secrets
	return result, nil
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
	for _, requested := range []int{0, 1, 2, 3, 4} {
		t.Run(fmt.Sprint(requested), func(t *testing.T) {
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
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			if requested == 4 {
				blocked := &blockedSessionArtifactStore{Store: store, entered: make(chan struct{})}
				machines.PlatformStore = blocked
				go func() {
					select {
					case <-blocked.entered:
						cancel()
					case <-lifetime.Done():
					}
				}()
			}
			client := &verifiedSessionStartClient{retainedSessionStartClient: retainedSessionStartClient{t}, ledger: ledger, startup: workerapi.AgentStartResponse{BundleDigest: sha256sum.DigestBytes([]byte("bundle")), ComputerID: "computer", AgentKey: "agent", Program: workerapi.RuntimeProgram{DeploymentID: "deployment", Runtime: workerapi.CASObject{Digest: runtimeObject.Digest, SizeBytes: 7, MediaType: runtimeObject.MediaType}, Artifact: workerapi.CASObject{Digest: programObject.Digest, SizeBytes: 7, MediaType: programObject.MediaType}, IndexDigest: sha256sum.DigestBytes(canonical)}}}
			client.startup.Secrets = []workerapi.SecretDelivery{{Env: &workerapi.SecretEnv{Name: "RAW_TOKEN"}, Value: []byte("env-value")}, {File: &workerapi.SecretFile{Path: "/secrets/token"}, Value: []byte("file-value")}}
			client.startup.ProtectedEnv = &workerapi.ProtectedEnv{Env: map[string]string{"TOKEN": "hlmr_protected_test"}, CA: []byte("public-ca")}
			var streams atomic.Int32
			machine, done := programTransferMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) error {
				streams.Add(1)
				if r.Start == nil {
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
					if (part.kind == agentv1.SessionProgramRequest_KIND_RUNTIME && requested&1 == 0 && requested != 4) || (part.kind == agentv1.SessionProgramRequest_KIND_ARTIFACT && requested&2 == 0) {
						continue
					}
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
			if blocked, ok := machines.PlatformStore.(*blockedSessionArtifactStore); ok {
				blocked.beforeGet = func() {
					if machine.running.Load() {
						t.Error("remote staging held guest lifecycle barrier")
					}
				}
			}
			connection, _, err := machines.StartAllocatedAgentSession(lifetime, machine, "env", sessionTransportGrant(), client, false)
			if requested != 0 {
				if !errors.Is(err, errSessionArtifactsRequired) || connection != nil {
					t.Fatalf("demand probe: connection=%v err=%v", connection, err)
				}
				if len(store.getCalls) != 0 || client.releases != 1 || ledger.Snapshot().Used.HostDiskBytes != 0 {
					t.Fatalf("probe performed I/O, released execution or retained disk: gets=%v releases=%d ledger=%+v", store.getCalls, client.releases, ledger.Snapshot())
				}
				probeClosed := 0
				for range 2 {
					if err := <-done; errors.Is(err, io.EOF) {
						probeClosed++
					} else if err != nil {
						t.Fatal(err)
					}
				}
				if probeClosed != 1 {
					t.Fatalf("probe did not close exactly its transfer stream: %d", probeClosed)
				}

				connection, _, err = machines.StartAllocatedAgentSession(lifetime, machine, "env", sessionTransportGrant(), client, true)
			}

			if requested == 4 {
				if !errors.Is(err, context.Canceled) || connection != nil {
					t.Fatalf("blocked artifact read: connection=%v err=%v", connection, err)
				}
				<-done
				if client.releases != 1 || ledger.Snapshot().Used.HostDiskBytes != 0 {
					t.Fatalf("failed transfer released execution or leaked reservation: releases=%d ledger=%+v", client.releases, ledger.Snapshot())
				}
				files, err := os.ReadDir(machines.TempDir)
				if err != nil || len(files) != 0 {
					t.Fatalf("failed transfer staging remains: %v %v", files, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			wantStreams, wantReleases := int32(2), 2
			if requested != 0 {
				wantStreams, wantReleases = 4, 3
			}
			if streams.Load() != wantStreams || client.releases != wantReleases || ledger.Snapshot().Used.HostDiskBytes != 0 {
				t.Fatalf("startup ownership: streams=%d releases=%d ledger=%+v", streams.Load(), client.releases, ledger.Snapshot())
			}
			for _, delivery := range client.delivered {
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
			wantReads := 0
			if requested != 0 {
				wantReads = 1
			}
			if store.getCalls[runtimeObject.Digest] != wantReads || store.getCalls[programObject.Digest] != wantReads {
				t.Fatal("staged unrequested artifact", store.getCalls)
			}
		})
	}
}

type blockedSessionArtifactStore struct {
	cas.Store
	entered   chan struct{}
	beforeGet func()
}

func (s *blockedSessionArtifactStore) Get(ctx context.Context, _ string) (io.ReadCloser, error) {
	if s.beforeGet != nil {
		s.beforeGet()
	}
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
