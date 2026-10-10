//go:build linux

package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// These resources belong to the Session process epoch, not a Turn or host
// attachment. A healthy RAM capture retains this object and its local pipes.
// The supervisor owns admission/authority; this owner handles physical lifetime.
type agentProcess struct {
	program     *agentProgramLease
	identity    *agentv1.SessionIdentity
	cmd         *exec.Cmd
	group       *linuxProcessCgroup
	launcher    *nativeLauncher
	rootFD      int
	cancel      context.CancelFunc
	stdin       *os.File
	events      *os.File
	stdout      *os.File
	stderr      *os.File
	proof       *os.File
	inherited   []*os.File
	cleanup     func()
	writeMu     sync.Mutex
	lifecycleMu sync.Mutex
	started     bool
	closed      bool
	waitDone    chan struct{}
	waitErr     error
	brokerDone  chan error
}

func newAgentProcess(ctx context.Context, entry *computerMountEntry, options agentProcessOptions) (_ *agentProcess, err error) {
	// Both executable owners consume and clear the delivered secret values.
	secrets := options.Secrets
	defer clearProgramSecretValues(secrets)
	if entry == nil || entry.runtimeUser == nil || options.Identity == nil || options.Program == (programMounts{}) {
		return nil, errors.New("session process requires its mounted Computer and identity")
	}
	if filepath.Clean(entry.computerMount) != defaultRuntimeWorkdir {
		return nil, errors.New("computer durable root must be /workspace")
	}
	group, err := createSessionProcessCgroup(options.Identity.GetSessionId(), options.Identity.GetProcessEpoch())
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(ctx)
	process := &agentProcess{program: options.ProgramLease, identity: proto.Clone(options.Identity).(*agentv1.SessionIdentity), group: group, rootFD: -1, cancel: cancel, waitDone: make(chan struct{}), cleanup: func() {}}
	// No process has started on this path. Preserve the error and close only the
	// exclusively created scope; duplicate startup never reaches this cleanup.
	defer func() {
		if err == nil {
			return
		}
		cancel()
		closeAgentFiles(process.inherited...)
		closeAgentFiles(process.stdin, process.events, process.stdout, process.stderr, process.proof)
		if process.launcher != nil {
			stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			err = errors.Join(err, process.launcher.close(stopCtx))
			stop()
		}
		err = errors.Join(err, group.close())
		process.cleanup()
	}()
	if err = prepareLaunchPath(entry.imageRoot, defaultRuntimeWorkdir, entry.runtimeUser); err != nil {
		return nil, err
	}
	env := managedRuntimeEnv(entry.imageConfig, entry.runtimeUser, defaultRuntimeWorkdir)
	flags, err := managedProgramNodeFlags(options.Program.Runtime)
	if err != nil {
		return nil, err
	}
	secretRoot, cleanupSecrets, err := stageProgramSecrets(entry.imageRoot, secrets, entry.runtimeUser, &env)
	if err != nil {
		return nil, err
	}
	process.cleanup = cleanupSecrets
	if err = stageProtectedEnv(entry.imageRoot, options.ProtectedEnv, options.ProxyCA, &env); err != nil {
		return nil, err
	}
	cleanupRuntime, err := mountImageRuntimeFilesystems(entry.imageRoot)
	if err != nil {
		return nil, err
	}
	process.cleanup = func() { cleanupRuntime(); cleanupSecrets() }
	process.launcher, err = newNativeLauncher(group, entry.imageRoot, entry.runtimeUser, secretRoot, options.Program)
	if err != nil {
		return nil, err
	}
	process.cmd, err = imageCommand(lifetime, managedProgramNode, append(flags, managedProgramEntry), defaultRuntimeWorkdir, env, entry.imageRoot, entry.runtimeUser, imageCommandOptions{
		Program: options.Program, SecretRoot: secretRoot, NativeRoot: process.launcher.directory,
		CgroupNamespace: true, CgroupLeaf: filepath.Base(group.path), StartProof: true,
	})
	if err != nil {
		return nil, err
	}
	process.cmd.SysProcAttr.PidFD = &process.rootFD
	process.cmd.Cancel = func() error { return killNativeProcess(group.kill, process.rootFD) }
	if err = group.attach(process.cmd); err != nil {
		return nil, err
	}
	// Own all pipe ends explicitly: exec.Wait must not close a log/control reader
	// while the supervisor is draining its final bytes.
	input, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	process.stdin = writer
	process.inherited = append(process.inherited, input)
	process.cmd.Stdin = input
	outputs := []**os.File{&process.stdout, &process.stderr, &process.events, &process.proof}
	writers := make([]*os.File, 0, len(outputs))
	for _, output := range outputs {
		reader, writer, pipeErr := os.Pipe()
		if pipeErr != nil {
			return nil, pipeErr
		}
		*output = reader
		process.inherited = append(process.inherited, writer)
		writers = append(writers, writer)
	}
	process.cmd.Stdout, process.cmd.Stderr = writers[0], writers[1]
	process.cmd.ExtraFiles = []*os.File{writers[2], writers[3]}
	return process, nil
}

