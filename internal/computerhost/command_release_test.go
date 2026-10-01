package computerhost

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type commandReleaseClient struct {
	serverTestClient
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
			mount := workerapi.ComputerInstanceAssignment{OrgID: "org", ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 2, GuestChannelCredential: "token"}
			receipt := workerapi.ComputerCommandRelease{ComputerID: "computer", RequestFingerprint: "fingerprint", Completion: workerapi.ComputerCommandCompleteRequest{OrgID: "org", CommandID: "command", ComputerInstanceID: "instance", WriterGeneration: 2, Outcome: "exited"}}
			err := (Server{}).releaseComputerCommand(t.Context(), &serverTestSession{operation: host}, mount, receipt, client)
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
	mount := workerapi.ComputerInstanceAssignment{OrgID: "org", ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 2, GuestChannelCredential: "token"}
	receipt := workerapi.ComputerCommandRelease{ComputerID: "computer", RequestFingerprint: "fingerprint", Completion: workerapi.ComputerCommandCompleteRequest{OrgID: "org", CommandID: "command", ComputerInstanceID: "instance", WriterGeneration: 2}}
	result := make(chan error, 1)
	go func() {
		result <- (Server{}).releaseComputerCommand(ctx, &serverTestSession{operation: host}, mount, receipt, client)
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

type commandReleaseOrderClient struct {
	serverTestClient
	record func(string)
}

func (c *commandReleaseOrderClient) ReconcileComputerCommand(context.Context, workerapi.ComputerCommandCompleteRequest) error {
	c.record("reconcile")
	return nil
}

// commandReleaseOrderStream answers one release and records when its Close
// starts; Close blocks until releaseClose is closed.
type commandReleaseOrderStream struct {
	response     *bytes.Reader
	record       func(string)
	closeStarted chan struct{}
	releaseClose chan struct{}
	once         sync.Once
}

func (s *commandReleaseOrderStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *commandReleaseOrderStream) Read(p []byte) (int, error)  { return s.response.Read(p) }
func (s *commandReleaseOrderStream) Close() error {
	s.once.Do(func() {
		s.record("close")
		close(s.closeStarted)
	})
	<-s.releaseClose
	return nil
}

func TestCommandReleaseReconcilesBeforeClosingGuestStream(t *testing.T) {
	var response bytes.Buffer
	if err := frameio.WriteProtoFrame(&response, &computerv0.ComputerCommandReleaseResponse{Released: true}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	stream := &commandReleaseOrderStream{response: bytes.NewReader(response.Bytes()), record: record, closeStarted: make(chan struct{}), releaseClose: make(chan struct{})}
	client := &commandReleaseOrderClient{record: record}
	mount := workerapi.ComputerInstanceAssignment{OrgID: "org", ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 2, GuestChannelCredential: "token"}
	receipt := workerapi.ComputerCommandRelease{ComputerID: "computer", RequestFingerprint: "fingerprint", Completion: workerapi.ComputerCommandCompleteRequest{OrgID: "org", CommandID: "command", ComputerInstanceID: "instance", WriterGeneration: 2, Outcome: "exited"}}
	result := make(chan error, 1)
	go func() {
		result <- (Server{}).releaseComputerCommand(t.Context(), &guestControlTestMachine{stream: testVMStream(stream)}, mount, receipt, client)
	}()
	select {
	case <-stream.closeStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("release did not close its guest stream")
	}
	mu.Lock()
	got := append([]string(nil), events...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "reconcile" || got[1] != "close" {
		t.Fatalf("events = %v, want [reconcile close]", got)
	}
	select {
	case err := <-result:
		t.Fatalf("release returned before its stream closed: %v", err)
	default:
	}
	close(stream.releaseClose)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
