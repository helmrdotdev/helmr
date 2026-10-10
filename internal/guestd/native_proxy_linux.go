//go:build linux

package guestd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const nativeProxyArg = "__helmr-native-proxy"
const nativeLaunchPacketLimit = 128 * 1024
const nativeLaunchTimeout = 30 * time.Second

type nativeLaunchRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Cwd     string   `json:"cwd"`
	Env     []string `json:"env"`
}

type nativeLaunchReply struct {
	ScopeID string `json:"scopeId,omitempty"`
	// ProcessID is an opaque guest diagnostic, never a PID the caller may signal.
	ProcessID int    `json:"processId,omitempty"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Error     string `json:"error,omitempty"`
}

func init() {
	if len(os.Args) > 1 && os.Args[1] == nativeProxyArg {
		code, err := runNativeProxy(os.Args[2:])
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "helmr native launch: %v\n", err)
			os.Exit(127)
		}
		os.Exit(code)
	}
}

// The proxy owns no containment authority. It transfers its standard streams
// to the guest and keeps the connection alive until the actual process exits.
// Its PID belongs to the proxy; the private descriptor carries native identity.
func runNativeProxy(args []string) (int, error) {
	var metadataStat unix.Stat_t
	if err := unix.Fstat(3, &metadataStat); err != nil || (metadataStat.Mode&unix.S_IFMT != unix.S_IFIFO && metadataStat.Mode&unix.S_IFMT != unix.S_IFSOCK) {
		return 0, errors.New("native identity pipe is missing")
	}
	metadata := os.NewFile(3, "native identity")
	defer metadata.Close()

	if len(args) == 0 {
		return 0, errors.New("native command is required")
	}
	command, err := exec.LookPath(args[0])
	if err != nil {
		return 0, err
	}
	command, err = filepath.Abs(command)
	if err != nil {
		return 0, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 0, err
	}
	request, err := json.Marshal(nativeLaunchRequest{Command: command, Args: args[1:], Cwd: cwd, Env: os.Environ()})
	if err != nil {
		return 0, err
	}
	if len(request) > nativeLaunchPacketLimit {
		return 0, errors.New("native launch request exceeds its bound")
	}
	connection, err := net.DialUnix("unixpacket", nil, &net.UnixAddr{Name: nativeLauncherSocket, Net: "unixpacket"})
	if err != nil {
		return 0, err
	}
	defer connection.Close()
	// Signals and abrupt proxy death both close the connection. The guest owns
	// stopping and joining the entire native scope, including detached children.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(signals)
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-signals:
			_ = connection.Close()
		case <-finished:
		}
	}()
	if err := connection.SetDeadline(time.Now().Add(nativeLaunchTimeout)); err != nil {
		return 0, err
	}
	n, oob, err := connection.WriteMsgUnix(request, unix.UnixRights(0, 1, 2), nil)
	if err != nil {
		return 0, err
	}
	if n != len(request) || oob != len(unix.UnixRights(0, 1, 2)) {
		return 0, errors.New("incomplete native launch request")
	}
	reply, err := receiveNativeLaunchReply(connection)
	if err != nil {
		return 0, err
	}
	if reply.Error != "" {
		return 0, errors.New(reply.Error)
	}
	if reply.ScopeID == "" || reply.ProcessID <= 0 || reply.ExitCode != nil {
		return 0, errors.New("invalid native launch identity")
	}
	encodeErr := json.NewEncoder(metadata).Encode(reply)
	closeErr := metadata.Close()
	if err := errors.Join(encodeErr, closeErr); err != nil {
		return 0, err
	}
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return 0, err
	}
	reply, err = receiveNativeLaunchReply(connection)
	if err != nil {
		return 0, err
	}
	if reply.Error != "" {
		return 0, errors.New(reply.Error)
	}
	if reply.ExitCode == nil || *reply.ExitCode < 0 || *reply.ExitCode > 255 {
		return 0, errors.New("invalid native exit receipt")
	}
	return *reply.ExitCode, nil
}

func receiveNativeLaunchReply(connection *net.UnixConn) (nativeLaunchReply, error) {
	var reply nativeLaunchReply
	data := make([]byte, 64*1024)
	ancillary := make([]byte, unix.CmsgSpace(16*4))
	n, oob, flags, _, err := connection.ReadMsgUnix(data, ancillary)
	if err != nil {
		return reply, err
	}
	// No privileged descriptor is ever part of a reply. Close any unexpected
	// rights before failing, including descriptors received with truncated data.
	descriptors, rightsErr := nativeMessageDescriptors(ancillary[:oob])
	for _, fd := range descriptors {
		_ = unix.Close(fd)
	}
	if rightsErr != nil || len(descriptors) != 0 || flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		return reply, errors.New("invalid native reply control data")
	}
	if n == 0 {
		return reply, errors.New("native launcher connection ended")
	}
	if err := json.Unmarshal(data[:n], &reply); err != nil {
		return reply, err
	}
	return reply, nil
}

func nativeMessageDescriptors(ancillary []byte) ([]int, error) {
	messages, err := unix.ParseSocketControlMessage(ancillary)
	if err != nil {
		return nil, err
	}
	var descriptors []int
	var controlErr error
	for _, message := range messages {
		if message.Header.Level != unix.SOL_SOCKET || message.Header.Type != unix.SCM_RIGHTS {
			controlErr = errors.New("unexpected native launch control message")
			continue
		}
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			controlErr = errors.Join(controlErr, err)
			continue
		}
		for _, fd := range fds {
			unix.CloseOnExec(fd)
		}
		descriptors = append(descriptors, fds...)
	}
	return descriptors, controlErr
}
