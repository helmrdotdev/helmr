package executor

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type execCaptureSession struct {
	*workspaceMaterializerTestSession
	events               *[]string
	root                 computer.GenerationRoot
	pauseErr, publishErr error
}

func (s *execCaptureSession) SnapshotLimits() (vm.SnapshotLimits, error) {
	return vm.SnapshotLimits{ComputerBytes: s.root.LogicalBytes}, nil
}
func (s *execCaptureSession) PauseComputer(context.Context) (*vm.ComputerSnapshot, error) {
	*s.events = append(*s.events, "pause")
	if s.pauseErr != nil {
		return nil, s.pauseErr
	}
	return &vm.ComputerSnapshot{ComputerID: "computer", Capture: &generationCaptureFixture{root: s.root, publish: func(context.Context, computer.ContinuationPublication) error {
		*s.events = append(*s.events, "publish")
		return s.publishErr
	}, release: func() { *s.events = append(*s.events, "release") }}}, nil
}

func (s *execCaptureSession) Close(ctx context.Context) error {
	*s.events = append(*s.events, "close")
	return s.workspaceMaterializerTestSession.Close(ctx)
}

type execCaptureClient struct {
	workspaceMaterializerTestClient
	events     *[]string
	captureErr error
}

func (c *execCaptureClient) CaptureWorkspaceMount(ctx context.Context, r workerapi.WorkspaceMountCaptureRequest) (workerapi.WorkspaceMountCaptureResponse, error) {
	*c.events = append(*c.events, "stage")
	if c.captureErr != nil {
		return workerapi.WorkspaceMountCaptureResponse{}, c.captureErr
	}
	return c.workspaceMaterializerTestClient.CaptureWorkspaceMount(ctx, r)
}
func (c *execCaptureClient) StopWorkspaceMount(ctx context.Context, r workerapi.WorkspaceMountStopRequest) (workerapi.WorkspaceMountResponse, error) {
	*c.events = append(*c.events, "settle")
	return c.workspaceMaterializerTestClient.StopWorkspaceMount(ctx, r)
}

type execUnusedObjectStore struct{ *cas.File }

func (execUnusedObjectStore) Publish(context.Context, cas.Descriptor, *os.File) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected upload in ordering fixture")
}
func TestExecComputerCaptureRequiresWritebackPublicationAndPhysicalClose(t *testing.T) {
	for _, mode := range []string{"success", "guest rejects", "pause fails", "publish fails", "stage rejects", "close fails", "discard"} {
		t.Run(mode, func(t *testing.T) {
			var events []string
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			done := make(chan error, 1)
			if mode != "discard" {
				go func() {
					h, _, err := wire.ReadStreamFrameHeader(guest)
					if err != nil {
						done <- err
						return
					}
					if h.Type != wire.StreamTypeWorkspaceStop {
						done <- errors.New("wrong prepare stream")
						return
					}
					var request workspacev0.StopWorkspaceRequest
					if err := frameio.ReadProtoFrame(guest, &request); err != nil {
						done <- err
						return
					}
					if request.GetEnvelope().GetChannelToken() != "token" || request.GetEnvelope().GetFencingGeneration() != 9 {
						done <- errors.New("missing channel authority")
						return
					}
					status := "stopped"
					if mode == "guest rejects" {
						status = "failed"
					}
					done <- frameio.WriteProtoFrame(guest, &workspacev0.StopWorkspaceResponse{Status: status})
				}()
			}
			session := &execCaptureSession{workspaceMaterializerTestSession: &workspaceMaterializerTestSession{streams: []io.ReadWriteCloser{host}}, events: &events, root: testGenerationRoot(4096)}
			client := &execCaptureClient{events: &events}
			if mode == "pause fails" {
				session.pauseErr = errors.New("pause")
			}
			if mode == "publish fails" {
				session.publishErr = errors.New("publish")
			}
			if mode == "stage rejects" {
				client.captureErr = &httpclient.Error{StatusCode: 409}
			}
			if mode == "close fails" {
				session.closeErr = errors.New("close")
			}
			kind := "capture"
			if mode == "discard" {
				kind = "discard"
			}
			err := (WorkspaceMaterializer{ComputerObjects: execUnusedObjectStore{}}).stopControlledWorkspaceMount(t.Context(), session, workerapi.WorkspaceMount{ID: "mount", OrgID: "org", WorkspaceID: "computer", GuestdChannelToken: "token", FencingGeneration: 2}, workerapi.WorkspaceMountResponse{Status: "unmounting", FinalizationKind: kind, DirtyGeneration: 1, FencingGeneration: 9}, client)
			if mode != "discard" {
				if e := <-done; e != nil {
					t.Fatal(e)
				}
			}
			success := mode == "success" || mode == "discard"
			if (err == nil) != success {
				t.Fatalf("error=%v", err)
			}
			if success {
				want := []string{"pause", "publish", "stage", "release", "close", "settle"}
				if mode == "discard" {
					want = []string{"close", "settle"}
				}
				if !reflect.DeepEqual(events, want) {
					t.Fatalf("events=%v", events)
				}
			} else if client.stops != 0 || session.closeCount() == 0 {
				t.Fatalf("failed capture settled or did not stop: %v", events)
			}
			if mode == "publish fails" || mode == "stage rejects" || mode == "close fails" {
				found := false
				for _, event := range events {
					if event == "release" {
						found = true
					}
				}
				if !found {
					t.Fatalf("capture leaked on failure: %v", events)
				}
			}
			if mode == "success" && (len(client.captures) != 1 || client.captures[0].Computer.Root != session.root) {
				t.Fatal("capture root changed")
			}
		})
	}
}
