package computerhost

import (
	"context"
	"errors"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"io"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestInstanceRenewalResponseAuthority(t *testing.T) {
	now := time.Now()
	request := workerapi.ComputerInstanceRenewRequest{EnvironmentID: "environment", ComputerInstanceID: "instance", WriterGeneration: 3}
	ready := workerapi.ComputerInstanceRenewResponse{ComputerInstanceID: "instance", WriterGeneration: 3, DesiredState: "ready", DesiredVersion: 2, ObservedVersion: 1, WriterExpiresAt: now.Add(time.Minute)}
	if err := validateInstanceRenewal(request, ready, now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*workerapi.ComputerInstanceRenewResponse){
		"instance": func(r *workerapi.ComputerInstanceRenewResponse) { r.ComputerInstanceID = "other" },
		"writer":   func(r *workerapi.ComputerInstanceRenewResponse) { r.WriterGeneration++ },
		"expired":  func(r *workerapi.ComputerInstanceRenewResponse) { r.WriterExpiresAt = now },
		"state":    func(r *workerapi.ComputerInstanceRenewResponse) { r.DesiredState = "unknown" },
		"version":  func(r *workerapi.ComputerInstanceRenewResponse) { r.DesiredVersion = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := ready
			mutate(&changed)
			if err := validateInstanceRenewal(request, changed, now); err == nil {
				t.Fatal("invalid renewal accepted")
			}
		})
	}
	closed := ready
	closed.DesiredState = "closed"
	closed.WriterExpiresAt = now.Add(-time.Hour)
	if err := validateInstanceRenewal(request, closed, now); err != nil {
		t.Fatal("close observation incorrectly required a live grant", err)
	}
}

type instanceClosingClient struct{ serverTestClient }

func (c *instanceClosingClient) RenewComputerInstance(_ context.Context, r workerapi.ComputerInstanceRenewRequest) (workerapi.ComputerInstanceRenewResponse, error) {
	return workerapi.ComputerInstanceRenewResponse{ComputerInstanceID: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration, DesiredState: "closed", DesiredVersion: 2, ObservedVersion: 1}, nil
}
func TestInstanceCloseObservationStopsPhysicalSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	client := &instanceClosingClient{}
	m := Server{PollEvery: time.Hour}
	request := workerapi.ComputerInstanceRenewRequest{EnvironmentID: "environment", ComputerInstanceID: "instance", WriterGeneration: 3}
	renewal := m.startRenewalLoop(ctx, request, client, time.Millisecond)
	raw := &serverTestSession{exit: make(chan error)}
	err := m.serveComputerMount(ctx, renewal, newInstanceMount(raw), nil, workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", WriterGeneration: 3, RuntimeEpoch: 7}, client, nil)
	if err != nil || raw.closeCount() != 1 || client.stops != 1 {
		t.Fatalf("err=%v close=%d stops=%d", err, raw.closeCount(), client.stops)
	}
	r := client.closed[0]
	if r.ID != "instance" || r.WorkerEpoch != 7 || r.DesiredVersion != 2 || r.ExpectedObservedVersion != 1 || r.CleanupProof == nil || r.CleanupProof.Method != workerapi.RuntimeCleanupSessionClosed {
		t.Fatalf("close authority=%+v", r)
	}
}

func TestInstanceFailureReportsPhysicalAuthorityWithoutCleanup(t *testing.T) {
	client := &serverTestClient{}
	mount := workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", RuntimeEpoch: 7, DesiredVersion: 9, ObservedVersion: 5}
	if err := (Server{}).failComputerMount(client, mount, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	if len(client.failures) != 1 {
		t.Fatalf("failures=%v", client.failures)
	}
	r := client.failures[0]
	if r.ID != mount.ComputerInstanceID || r.WorkerEpoch != 7 || r.DesiredVersion != 9 || r.ExpectedObservedVersion != 5 || r.CleanupProof != nil {
		t.Fatalf("failure authority=%+v", r)
	}
	mount.DesiredVersion = 0
	if err := (Server{}).failComputerMount(client, mount, context.DeadlineExceeded); err == nil || len(client.failures) != 1 {
		t.Fatal("missing authority was synthesized")
	}
}

// The second call can only begin after the first response has been validated
// and stored by the renewal loop, independently of the serve-loop consumer.
type startupRenewalClient struct {
	serverTestClient
	observed chan struct{}
	calls    int
}

func (c *startupRenewalClient) RenewComputerInstance(_ context.Context, r workerapi.ComputerInstanceRenewRequest) (workerapi.ComputerInstanceRenewResponse, error) {
	c.calls++
	if c.calls == 2 {
		close(c.observed)
	}
	return workerapi.ComputerInstanceRenewResponse{ComputerInstanceID: r.ComputerInstanceID, WriterGeneration: r.WriterGeneration, DesiredState: "closed", DesiredVersion: 4, ObservedVersion: 3}, nil
}
func TestStartupFailureUsesLatestInstanceObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	store, mount := testComputerMountArtifacts(t)
	mount.ComputerInstanceID = "mount"
	mount.GuestChannelCredential = "channel-credential"
	mount.GuestChannelCredentialHash = sha256sum.HexBytes([]byte(mount.GuestChannelCredential))
	client := &startupRenewalClient{observed: make(chan struct{})}
	host, guest := net.Pipe()
	defer guest.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		respondToPreparedComputerMountWithRequest(t, guest, func(*computerv0.MaterializeComputerRequest) *computerv0.MaterializeComputerResponse {
			select {
			case <-client.observed:
			case <-ctx.Done():
			}
			return &computerv0.MaterializeComputerResponse{Status: "failed"}
		})
	}()
	session := &serverTestSession{streams: []io.ReadWriteCloser{host}, closeErr: errors.New("cleanup failed")}
	machines := computerPreparedMachines(t, mount, session)
	m := Server{RestoreControl: unusedComputerRestoreControl{}, ComputerSaves: &saveHostFixture{}, ComputerSaveEvery: time.Hour, ComputerObjects: &checkpointCAS{}, Mounts: NewMounts(), CAS: store, Machines: machines, Heartbeat: time.Millisecond}
	err := m.Serve(ctx, mount, client)
	<-done
	if err == nil || len(client.failures) != 2 {
		t.Fatalf("err=%v failures=%v", err, client.failures)
	}
	for _, r := range client.failures {
		if r.ID != mount.ComputerInstanceID || r.DesiredVersion != 4 || r.ExpectedObservedVersion != 3 || r.CleanupProof != nil {
			t.Fatalf("stale startup/cleanup failure=%+v", r)
		}
	}
	if mount.DesiredVersion != 1 || mount.ObservedVersion != 1 {
		t.Fatal("shared descriptor was mutated")
	}
}
