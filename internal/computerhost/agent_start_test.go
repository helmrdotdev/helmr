package computerhost

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"io"
	"net"
	"os"
	"testing"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type retainedSessionStartClient struct{ t *testing.T }

func (c retainedSessionStartClient) AcquireAgentAttachment(context.Context, workerapi.RuntimeSession) (workerapi.AgentAttachmentResponse, error) {
	return workerapi.AgentAttachmentResponse{Starting: true, AttachmentSequence: 5, AuthorityGeneration: 4, ExpiresAt: time.Now().Add(time.Minute)}, nil
}
func (c retainedSessionStartClient) AuthorizeAgentStart(context.Context, workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error) {
	c.t.Fatal("retained process requested its unavailable Program")
	return workerapi.AgentStartResponse{}, nil
}
func TestStartAllocatedAgentSessionRetainedProcessNeedsNoProgram(t *testing.T) {
	machine := sessionTransportMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) {
		if r.Start != nil || r.AttachmentSequence != 5 || r.Grant.AuthorityGeneration != 4 {
			t.Error("retained process received startup")
		}
		_, _ = io.Copy(io.Discard, stream)
	})
	var machines PreparedMachines // No artifact stores or verifier are available.
	connection, attached, err := machines.StartAllocatedAgentSession(t.Context(), machine, "env", sessionTransportGrant(), retainedSessionStartClient{t})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if !attached.Ready {
		t.Fatal("retained process not ready")
	}
}

func (c retainedSessionStartClient) ReleaseAgentStart(context.Context, workerapi.AgentStartReleaseRequest) (workerapi.AgentAuthorityResponse, error) {
	c.t.Fatal("retained process requested setup release")
	return workerapi.AgentAuthorityResponse{}, nil
}

type newSessionStartClient struct{ retainedSessionStartClient }

func (c newSessionStartClient) AuthorizeAgentStart(context.Context, workerapi.AgentControlRequest) (workerapi.AgentStartResponse, error) {
	return workerapi.AgentStartResponse{ComputerID: "computer", Program: workerapi.RuntimeProgram{DeploymentID: "deployment", Runtime: workerapi.CASObject{SizeBytes: 7}, Artifact: workerapi.CASObject{SizeBytes: 7}}}, nil
}
func TestStartAllocatedAgentSessionBudgetsStagingAndKeepsPeerReservation(t *testing.T) {
	for _, capacity := range []int64{100, 110} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			ledger, err := reservation.New(reservation.Vector{CPUMillis: 1000, MemoryBytes: 1024, HostDiskBytes: capacity})
			if err != nil {
				t.Fatal(err)
			}
			peer := reservation.Key{Kind: "instance", ID: "peer", Epoch: 1}
			if _, err = ledger.Reserve(peer, reservation.Vector{HostDiskBytes: 90}); err != nil {
				t.Fatal(err)
			}
			machine, done := programTransferMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) error {
				return frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: r.Grant.Identity, AttachmentSequence: r.AttachmentSequence, Message: &agentv1.GuestSessionMessage_Absent{Absent: &agentv1.SessionAbsent{}}})
			})
			machines := PreparedMachines{SessionLogLimits: &agentv1.SessionLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 2}, TempDir: t.TempDir(), Reservations: ledger}
			_, _, err = machines.StartAllocatedAgentSession(t.Context(), machine, "env", sessionTransportGrant(), newSessionStartClient{retainedSessionStartClient{t}})
			if err == nil {
				t.Fatal("missing verifier unexpectedly succeeded")
			}
			if capacity == 100 && !errors.Is(err, reservation.ErrCapacityExceeded) {
				t.Fatalf("unreserved staging: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			snapshot := ledger.Snapshot()
			if snapshot.Used.HostDiskBytes != 90 || len(snapshot.Reservations) != 1 {
				t.Fatalf("failed start changed peer reservation: %+v", snapshot)
			}
			files, err := os.ReadDir(machines.TempDir)
			if err != nil || len(files) != 0 {
				t.Fatalf("staging leaked: %v %v", files, err)
			}
		})
	}
}
func TestStartAllocatedAgentSessionDoesNotInferAbsenceFromTransportFailure(t *testing.T) {
	machine, done := programTransferMachine(t, func(net.Conn, *agentv1.SessionAttach) error { return nil })
	var machines PreparedMachines
	_, _, err := machines.StartAllocatedAgentSession(t.Context(), machine, "env", sessionTransportGrant(), retainedSessionStartClient{t})
	if err == nil || errors.Is(err, ErrAgentSessionAbsent) {
		t.Fatalf("unknown outcome treated as absence: %v", err)
	}
	<-done
}
