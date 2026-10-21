package computerhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

type waitingCheckpointPublisher struct {
	cas.ImmutableStore
	start     func()
	cancelled func()
	active    atomic.Int32
}

func (p *waitingCheckpointPublisher) Publish(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	p.active.Add(1)
	defer p.active.Add(-1)
	p.start()
	<-ctx.Done()
	p.cancelled()
	return cas.Object{}, ctx.Err()
}

func TestCheckpointDemandResumesSourceAfterReservationWithoutLosingDiskCut(t *testing.T) {
	o, c, m, cut := checkpointPipelineFixture(t, "", "")
	f := &allocationContinuationFixture{AllocatedComputerClient: c, t: t, target: o.identity, source: true, scenario: "stale-before-install"}
	m.control = &agentControlMachine{handle: func(stream net.Conn) {
		f.mu.Lock()
		if f.capture == nil {
			f.capture = new(agentv1.ComputerSessionCapture)
			if err := proto.Unmarshal(c.capture, f.capture); err != nil {
				t.Error(err)
			}
		}
		// The first request is a successful guest freeze; subsequent requests use
		// the continuation fixture's stale-grant and activation protocol.
		f.mu.Unlock()
		f.handle(stream)
	}}
	o.client = f
	o.observe = func(context.Context, *agentv1.GuestSessionMessage) error { return nil }
	o.saves = &agentComputerSaves{client: c, objects: o.machines.ComputerObjects, identity: o.identity}
	publisher := &waitingCheckpointPublisher{ImmutableStore: o.machines.ComputerObjects, start: func() { c.workPending.Store(true); o.Wake() }, cancelled: func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.validate < 2 {
			t.Error("upload cancelled before source reservation")
		}
	}}
	o.machines.ComputerObjects = publisher
	resumed := 0
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := o.captureIdle(ctx, workerapi.ComputerAllocationDelivery{BaseVersion: "base", ChannelCredential: "current"}, func() func() { return func() { resumed++ } })
	if err != nil {
		t.Fatal(err)
	}
	if publisher.active.Load() != 0 || resumed != 1 || m.snapshots != 1 || m.resumes != 1 || m.aborts != 1 || !f.activated || f.absence != 0 || o.checkpointCut != nil || o.saves.pending == nil || o.saves.capture != m.artifact.Computer.Capture {
		t.Fatalf("continuation/custody lost: resumed=%d snapshots=%d activate=%v absence=%d pending=%v", resumed, m.snapshots, f.activated, f.absence, o.saves.pending)
	}
	for _, step := range cut.steps {
		if step == "release" {
			t.Fatal("cut released before publication")
		}
	}
	if err := o.saves.step(ctx); err != nil {
		t.Fatal(err)
	}
	if o.saves.pending != nil || o.saves.maintenance == nil {
		t.Fatal("retained save did not finish independently")
	}
	o.saves.release()
}

type rejectedSourceReservation struct {
	AllocatedComputerClient
	rejected      chan struct{}
	once          sync.Once
	notReadyFirst bool
	calls         int
}

func (c *rejectedSourceReservation) ValidateAgentComputerSourceAbort(context.Context, workerapi.AgentComputerInstallationRequest) error {
	c.calls++
	if c.notReadyFirst && c.calls == 1 {
		return &httpclient.Error{StatusCode: 409, Code: workerapi.AgentComputerNotReady}
	}
	c.once.Do(func() { close(c.rejected) })
	return &httpclient.Error{StatusCode: 409, Code: workerapi.AgentComputerEvidenceConflict}
}

type publicationAfterReservationRejection struct {
	cas.ImmutableStore
	rejected <-chan struct{}
	demand   func()
}

