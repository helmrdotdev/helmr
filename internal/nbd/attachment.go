//go:build linux || darwin

// Package nbd owns exclusive Linux NBD claims. Both-owner death requires
// external reconciliation before new admission; journals are evidence, not an
// orphan reclaimer. Never infer permission to reuse a device from PID absence.
package nbd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Config is deliberately restricted to an operator-owned private arena and an
// explicit disposable device allowlist. Helper must dispatch Helper on --nbd-helper.
type Config struct {
	Helper, Socket, Arena string
	Devices               []string
	Size                  int64
}

type Attachment struct {
	owned          *os.File
	ready, closing bool
	decoder        *json.Decoder
	mu             sync.Mutex
	conn           net.Conn
	cmd            *exec.Cmd
	done           chan struct{}
	waitErr        error
	seq            uint64
	released       bool
	consumerExit   <-chan struct{}
	device         string // diagnostic identity, never authority to reclaim a device
	Arena          string
}

type request struct {
	ID       uint64
	Op, Path string
	UID, GID int
}
type response struct {
	ID            uint64
	Device, Error string
}

// Claim returns the attachment even on uncertain startup so its arena and helper
// remain attributable. Never remove Arena after an error without proven release.
func Claim(ctx context.Context, cfg Config) (*Attachment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Lstat(cfg.Arena)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("private existing 0700 arena required")
	}
	if !filepath.IsAbs(cfg.Arena) || filepath.Dir(cfg.Socket) != cfg.Arena || !filepath.IsAbs(cfg.Socket) || cfg.Size <= 0 || cfg.Size%4096 != 0 || len(cfg.Devices) == 0 {
		return nil, errors.New("socket, geometry and disposable devices required")
	}
	for _, d := range cfg.Devices {
		if !validDevice(d) {
			return nil, fmt.Errorf("invalid NBD device %q", d)
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	configFile, err := os.OpenFile(filepath.Join(cfg.Arena, "config.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := configFile.Write(raw)
	if err = errors.Join(writeErr, configFile.Sync(), configFile.Close()); err != nil {
		return nil, err
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(pair[0])
	unix.CloseOnExec(pair[1])
	parent, child := os.NewFile(uintptr(pair[0]), "nbd-control"), os.NewFile(uintptr(pair[1]), "nbd-helper-control")
	defer child.Close()
	conn, err := net.FileConn(parent)
	parent.Close()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(cfg.Helper, "--nbd-helper", cfg.Arena)
	cmd.ExtraFiles = []*os.File{child}
	cmd.Stderr = os.Stderr
	a := &Attachment{decoder: json.NewDecoder(conn), conn: conn, cmd: cmd, done: make(chan struct{}), Arena: cfg.Arena}
	if err = ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		conn.Close()
		return nil, err
	}
	go func() { a.waitErr = cmd.Wait(); close(a.done) }()
	a.mu.Lock()
	defer a.mu.Unlock()
	r, err := a.call(ctx, request{Op: "ready"})
	a.device = r.Device
	if err != nil {
		return a, fmt.Errorf("NBD claim uncertain; retain %s: %w", cfg.Arena, err)
	}
	if a.owned == nil {
		return a, errors.New("helper omitted exclusive claim descriptor")
	}
	a.ready = true
	return a, nil
}

func (a *Attachment) call(ctx context.Context, r request) (response, error) {
	a.seq++
	r.ID = a.seq
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := ctx.Err(); err != nil {
		return response{}, err
	}
	if err := a.conn.SetDeadline(deadline); err != nil {
		return response{}, err
	}
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() { _ = a.conn.SetDeadline(time.Now()); close(cancelDone) })
	defer func() {
		if !stopCancel() {
			<-cancelDone
		}
	}()
	if err := json.NewEncoder(a.conn).Encode(r); err != nil {
		return response{}, err
	}
	if r.Op == "ready" {
		byteBuf, oob := make([]byte, 1), make([]byte, unix.CmsgSpace(4))
		n, oobn, flags, _, err := a.conn.(*net.UnixConn).ReadMsgUnix(byteBuf, oob)
		if err != nil {
			return response{}, err
		}
		if n != 1 || flags&unix.MSG_CTRUNC != 0 {
			return response{}, errors.New("invalid claim descriptor transfer")
		}
		messages, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			return response{}, err
		}
		var fds []int
		for _, m := range messages {
			rights, e := unix.ParseUnixRights(&m)
			if e != nil {
				return response{}, e
			}
			fds = append(fds, rights...)
		}
		if len(fds) > 1 {
			for _, fd := range fds {
				unix.Close(fd)
			}
			return response{}, errors.New("unexpected claim descriptors")
		}
		if len(fds) == 1 {
			unix.CloseOnExec(fds[0])
			a.owned = os.NewFile(uintptr(fds[0]), "owned-nbd")
		}
	}
	dec := a.decoder
	for {
		var out response
		if err := dec.Decode(&out); err != nil {
			return out, err
		}
		if out.ID < r.ID {
			continue
		}
		if out.ID != r.ID {
			return out, errors.New("control identity mismatch")
		}
		if out.Error != "" {
			return out, errors.New(out.Error)
		}
		return out, nil
	}
}

// ExposeInto creates only computer.nbd beneath the private arena's jail. It
// never modifies the global device node. The caller supplies the VMM credentials.
func (a *Attachment) ExposeInto(ctx context.Context, uid, gid int) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released || !a.ready || a.closing {
		return "", errors.New("attachment released")
	}
	path := filepath.Join(a.Arena, "jail", "computer.nbd")
	_, err := a.call(ctx, request{Op: "expose", Path: path, UID: uid, GID: gid})
	return path, err
}
func (a *Attachment) Flush(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released || !a.ready || a.closing {
		return errors.New("attachment released")
	}
	_, err := a.call(ctx, request{Op: "flush"})
	return err
}

