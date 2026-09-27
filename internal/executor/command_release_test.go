package executor

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type commandReleaseClient struct {
	computerMaterializerTestClient
	reconciled bool
}

func (c *commandReleaseClient) ReconcileComputerCommand(context.Context, workerapi.ComputerCommandCompleteRequest) error {
	c.reconciled = true
	return nil
}

func TestCommandReleaseWaitsForGuestReceipt(t *testing.T) {
	for _, guestError := range []string{"", "scope unresolved"} {
		t.Run(guestError, func(t *testing.T) {
			host, guest := net.Pipe()
			defer guest.Close()
			done := make(chan error, 1)
			go func() {
				header, _, err := wire.ReadStreamFrameHeader(guest)
				if err != nil {
					done <- err
					return
				}
				if header.Type != wire.StreamTypeComputerCommandRelease {
					done <- errors.New("wrong control type")
					return
				}
				var request computerv0.ComputerCommandReleaseRequest
				if err = frameio.ReadProtoFrame(guest, &request); err != nil {
					done <- err
					return
				}
				if request.GetAuthority().GetWriterGeneration() != 2 || request.GetAuthority().GetComputerInstanceId() != "instance" {
					done <- errors.New("wrong physical authority")
					return
				}
				done <- frameio.WriteProtoFrame(guest, &computerv0.ComputerCommandReleaseResponse{Released: guestError == "", Error: guestError})
			}()
			client := &commandReleaseClient{}
			mount := workerapi.ComputerInstanceAssignment{OrgID: "org", ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 2, GuestdChannelToken: "token"}
			receipt := workerapi.ComputerCommandRelease{ComputerID: "computer", RequestFingerprint: "fingerprint", Completion: workerapi.ComputerCommandCompleteRequest{OrgID: "org", CommandID: "command", ComputerInstanceID: "instance", WriterGeneration: 2, Outcome: "exited"}}
			err := (ComputerMaterializer{}).releaseComputerCommand(t.Context(), &computerMaterializerTestSession{operation: host}, mount, receipt, client)
			if (err == nil) != (guestError == "") || client.reconciled != (guestError == "") {
				t.Fatalf("err=%v reconciled=%v", err, client.reconciled)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCommandReleaseCancelsSilentPeer(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := make(chan error, 1)
	go func() {
		_, _, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			observed <- err
			cancel()
			return
		}
		var request computerv0.ComputerCommandReleaseRequest
		err = frameio.ReadProtoFrame(guest, &request)
		observed <- err
		cancel()
	}()
	client := &commandReleaseClient{}
	mount := workerapi.ComputerInstanceAssignment{OrgID: "org", ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 2, GuestdChannelToken: "token"}
	receipt := workerapi.ComputerCommandRelease{ComputerID: "computer", RequestFingerprint: "fingerprint", Completion: workerapi.ComputerCommandCompleteRequest{OrgID: "org", CommandID: "command", ComputerInstanceID: "instance", WriterGeneration: 2}}
	result := make(chan error, 1)
	go func() {
		result <- (ComputerMaterializer{}).releaseComputerCommand(ctx, &computerMaterializerTestSession{operation: host}, mount, receipt, client)
	}()
	select {
	case err := <-result:
		if err == nil || client.reconciled {
			t.Fatal("cancelled control reconciled")
		}
	case <-time.After(time.Second):
		t.Fatal("silent Guest prevented shutdown")
	}
	if err := <-observed; err != nil {
		t.Fatal(err)
	}
}
