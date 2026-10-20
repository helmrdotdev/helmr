//go:build linux

package bootgate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unsafe"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
)

// Run waits before Computer/Program mounts, networking, and the first boot_id
// read. A probe closes completely before the host pauses the builder. Only a
// clone receives Resume, with fresh host entropy and wall clock.
func Run() error {
	listener, err := vsock.Listen(Port, nil)
	if err != nil {
		return err
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		address, ok := conn.RemoteAddr().(*vsock.Addr)
		if !ok || address.ContextID != vsock.Host {
			conn.Close()
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		var operation [1]byte
		_, err = io.ReadFull(conn, operation[:])
		if err != nil {
			conn.Close()
			continue
		}
		if operation[0] == Probe {
			_, writeErr := io.WriteString(conn, Protocol)
			closeErr := conn.Close()
			if err := errors.Join(writeErr, closeErr); err != nil {
				return err
			}
			continue
		}
		if operation[0] != Resume {
			conn.Close()
			return errors.New("invalid bootstrap operation")
		}
		request, err := ReadRequest(conn)
		if err != nil {
			conn.Close()
			return err
		}
		receipt, err := initializeClone(request)
		clear(request.Entropy[:])
		if err == nil {
			_, err = conn.Write(receipt[:])
		}
		// A lost resume reply never returns to a reusable waiting state.
		return errors.Join(err, conn.Close())
	}
}

func initializeClone(request Request) ([ReceiptBytes]byte, error) {
	var receipt [ReceiptBytes]byte
	pool := struct {
		EntropyBits int32
		Size        int32
		Bytes       [EntropyBytes]byte
	}{EntropyBits: EntropyBytes * 8, Size: EntropyBytes, Bytes: request.Entropy}
	defer clear(pool.Bytes[:])
	defer clear(request.Entropy[:])
	random, err := os.OpenFile("/dev/urandom", os.O_RDWR, 0)
	if err != nil {
		return receipt, err
	}
	defer random.Close()
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, random.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&pool))); errno != 0 {
		return receipt, fmt.Errorf("add fresh bootstrap entropy: %w", errno)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, random.Fd(), unix.RNDRESEEDCRNG, 0); errno != 0 {
		return receipt, fmt.Errorf("reseed bootstrap randomness: %w", errno)
	}
	clock := unix.NsecToTimespec(request.Time.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &clock); err != nil {
		return receipt, fmt.Errorf("set bootstrap clock: %w", err)
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return receipt, err
	}
	id := strings.TrimSpace(string(bootID))
	if len(id) != 36 {
		return receipt, errors.New("invalid kernel boot identity")
	}
	copy(receipt[:ChallengeBytes], request.Challenge[:])
	copy(receipt[ChallengeBytes:], id)
	// Read the kernel directly after reseeding; the bootstrap process's own
	// pre-snapshot userspace randomness is never an application entropy source.
	if _, err := io.ReadFull(random, receipt[ChallengeBytes+36:]); err != nil {
		return receipt, err
	}
	return receipt, nil
}