// BindConsumer binds the exact consumer owner's exit proof before it can open
// the device. The owner closes exited only after its process and all delegated
// device users are proven absent (or launch was never attempted). It owns launch,
// stop and wait; the attachment must not infer exit from a PID or a stop request.
// Failed/ambiguous launch still requires the bound owner's proof before release.
func (a *Attachment) BindConsumer(exited <-chan struct{}) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if exited == nil || a.released || !a.ready || a.closing || a.consumerExit != nil {
		return errors.New("consumer ownership unavailable")
	}
	a.consumerExit = exited
	return nil
}

// Release requires affirmative bound-consumer exit before disconnect. Timeout or
// an ambiguous helper result keeps the attachment unreleased and its arena intact.
// In particular no PID/device-name recovery or optimistic SIGKILL proof is used.
func (a *Attachment) Release(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.closing = true
	if a.consumerExit != nil {
		select {
		case <-a.consumerExit:
		case <-ctx.Done():
			return fmt.Errorf("consumer exit unproven: %w", ctx.Err())
		}
	}

	if _, err := a.call(ctx, request{Op: "release"}); err != nil {
		return fmt.Errorf("cleanup unproven; retain %s: %w", a.Arena, err)
	}
	select {
	case <-a.done:
		if a.waitErr != nil {
			return a.waitErr
		}
	case <-ctx.Done():
		return fmt.Errorf("helper exit unproven: %w", ctx.Err())
	}
	if a.owned != nil {
		if err := a.owned.Close(); err != nil {
			return err
		}
		a.owned = nil
	}
	if a.ready {
		for {
			_, err := os.Stat(filepath.Join("/sys/block", filepath.Base(a.device), "pid"))
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			select {
			case <-ctx.Done():
				return errors.New("device inactivity unproven after descriptor close")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	a.released = true
	return a.conn.Close()
}

func validDevice(path string) bool {
	suffix := strings.TrimPrefix(path, "/dev/nbd")
	n, err := strconv.Atoi(suffix)
	return err == nil && n >= 0 && path == fmt.Sprintf("/dev/nbd%d", n)
}

// Device returns diagnostic identity; it does not authorize independent cleanup.
func (a *Attachment) Device() string { return a.device }
