package computerhost

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type admissionSaveClient struct {
	*saveHostFixture
	requests []workerapi.ComputerSaveBeginRequest
	errors   []error
	first    func(context.Context) error
}

func (c *admissionSaveClient) BeginComputerSave(ctx context.Context, r workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
	c.requests = append(c.requests, r)
	if len(c.requests) == 1 && c.first != nil {
		if err := c.first(ctx); err != nil {
			return workerapi.ComputerSaveBeginResponse{}, err
		}
	}
	if len(c.errors) > 0 {
		err := c.errors[0]
		c.errors = c.errors[1:]
		if err != nil {
			return workerapi.ComputerSaveBeginResponse{}, err
		}
	}
	return c.saveHostFixture.BeginComputerSave(ctx, r)
}

func startAdmissionSave(t *testing.T, ctx context.Context, failures ...error) (*admissionSaveClient, *computerSave) {
	t.Helper()
	f := &saveHostFixture{instance: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	c := &admissionSaveClient{saveHostFixture: f, errors: failures}
	r := workerapi.ComputerSaveBeginRequest{EnvironmentID: uuid.NewV7().String(), ComputerInstanceID: f.instance, WriterGeneration: 2, SaveID: uuid.NewV7().String(), Sequence: 1}
	s, err := startComputerSave(ctx, c, f, r, f.instance, f.computer, func(context.Context) (computerSaveCapture, error) {
		if err := f.step("capture"); err != nil {
			return nil, err
		}
		return saveHostCapture{f}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestComputerSaveAdmissionRetriesExactOperationBeforeCapture(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, s := startAdmissionSave(t, t.Context(), io.EOF, &httpclient.Error{StatusCode: http.StatusServiceUnavailable},
			&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED})
		if err := s.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(c.requests) != 4 {
			t.Fatalf("admission attempts: %d", len(c.requests))
		}
		for _, r := range c.requests {
			if r != s.request {
				t.Fatal("retry changed save identity or writer")
			}
		}
		if !reflect.DeepEqual(c.steps, []string{"begin", "capture", "objects", "commit", "adopt", "ack", "release"}) {
			t.Fatalf("capture or publication repeated: %v", c.steps)
		}
	})
}

func TestComputerSaveAdmissionBoundsUnansweredAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &saveHostFixture{instance: uuid.NewV7().String(), computer: uuid.NewV7().String()}
		c := &admissionSaveClient{saveHostFixture: f, first: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		r := workerapi.ComputerSaveBeginRequest{EnvironmentID: uuid.NewV7().String(), ComputerInstanceID: f.instance, WriterGeneration: 2, SaveID: uuid.NewV7().String(), Sequence: 1}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		started := time.Now()
		s, err := startComputerSave(ctx, c, f, r, f.instance, f.computer, func(context.Context) (computerSaveCapture, error) {
			return saveHostCapture{f}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(c.requests) != 2 || time.Since(started) >= time.Minute || c.requests[0] != c.requests[1] {
			t.Fatal("unanswered admission did not retry within writer lifetime")
		}
	})
}

func TestComputerSaveAdmissionUncertainConflictRetainsOriginalSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, s := startAdmissionSave(t, t.Context(), io.ErrUnexpectedEOF, &httpclient.Error{StatusCode: http.StatusConflict})
		if err := s.Wait(t.Context()); !httpclient.IsStatus(err, http.StatusConflict) {
			t.Fatalf("expected uncertain admission conflict: %v", err)
		}
		if s.rejected || s.released || len(c.requests) != 2 {
			t.Fatalf("uncertain slot discarded: rejected=%v released=%v calls=%d", s.rejected, s.released, len(c.requests))
		}
		if err := s.Quiesce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.steps, []string{"begin", "abandon"}) || len(c.requests) != 3 {
			t.Fatalf("uncertain admission was not reconciled without capture: %v", c.steps)
		}
		for _, r := range c.requests {
			if r != s.request {
				t.Fatal("reconciliation changed save identity")
			}
		}
	})
}

func TestComputerSaveAdmissionRetryEndsWithWriterAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		c, s := startAdmissionSave(t, ctx, io.EOF)
		if err := s.Wait(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("writer expiry did not stop admission: %v", err)
		}
		if len(c.requests) != 1 || len(c.steps) != 0 || s.rejected || s.released {
			t.Fatal("expired writer captured or discarded uncertain admission")
		}
		if err := s.Quiesce(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.steps, []string{"begin", "abandon"}) {
			t.Fatalf("cleanup did not settle exact pending admission: %v", c.steps)
		}
	})
}

func TestComputerSaveAdmissionDoesNotRetryDefinitiveFailure(t *testing.T) {
	for _, failure := range []error{&httpclient.Error{StatusCode: http.StatusForbidden}, errors.New("invalid response")} {
		c, s := startAdmissionSave(t, t.Context(), failure)
		if err := s.Wait(t.Context()); !errors.Is(err, failure) {
			t.Fatalf("definitive failure changed: %v", err)
		}
		if len(c.requests) != 1 || len(c.steps) != 0 {
			t.Fatal("definitive failure retried or captured")
		}
	}
}

func TestInstanceComputerSaveLoopQuiescesAdmissionRetryWithoutAnotherSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &saveHostFixture{instance: uuid.NewV7().String(), computer: uuid.NewV7().String()}
		c := &admissionSaveClient{saveHostFixture: f, errors: []error{io.EOF}}
		s := &instanceComputerSaves{}
		if err := s.bind(workerapi.ComputerSaveBeginRequest{EnvironmentID: uuid.NewV7().String(), ComputerInstanceID: f.instance, WriterGeneration: 2}, f.computer); err != nil {
			t.Fatal(err)
		}
		result, err := s.run(t.Context(), time.Millisecond, c, f, func(context.Context) (computerSaveCapture, error) {
			t.Error("captured while admission was unavailable or quiescing")
			return saveHostCapture{f}, nil
		}, func(err error) { t.Errorf("quiescing retry failed the live owner: %v", err) })
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()
		if len(c.requests) != 1 {
			t.Fatal("periodic ticks replaced a retrying admission")
		}
		if err := s.Quiesce(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-result
		if len(c.requests) != 2 || c.requests[0] != c.requests[1] || !s.joined() ||
			!reflect.DeepEqual(c.steps, []string{"begin", "abandon"}) {
			t.Fatalf("quiescence did not join and settle the original operation: %v", c.steps)
		}
	})
}
