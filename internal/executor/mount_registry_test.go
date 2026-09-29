package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

// testMounts stands in for the physical mount registry behind the Run side's
// MountRegistry. Each mount lends a Run a channel on a newly opened stream of
// its machine, releases the machine once as a checkpoint source, and hands
// failure requests to the test.
type testMounts struct {
	mu     sync.Mutex
	mounts map[string]*testMount
}

type testMount struct {
	machine  vm.Machine
	mount    workerapi.ComputerInstanceAssignment
	token    string
	failures chan testMountFailure

	releaseOnce sync.Once
	releaseErr  error
}

type testMountFailure struct {
	result chan error
}

func newTestMounts() *testMounts {
	return &testMounts{mounts: map[string]*testMount{}}
}

// add mounts machine for Runs of mount and returns it with its unmount.
func (m *testMounts) add(mount workerapi.ComputerInstanceAssignment, machine vm.Machine, channelToken string) (*testMount, func()) {
	mounted := &testMount{machine: machine, mount: mount, token: channelToken, failures: make(chan testMountFailure, 1)}
	m.mu.Lock()
	m.mounts[mount.ComputerInstanceID] = mounted
	m.mu.Unlock()
	return mounted, func() {
		m.mu.Lock()
		if m.mounts[mount.ComputerInstanceID] == mounted {
			delete(m.mounts, mount.ComputerInstanceID)
		}
		m.mu.Unlock()
	}
}

func (m *testMounts) lookup(id string) (*testMount, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mounted := m.mounts[id]
	if mounted == nil {
		return nil, fmt.Errorf("%w: %s", ErrMountNotFound, id)
	}
	return mounted, nil
}

func (m *testMounts) OpenChannel(ctx context.Context, id string) (MountChannel, error) {
	mounted, err := m.lookup(id)
	if err != nil {
		return MountChannel{}, err
	}
	stream, err := mounted.machine.OpenStream(ctx)
	if err != nil {
		return MountChannel{}, err
	}
	return MountChannel{
		Channel:       &testChannel{machine: mounted.machine, stream: stream},
		ReleaseSource: mounted.release,
		GrantProgramResume: func(context.Context, *computerv0.GrantProgramResumeRequest) (*programv0.ResumeAttach, error) {
			return nil, errors.New("test mount does not grant Program resume")
		},
		ChannelToken: mounted.token,
		Mount:        mounted.mount,
	}, nil
}

func (m *testMounts) RequestFailure(ctx context.Context, id string) error {
	mounted, err := m.lookup(id)
	if err != nil {
		return err
	}
	request := testMountFailure{result: make(chan error, 1)}
	select {
	case mounted.failures <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RenewComputerAuthority accepts the requested expiry for the mount's channel
// token, as a guest does, unless ctx has ended.
func (m *testMounts) RenewComputerAuthority(ctx context.Context, request *computerv0.RenewComputerAuthorityRequest) (*computerv0.ComputerAuthorityFence, error) {
	fence := request.GetPrevious().GetFence()
	mounted, err := m.lookup(fence.GetComputerInstanceId())
	if err != nil {
		return nil, err
	}
	if request.GetPrevious().GetChannelToken() != mounted.token {
		return nil, errors.New("test mount channel token differs")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	renewed := proto.Clone(fence).(*computerv0.ComputerAuthorityFence)
	renewed.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	return renewed, nil
}

func (m *testMount) release(ctx context.Context) error {
	m.releaseOnce.Do(func() { m.releaseErr = m.machine.Close(ctx) })
	return m.releaseErr
}

// testChannel is a Run's borrowed stream on a test mount. Closing it closes
// only that stream.
type testChannel struct {
	machine vm.Machine
	stream  vm.Stream
	once    sync.Once
	err     error
}

func (c *testChannel) Stream() vm.Stream { return c.stream }

func (c *testChannel) OpenStream(context.Context) (vm.Stream, error) {
	return nil, errors.New("test channel does not open nested streams")
}

func (c *testChannel) Wait(ctx context.Context) error { return c.machine.Wait(ctx) }

func (c *testChannel) Close(context.Context) error {
	c.once.Do(func() { c.err = c.stream.Close() })
	return c.err
}
