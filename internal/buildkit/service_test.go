package buildkit

import (
	"context"
	"errors"
	"reflect"
	"testing"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/client/llb"
)

type fakeServiceController struct {
	states       []serviceState
	actions      []string
	cgroupEmpty  bool
	socketAbsent bool
	waitErr      error
}

func (c *fakeServiceController) Inspect(context.Context, string) (serviceState, error) {
	c.actions = append(c.actions, "inspect")
	if len(c.states) == 0 {
		return serviceState{}, errors.New("unexpected inspect")
	}
	state := c.states[0]
	c.states = c.states[1:]
	return state, nil
}

func (c *fakeServiceController) Stop(context.Context, string) error {
	c.actions = append(c.actions, "stop")
	return nil
}

func (c *fakeServiceController) Start(context.Context, string) error {
	c.actions = append(c.actions, "start")
	return nil
}

func (c *fakeServiceController) CgroupEmpty(string) (bool, error) {
	c.actions = append(c.actions, "cgroup")
	return c.cgroupEmpty, nil
}

func (c *fakeServiceController) SocketAbsent(string) (bool, error) {
	c.actions = append(c.actions, "absent")
	return c.socketAbsent, nil
}

func (c *fakeServiceController) WaitSocket(context.Context, string) error {
	c.actions = append(c.actions, "socket")
	return c.waitErr
}

type fakeDaemonClient struct {
	waitErr error
	closed  bool
}

func (*fakeDaemonClient) Solve(context.Context, *llb.Definition, bkclient.SolveOpt, chan *bkclient.SolveStatus) (*bkclient.SolveResponse, error) {
	return &bkclient.SolveResponse{}, nil
}

func (c *fakeDaemonClient) Wait(context.Context) error {
	return c.waitErr
}

func (c *fakeDaemonClient) Close() error {
	c.closed = true
	return nil
}

func freshController() *fakeServiceController {
	return &fakeServiceController{
		states: []serviceState{
			{ActiveState: "active", MainPID: 10, Invocation: "old", ControlGroup: "/system.slice/helmr-buildkit.service"},
			{ActiveState: "inactive", ControlGroup: "/system.slice/helmr-buildkit.service"},
			{ActiveState: "active", MainPID: 20, Invocation: "new", ControlGroup: "/system.slice/helmr-buildkit.service"},
		},
		cgroupEmpty:  true,
		socketAbsent: true,
	}
}

func TestOpenFreshProvesOldTreeAbsentBeforeOpeningNewClient(t *testing.T) {
	controller := freshController()
	client := &fakeDaemonClient{}
	opened := false

	builder, closeClient, err := openFresh(context.Background(), Config{Addr: "unix:///tmp/buildkit.sock"}, controller, func(_ context.Context, addr string) (daemonClient, error) {
		opened = true
		if addr != "unix:///tmp/buildkit.sock" {
			t.Fatalf("unexpected endpoint %q", addr)
		}
		return client, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"inspect", "stop", "inspect", "cgroup", "absent", "start", "socket", "inspect"}
	if !reflect.DeepEqual(controller.actions, want) {
		t.Fatalf("actions = %v, want %v", controller.actions, want)
	}
	if !opened || builder == nil || builder.health == nil {
		t.Fatal("expected generation-bound ready BuildKit client")
	}
	if err := closeClient(); err != nil {
		t.Fatal(err)
	}
	if !client.closed {
		t.Fatal("expected BuildKit client to close")
	}
}

func TestOpenFreshRejectsPopulatedOldCgroup(t *testing.T) {
	controller := freshController()
	controller.cgroupEmpty = false
	opened := false

	_, _, err := openFresh(context.Background(), Config{}, controller, func(context.Context, string) (daemonClient, error) {
		opened = true
		return &fakeDaemonClient{}, nil
	})
	if err == nil || opened {
		t.Fatalf("error = %v, opened = %t", err, opened)
	}
	if got := controller.actions; !reflect.DeepEqual(got, []string{"inspect", "stop", "inspect", "cgroup"}) {
		t.Fatalf("actions = %v", got)
	}
}

func TestOpenFreshRejectsUnchangedInvocation(t *testing.T) {
	controller := freshController()
	controller.states[2].Invocation = "old"
	client := &fakeDaemonClient{}

	_, _, err := openFresh(context.Background(), Config{}, controller, func(context.Context, string) (daemonClient, error) {
		return client, nil
	})
	if err == nil {
		t.Fatal("unchanged service invocation was accepted")
	}
	if client.closed {
		t.Fatal("client opened before invocation proof")
	}
}

func TestOpenFreshClosesClientWhenAPIIsNotReady(t *testing.T) {
	controller := freshController()
	waitErr := errors.New("not ready")
	client := &fakeDaemonClient{waitErr: waitErr}

	_, _, err := openFresh(context.Background(), Config{}, controller, func(context.Context, string) (daemonClient, error) {
		return client, nil
	})
	if !errors.Is(err, waitErr) {
		t.Fatalf("got %v, want readiness error", err)
	}
	if !client.closed {
		t.Fatal("client was not closed after readiness failure")
	}
}

func TestBuildKitSocketPathRejectsNonUnixEndpoint(t *testing.T) {
	if _, err := buildKitSocketPath("tcp://127.0.0.1:1234"); err == nil {
		t.Fatal("non-Unix BuildKit endpoint was accepted")
	}
}
