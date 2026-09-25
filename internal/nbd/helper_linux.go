//go:build linux

package nbd

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	setSock    = 0xab00
	setBlock   = 0xab01
	setSize    = 0xab02
	doIt       = 0xab03
	clearSock  = 0xab04
	disconnect = 0xab08
	setTimeout = 0xab09
	setFlags   = 0xab0a
)

func ioctl(fd int, op int, value uintptr) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(op), value)
	if e != 0 {
		return e
	}
	return nil
}

type claim struct {
	driverTID   int
	configured  chan error
	cfg         Config
	file        *os.File
	device      string
	running     chan error
	stopped     bool
	rdev        uint64
	boot, start string
}

// Helper is dispatched only by the qualification executable. Its control FD is
// inherited, never publicly listening. EOF quarantines a claim until an operator
// proves its consumer absent; it is not permission to disconnect a live VMM.
func Helper(arena string) error {
	raw, err := os.ReadFile(filepath.Join(arena, "config.json"))
	if err != nil {
		return err
	}
	var cfg Config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	control := os.NewFile(3, "control")
	if control == nil {
		return errors.New("control descriptor missing")
	}
	defer control.Close()
	c := &claim{cfg: cfg}
	c.boot, err = readText("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	c.start, err = readText("/proc/self/stat")
	if err != nil {
		return err
	}
	c.configured = make(chan error, 1)
	c.running = make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := c.attach()
		c.configured <- err
		c.running <- err
	}()
	claimErr := <-c.configured
	if claimErr == nil {
		claimErr = errors.New("driver readiness unproven")
		for i := 0; i < 100; i++ {
			if err := c.alive(); err == nil {
				claimErr = boundQueue(c.device)
				if claimErr == nil {
					claimErr = c.journal("ready")
				}
				break
			}
			claimErr = c.alive()
			time.Sleep(10 * time.Millisecond)
		}
	}
	dec, enc := json.NewDecoder(control), json.NewEncoder(control)
	for {
		var r request
		if err = dec.Decode(&r); err != nil {
			if c.file != nil {
				_ = c.journal("quarantined-control-lost")
				quarantine()
			}
			return err
		}
		out := response{ID: r.ID, Device: c.device}
		var opErr error
		switch r.Op {
		case "ready":
			opErr = claimErr
		case "expose":
			if claimErr != nil {
				opErr = claimErr
			} else {
				opErr = c.expose(r)
			}
		case "flush":
			if claimErr != nil {
				opErr = claimErr
			} else {
				opErr = c.alive()
				if opErr == nil {
					opErr = c.file.Sync()
				}
			}
		case "release":
			opErr = c.release()
		default:
			opErr = errors.New("unsupported control operation")
		}
		if opErr != nil {
			out.Error = opErr.Error()
		}
		if r.Op == "ready" {
			var rights []byte
			if opErr == nil {
				rights = unix.UnixRights(int(c.file.Fd()))
			}
			if _, err = unix.SendmsgN(int(control.Fd()), []byte{0}, rights, nil, 0); err != nil {
				quarantine()
			}
		}
		if err = enc.Encode(out); err != nil {
			if c.file != nil {
				_ = c.journal("quarantined-response-lost")
				quarantine()
			}
			return err
		}
		if r.Op == "release" && opErr == nil {
			return nil
		}
	}
}
func readText(path string) (string, error) {
	b, e := os.ReadFile(path)
	return strings.TrimSpace(string(b)), e
}
func (c *claim) journal(phase string) error {
	value := struct {
		Phase, Device, Boot, ProcessStat string
		PID                              int
		Size                             int64
	}{phase, c.device, c.boot, c.start, os.Getpid(), c.cfg.Size}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	path := filepath.Join(c.cfg.Arena, "claim.json")
	f, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, w := f.Write(raw)
	s := f.Sync()
	err = errors.Join(w, s, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(path+".tmp", path); err != nil {
		return err
	}
	dir, err := os.Open(c.cfg.Arena)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func (c *claim) attach() error {
	socket, err := os.Lstat(c.cfg.Socket)
	if err != nil {
		return err
	}
	if socket.Mode()&os.ModeSocket == 0 || socket.Mode().Perm()&0077 != 0 {
		return errors.New("private Unix socket required")
	}
	conn, err := net.DialTimeout("unix", c.cfg.Socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	var hello [18]byte
	if _, err = io.ReadFull(conn, hello[:]); err != nil {
		return err
	}
	if binary.BigEndian.Uint64(hello[:8]) != 0x4e42444d41474943 || binary.BigEndian.Uint64(hello[8:16]) != 0x49484156454f5054 || binary.BigEndian.Uint16(hello[16:])&3 != 3 {
		return errors.New("fixed-newstyle no-zeroes required")
	}
	var option [20]byte
	binary.BigEndian.PutUint32(option[:4], 3)
	binary.BigEndian.PutUint64(option[4:12], 0x49484156454f5054)
	binary.BigEndian.PutUint32(option[12:16], 1)
	if _, err = conn.Write(option[:]); err != nil {
		return err
	}
	var export [10]byte
	if _, err = io.ReadFull(conn, export[:]); err != nil {
		return err
	}
	flags := binary.BigEndian.Uint16(export[8:])
	if binary.BigEndian.Uint64(export[:8]) != uint64(c.cfg.Size) || flags&4 == 0 || flags & ^uint16(1|4|32) != 0 {
		return errors.New("unsupported export geometry or flags")
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	sock, err := conn.(*net.UnixConn).File()
	if err != nil {
		return err
	}
	defer sock.Close()
	for _, path := range c.cfg.Devices {
		if !validDevice(path) {
			return errors.New("invalid allowlist")
		}
		c.device = path
		if err = c.journal("claim-attempt"); err != nil {
			return err
		}
		fd, e := unix.Open(path, unix.O_RDWR|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(e, unix.EBUSY) {
			continue
		}
		if e != nil {
			return e
		}
		var st unix.Stat_t
		e = unix.Fstat(fd, &st)
		if e != nil || st.Mode&unix.S_IFMT != unix.S_IFBLK {
			unix.Close(fd)
			return errors.New("allowlisted path is not a block device")
		}
		e = ioctl(fd, setSock, sock.Fd())
		if e != nil {
			unix.Close(fd)
			if errors.Is(e, unix.EBUSY) {
				continue
			}
			return e
		}
		c.file = os.NewFile(uintptr(fd), path)
		c.rdev = uint64(st.Rdev)
		break
	}
	if c.file == nil {
		return errors.New("no available allowlisted NBD device")
	}
	if err = c.journal("claimed"); err != nil {
		return err
	}
	for _, v := range []struct {
		op  int
		arg uintptr
	}{{setBlock, 4096}, {setSize, uintptr(c.cfg.Size)}, {setFlags, uintptr(flags)}, {setTimeout, 10}} {
		if err = ioctl(int(c.file.Fd()), v.op, v.arg); err != nil {
			return err
		}
	}
	c.driverTID = unix.Gettid()
	c.configured <- nil
	return ioctl(int(c.file.Fd()), doIt, 0)
}
func (c *claim) alive() error {
	if c.file == nil || c.stopped {
		return errors.New("claim inactive")
	}
	pid, err := readText(filepath.Join("/sys/block", filepath.Base(c.device), "pid"))
	if err != nil || pid != strconv.Itoa(c.driverTID) {
		return fmt.Errorf("driver identity observed=%q expected=%d: %v", pid, c.driverTID, err)
	}
	var size uint64
	err = ioctl(int(c.file.Fd()), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&size)))
	if err != nil || size != uint64(c.cfg.Size) {
		return errors.New("device geometry changed")
	}
	return nil
}
func boundQueue(device string) error {
	root := filepath.Join("/sys/block", filepath.Base(device), "queue")
	for _, v := range []struct {
		name  string
		limit uint64
	}{{"max_sectors_kb", 1024}, {"discard_max_bytes", 1 << 20}} {
		path := filepath.Join(root, v.name)
		before, err := readText(path)
		if err != nil {
			return err
		}
		n, err := strconv.ParseUint(before, 10, 64)
		if err != nil || v.name == "max_sectors_kb" && n == 0 {
			return errors.New("invalid queue bound")
		}
		target := min(n, v.limit)
		if target != n {
			if err = os.WriteFile(path, []byte(strconv.FormatUint(target, 10)), 0600); err != nil {
				return err
			}
		}
		after, err := readText(path)
		if err != nil || after != strconv.FormatUint(target, 10) {
			return errors.New("queue readback mismatch")
		}
		fmt.Fprintf(os.Stderr, "%s %s: %s -> %s\n", device, v.name, before, after)
	}
	return nil
}
func (c *claim) expose(r request) error {
	if err := c.alive(); err != nil {
		return err
	}
	jail := filepath.Join(c.cfg.Arena, "jail")
	if r.Path != filepath.Join(jail, "computer.nbd") || r.UID < 0 || r.GID < 0 {
		return errors.New("invalid jail request")
	}
	if err := os.Mkdir(jail, 0711); err != nil {
		return err
	}
	// Arena remains private to its owner; the qualifier passes an open jail dir
	// to the exact unprivileged consumer rather than granting arena traversal.
	if err := unix.Mknod(r.Path, unix.S_IFBLK|0600, int(c.rdev)); err != nil {
		return err
	}
	if err := os.Chown(r.Path, r.UID, r.GID); err != nil {
		return err
	}
	var st unix.Stat_t
	if err := unix.Lstat(r.Path, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFBLK || uint64(st.Rdev) != c.rdev {
		return errors.New("jail device identity mismatch")
	}
	return c.journal("exposed")
}
func (c *claim) release() error {
	if c.file == nil {

		return c.journal("released-no-claim")
	}
	if c.running != nil && !c.stopped {
		if err := ioctl(int(c.file.Fd()), disconnect, 0); err != nil && !errors.Is(err, unix.ENOTCONN) && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.EIO) {
			return err
		}
		select {
		case <-c.running:
			c.stopped = true
		case <-time.After(10 * time.Second):
			return errors.New("driver exit unproven")
		}
	}
	if err := ioctl(int(c.file.Fd()), clearSock, 0); err != nil {
		return err
	}
	// The controller retains a duplicate exclusive file description. It closes
	// that final reference only after this helper has exited, then checks sysfs.
	if err := c.file.Close(); err != nil {
		return err
	}
	c.file = nil
	return c.journal("detached-controller-retains-claim")
}

// Keep an uncertain owner alive even after all device goroutines have stopped.
func quarantine() {
	for {
		time.Sleep(time.Hour)
	}
}