func (p *publicationAfterReservationRejection) Publish(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	p.demand()
	select {
	case <-p.rejected:
	case <-ctx.Done():
		return cas.Object{}, ctx.Err()
	}
	return p.ImmutableStore.Publish(ctx, d, f)
}
func TestCheckpointReservationRejectionFinishesSuccessfulCut(t *testing.T) {
	for _, notReadyFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(notReadyFirst), func(t *testing.T) {
			o, c, m, _ := checkpointPipelineFixture(t, "", "")
			f := &allocationContinuationFixture{AllocatedComputerClient: c, t: t, target: o.identity, source: true, scenario: "joined-uncut"}
			m.control = &agentControlMachine{handle: func(stream net.Conn) {
				f.mu.Lock()
				if f.capture == nil {
					f.capture = new(agentv1.ComputerSessionCapture)
					if err := proto.Unmarshal(c.capture, f.capture); err != nil {
						t.Error(err)
					}
				}
				f.mu.Unlock()
				f.handle(stream)
			}}
			rejected := make(chan struct{})
			o.client = &rejectedSourceReservation{AllocatedComputerClient: f, rejected: rejected, notReadyFirst: notReadyFirst}
			o.machines.ComputerObjects = &publicationAfterReservationRejection{ImmutableStore: o.machines.ComputerObjects, rejected: rejected, demand: func() { c.workPending.Store(true); o.Wake() }}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := o.captureIdle(ctx, workerapi.ComputerAllocationDelivery{BaseVersion: "base", ChannelCredential: "current"}, func() func() { return func() { t.Error("unreserved source dispatch reopened") } })
			if !errors.Is(err, errComputerHibernated) {
				t.Fatal(err)
			}
			if len(c.manifests) != 2 || f.activated || f.absence != 0 || o.checkpointCut == nil {
				t.Fatalf("coherent publication lost: manifests=%d activated=%v absence=%d cut=%v", len(c.manifests), f.activated, f.absence, o.checkpointCut)
			}
		})
	}
}

// A reservation may have committed even when its reply is lost. A later
// rejection cannot make waiting for the now-unpublishable cut safe.
type lostSourceReservation struct {
	AllocatedComputerClient
	calls      int
	firstError error
}

func (c *lostSourceReservation) ValidateAgentComputerSourceAbort(context.Context, workerapi.AgentComputerInstallationRequest) error {
	c.calls++
	if c.calls == 1 {
		return c.firstError
	}
	return &httpclient.Error{StatusCode: 409, Code: workerapi.AgentComputerEvidenceConflict}
}
func TestCheckpointUncertainReservationFailsWithoutWaitingForPublication(t *testing.T) {
	for _, firstError := range []error{context.DeadlineExceeded, &httpclient.Error{StatusCode: 408}, &httpclient.Error{StatusCode: 425}, &httpclient.Error{StatusCode: 429}} {
		t.Run(fmt.Sprint(firstError), func(t *testing.T) {
			o, c, m, _ := checkpointPipelineFixture(t, "", "")
			f := &allocationContinuationFixture{AllocatedComputerClient: c, t: t, target: o.identity, source: true, scenario: "joined-uncut"}
			m.control = &agentControlMachine{handle: func(stream net.Conn) {
				f.mu.Lock()
				if f.capture == nil {
					f.capture = new(agentv1.ComputerSessionCapture)
					if err := proto.Unmarshal(c.capture, f.capture); err != nil {
						t.Error(err)
					}
				}
				f.mu.Unlock()
				f.handle(stream)
			}}
			reservation := &lostSourceReservation{AllocatedComputerClient: f, firstError: firstError}
			o.client = reservation
			publisher := &waitingCheckpointPublisher{ImmutableStore: o.machines.ComputerObjects, start: func() { c.workPending.Store(true); o.Wake() }, cancelled: func() {}}
			o.machines.ComputerObjects = publisher
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := o.captureIdle(ctx, workerapi.ComputerAllocationDelivery{BaseVersion: "base", ChannelCredential: "current"}, func() func() { return func() { t.Error("unreserved source dispatch reopened") } })
			var rejected *httpclient.Error
			if !errors.As(err, &rejected) || rejected.Code != workerapi.AgentComputerEvidenceConflict {
				t.Fatalf("uncertain reservation did not fail explicitly: %v", err)
			}
			if reservation.calls != 2 || publisher.active.Load() != 0 || f.activated || f.absence != 0 {
				t.Fatalf("unsafe continuation or unjoined publisher: calls=%d active=%d activated=%v absence=%d", reservation.calls, publisher.active.Load(), f.activated, f.absence)
			}
		})
	}
}
