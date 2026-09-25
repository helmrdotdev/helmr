package executor

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"net/http"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type saveHostFixture struct {
	mu                sync.Mutex
	steps             []string
	fail              string
	failed            bool
	runtime, computer string
	root              computer.GenerationRoot
	requests          []workerapi.ComputerSavePublicationRequest
	blocked, joined   chan struct{}
}

func (f *saveHostFixture) step(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steps = append(f.steps, name)
	if f.fail == name && !f.failed {
		f.failed = true
		return errors.New("lost " + name + " response")
	}
	return nil
}
func (f *saveHostFixture) BeginComputerSave(_ context.Context, r workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
	err := f.step("begin")
	runtimeID := f.runtime
	if f.fail == "wrong admission" {
		runtimeID = uuid.NewV7().String()
	}
	return workerapi.ComputerSaveBeginResponse{RuntimeInstanceID: runtimeID, SaveID: r.SaveID, Sequence: r.Sequence, WorkspaceLeaseID: uuid.NewV7().String(), PredecessorID: uuid.NewV7().String(), DesiredVersion: 1}, err
}
func (f *saveHostFixture) RegisterComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error {
	return nil
}
func (f *saveHostFixture) CertifyComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error {
	return nil
}
func (f *saveHostFixture) ReuseComputerSaveObject(context.Context, workerapi.ComputerSaveObjectRequest) error {
	return nil
}
func (f *saveHostFixture) PublishComputerSave(_ context.Context, r workerapi.ComputerSavePublicationRequest) (workerapi.ComputerSavePublicationResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, r)
	f.mu.Unlock()
	err := f.step("commit")
	computerID := f.computer
	if f.fail == "wrong receipt" {
		computerID = uuid.NewV7().String()
	}
	return workerapi.ComputerSavePublicationResponse{ComputerID: computerID, VersionID: r.Save.SaveID}, err
}
func (f *saveHostFixture) AdoptComputerSave(context.Context, workerapi.ComputerSavePublicationRequest) error {
	return f.step("ack")
}
func (f *saveHostFixture) AbandonComputerSave(context.Context, workerapi.ComputerSaveBeginRequest) error {
	return f.step("abandon")
}
func (f *saveHostFixture) Publish(context.Context, cas.Descriptor, *os.File) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected upload in lifecycle fixture")
}

type saveHostCapture struct{ f *saveHostFixture }

func (c saveHostCapture) Root() computer.GenerationRoot { return c.f.root }
func (c saveHostCapture) Publish(ctx context.Context, _ computer.ContinuationPublication) error {
	if c.f.blocked != nil {
		close(c.f.blocked)
		<-ctx.Done()
		<-c.f.joined
		return ctx.Err()
	}
	if err := c.f.step("objects"); err != nil {
		return err
	}
	if c.f.fail == "abandon" {
		return errors.New("upload failed before commit")
	}
	return nil
}
func (c saveHostCapture) Adopt(context.Context, int) error            { return c.f.step("adopt") }
func (c saveHostCapture) Collect(context.Context, int) (int64, error) { return 0, nil }
func (c saveHostCapture) Release()                                    { _ = c.f.step("release") }