// start precedes any authored command. Only setup can create retained native
// scopes; accepting SessionReady seals admission for the rest of this epoch.
func (process *agentProcess) start(ctx context.Context) error {
	process.lifecycleMu.Lock()
	defer process.lifecycleMu.Unlock()
	if process.started || process.closed {
		return errors.New("session process already started or closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := process.launcher.setAdmission(true); err != nil {
		return err
	}
	if err := process.cmd.Start(); err != nil {
		_ = process.launcher.setAdmission(false)
		return err
	}
	process.started = true
	process.cmd.Env = nil
	closeAgentFiles(process.inherited...)
	process.inherited = nil
	process.brokerDone = make(chan error, 1)
	go func() { process.brokerDone <- process.launcher.run(context.Background()) }()
	go func() { process.waitErr = process.cmd.Wait(); close(process.waitDone) }()
	deadline := time.Now().Add(10 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if err := process.proof.SetReadDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = process.proof.SetReadDeadline(time.Now()) })
	defer stop()
	body, err := io.ReadAll(io.LimitReader(process.proof, 64*1024+1))
	if err != nil {
		return fmt.Errorf("read Session exec proof: %w", err)
	}
	if len(body) != 0 {
		return errors.New("session process initialization failed")
	}
	return process.proof.Close()
}

func (process *agentProcess) write(ctx context.Context, command *agentv1.GuestCommand) error {
	process.writeMu.Lock()
	defer process.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if err := process.stdin.SetWriteDeadline(deadline); err != nil {
		return err
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = process.stdin.SetWriteDeadline(time.Now()); close(interrupted) })
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()
	return writeAgentCommand(process.stdin, process.identity, command)
}

func (process *agentProcess) read() (*agentv1.ProgramEvent, error) {
	event, err := readAgentEvent(process.events, process.identity)
	if err != nil {
		return nil, err
	}
	if event.GetReady() != nil {
		if err := process.launcher.setAdmission(false); err != nil {
			return nil, err
		}
	}
	return event, nil
}

func (process *agentProcess) converge(scopes []nativeScopeEvidence) error {
	process.lifecycleMu.Lock()
	defer process.lifecycleMu.Unlock()
	if !process.started || process.closed {
		return errors.New("session process is not running")
	}
	return process.launcher.verifySessionConverged(process.cmd.Process.Pid, process.rootFD, scopes)
}

// close may be retried after a failed physical join. Mounts and retained identity
// descriptors are released only after the entire Session scope is proved empty.
func (process *agentProcess) close(ctx context.Context) error {
	process.lifecycleMu.Lock()
	defer process.lifecycleMu.Unlock()
	if process.closed {
		return nil
	}
	process.cancel()
	_ = process.stdin.Close()
	killErr := killNativeProcess(process.group.kill, process.rootFD)
	nativeErr := process.launcher.close(ctx)
	if process.started {
		select {
		case <-process.waitDone:
		case <-ctx.Done():
			return errors.Join(killErr, nativeErr, ctx.Err())
		}
	}
	if err := process.group.waitEmpty(); err != nil {
		return errors.Join(killErr, nativeErr, err)
	}
	if nativeErr != nil {
		return nativeErr
	}
	if process.program != nil {
		if err := process.program.close(); err != nil {
			return err
		}
	}
	if err := process.group.close(); err != nil {
		return err
	}
	if process.brokerDone != nil {
		select {
		case err := <-process.brokerDone:
			if err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		process.brokerDone = nil
	}
	closeAgentFiles(process.inherited...)
	// The relay owns log readers through EOF after physical join.
	closeAgentFiles(process.events, process.proof)
	if process.rootFD >= 0 {
		_ = unix.Close(process.rootFD)
		process.rootFD = -1
	}
	process.cleanup()
	process.closed = true
	return nil
}

func closeAgentFiles(files ...*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}

func createAgentProcess(ctx context.Context, entry *computerMountEntry, options agentProcessOptions) (agentSessionProcess, error) {
	return newAgentProcess(ctx, entry, options)
}
func (process *agentProcess) logs() (io.Reader, io.Reader) { return process.stdout, process.stderr }
func (process *agentProcess) freeze(ctx context.Context) error {
	process.lifecycleMu.Lock()
	defer process.lifecycleMu.Unlock()
	if !process.started || process.closed {
		return errors.New("session process is not running")
	}
	return process.group.freeze(ctx)
}
func (process *agentProcess) verifyFrozen() error {
	process.lifecycleMu.Lock()
	defer process.lifecycleMu.Unlock()
	if !process.started || process.closed {
		return errors.Join(errComputerCaptureUnusable, errors.New("session process is not running"))
	}
	frozen, populated, err := process.group.state()
	if err != nil {
		return err
	}
	if !frozen || !populated {
		return errors.Join(errComputerCaptureUnusable, errors.New("session process scope is not frozen"))
	}
	return nil
}
func (process *agentProcess) thaw(ctx context.Context) error {
	process.lifecycleMu.Lock()
	defer process.lifecycleMu.Unlock()
	if !process.started || process.closed {
		return errors.New("session process is not running")
	}
	return process.group.thaw(ctx)
}

func (process *agentProcess) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-process.waitDone:
		return process.waitErr
	}
}