func newSaveHostFixture(t *testing.T, failure string) (*saveHostFixture, *computerSave) {
	t.Helper()
	f := &saveHostFixture{fail: failure, runtime: uuid.NewV7().String(), computer: uuid.NewV7().String(), root: computer.GenerationRoot{FormatVersion: 1, LogicalBytes: 1 << 20}}
	if failure == "blocked" {
		f.blocked = make(chan struct{})
		f.joined = make(chan struct{})
	}
	r := workerapi.ComputerSaveBeginRequest{OrgID: uuid.NewV7().String(), WorkspaceMountID: uuid.NewV7().String(), SaveID: uuid.NewV7().String(), Sequence: 1}
	s, err := startComputerSave(t.Context(), f, f, r, f.runtime, f.computer, func(context.Context) (computerSaveCapture, error) {
		if err := f.step("capture"); err != nil {
			return nil, err
		}
		return saveHostCapture{f}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}

func TestComputerSaveHostReconcilesWithoutRecapture(t *testing.T) {
	for _, tt := range []struct {
		fail string
		want []string
	}{
		{"", []string{"begin", "capture", "objects", "commit", "adopt", "ack", "release"}},
		{"begin", []string{"begin", "begin", "abandon"}},
		{"objects", []string{"begin", "capture", "objects", "abandon", "release"}},
		{"commit", []string{"begin", "capture", "objects", "commit", "commit", "adopt", "ack", "release"}},
		{"adopt", []string{"begin", "capture", "objects", "commit", "adopt", "adopt", "ack", "release"}},
		{"ack", []string{"begin", "capture", "objects", "commit", "adopt", "ack", "ack", "release"}},
	} {
		t.Run(tt.fail, func(t *testing.T) {
			f, s := newSaveHostFixture(t, tt.fail)
			err := s.Wait(t.Context())
			if (err != nil) != (tt.fail != "") {
				t.Fatalf("attempt error: %v", err)
			}
			// Multiple lifecycle consumers must not repeat release or mutate a
			// succeeding operation while recovering an uncertain acknowledgement.
			done := make(chan error, 2)
			for range 2 {
				go func() { done <- s.Quiesce(t.Context()) }()
			}
			for range 2 {
				if err = <-done; err != nil {
					t.Fatal(err)
				}
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if !reflect.DeepEqual(f.steps, tt.want) {
				t.Fatalf("steps %v; want %v", f.steps, tt.want)
			}
			for _, request := range f.requests {
				if request.Save.SaveID != s.request.SaveID || request.Save.Sequence != s.request.Sequence || request.Root != f.root {
					t.Fatal("reconciliation changed publication")
				}
			}
		})
	}
}

func TestComputerSaveHostJoinsUploadBeforeAbandonment(t *testing.T) {
	f, s := newSaveHostFixture(t, "blocked")
	defer close(f.joined)
	select {
	case <-f.blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("upload not started")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := s.Quiesce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("did not retain unjoined producer: %v", err)
	}
	f.mu.Lock()
	if !reflect.DeepEqual(f.steps, []string{"begin", "capture"}) {
		t.Errorf("unjoined producer was released: %v", f.steps)
	}
	f.mu.Unlock()
	// Finish the producer only after proving the bounded wait did not release
	// its capture or remote pins. Another Quiesce can then finish cancellation.
	f.joined <- struct{}{}
	if err := s.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.steps, []string{"begin", "capture", "abandon", "release"}) {
		t.Fatalf("joined cleanup: %v", f.steps)
	}
}

func TestComputerSaveHostDoesNotContinueAfterAmbiguousCapture(t *testing.T) {
	f, s := newSaveHostFixture(t, "capture")
	if err := s.Wait(t.Context()); err == nil {
		t.Fatal("missing capture failure")
	}
	if err := s.Quiesce(t.Context()); err == nil {
		t.Fatal("ambiguous guest dispatch allowed continuation")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.steps, []string{"begin", "capture"}) {
		t.Fatalf("uncertain capture released: %v", f.steps)
	}
}

func TestComputerSaveHostReconcilesLostAbandonmentReply(t *testing.T) {
	f, s := newSaveHostFixture(t, "abandon")
	if err := s.Wait(t.Context()); err == nil {
		t.Fatal("missing upload failure")
	}
	if err := s.Quiesce(t.Context()); err == nil {
		t.Fatal("missing lost abandonment reply")
	}
	if err := s.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !reflect.DeepEqual(f.steps, []string{"begin", "capture", "objects", "abandon", "abandon", "release"}) {
		t.Fatalf("reconciliation: %v", f.steps)
	}
}

func TestComputerSaveHostRejectsMismatchedAuthorityResponses(t *testing.T) {
	for _, failure := range []string{"wrong admission", "wrong receipt"} {
		t.Run(failure, func(t *testing.T) {
			f, s := newSaveHostFixture(t, failure)
			if err := s.Wait(t.Context()); err == nil {
				t.Fatal("mismatched response accepted")
			}
			if err := s.Quiesce(t.Context()); err == nil {
				t.Fatal("unresolved mismatch allowed continuation")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, step := range f.steps {
				if step == "adopt" || step == "ack" || step == "abandon" || step == "release" {
					t.Fatalf("invalid authority released state: %v", f.steps)
				}
			}
		})
	}
}

// The first publication reply is lost across a managed-wait transition. The
// server later either returns its immutable receipt or denies new publication.
type waitingSaveClient struct {
	*saveHostFixture
	published       bool
	commitOnAbandon bool
	calls           int
}

func (c *waitingSaveClient) PublishComputerSave(_ context.Context, r workerapi.ComputerSavePublicationRequest) (workerapi.ComputerSavePublicationResponse, error) {
	c.calls++
	if c.calls == 1 {
		return workerapi.ComputerSavePublicationResponse{}, errors.New("lost publication reply")
	}
	if !c.published {
		return workerapi.ComputerSavePublicationResponse{}, &httpclient.Error{StatusCode: http.StatusConflict}
	}
	return workerapi.ComputerSavePublicationResponse{ComputerID: c.computer, VersionID: r.Save.SaveID}, nil
}
func (c *waitingSaveClient) AbandonComputerSave(ctx context.Context, r workerapi.ComputerSaveBeginRequest) error {
	if c.commitOnAbandon {
		c.published = true
		c.commitOnAbandon = false
	}
	if c.published {
		return &httpclient.Error{StatusCode: http.StatusConflict}
	}
	return c.saveHostFixture.AbandonComputerSave(ctx, r)
}

func TestComputerSaveHostSettlesLostCommitAcrossWait(t *testing.T) {
	for _, scenario := range []string{"unpublished", "committed", "lost abandon", "commit race"} {
		t.Run(scenario, func(t *testing.T) {
			f := &saveHostFixture{runtime: uuid.NewV7().String(), computer: uuid.NewV7().String()}
			client := &waitingSaveClient{saveHostFixture: f, published: scenario == "committed", commitOnAbandon: scenario == "commit race"}
			request := workerapi.ComputerSaveBeginRequest{OrgID: uuid.NewV7().String(), WorkspaceMountID: uuid.NewV7().String(), SaveID: uuid.NewV7().String(), Sequence: 1}
			operation, err := startComputerSave(t.Context(), client, f, request, f.runtime, f.computer, func(context.Context) (computerSaveCapture, error) { return saveHostCapture{f}, nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := operation.Wait(t.Context()); err == nil {
				t.Fatal("expected lost commit reply")
			}
			if scenario == "lost abandon" || scenario == "commit race" {
				if scenario == "lost abandon" {
					f.fail = "abandon"
				}
				if err := operation.Quiesce(t.Context()); err == nil || operation.released {
					t.Fatal("inferred absence without acknowledgement")
				}
			}
			if err := operation.Quiesce(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !operation.released {
				t.Fatal("capture not released after settlement")
			}
			if operation.committed != client.published {
				t.Fatal("publication outcome changed")
			}
			if scenario == "committed" {
				for _, step := range f.steps {
					if step == "abandon" {
						t.Fatal("abandoned committed result")
					}
				}
			}
		})
	}
}
