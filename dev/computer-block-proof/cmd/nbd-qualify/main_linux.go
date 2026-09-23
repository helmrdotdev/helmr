//go:build linux

// nbd-qualify exercises the owned device lifecycle, not production persistence.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/nbd"
	"golang.org/x/sys/unix"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) == 3 && os.Args[1] == "--nbd-helper" {
		return nbd.Helper(os.Args[2])
	}
	if len(os.Args) == 2 && (os.Args[1] == "--consumer" || os.Args[1] == "--consumer-hold") {
		return consume(os.Args[1] == "--consumer-hold")
	}

	if len(os.Args) == 4 && os.Args[1] == "--helper-death" && os.Getenv("HELMR_DISPOSABLE_NBD_PROOF") == "1" {
		return qualify(os.Args[2], os.Args[3], true)
	}
	if len(os.Args) != 3 || os.Getenv("HELMR_DISPOSABLE_NBD_PROOF") != "1" {
		return errors.New("usage: HELMR_DISPOSABLE_NBD_PROOF=1 nbd-qualify BACKEND PRIVATE_ARENA; requires disposable /dev/nbd14 and /dev/nbd15")
	}
	if err := qualify(os.Args[1], os.Args[2], false); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(exe, "--helper-death", os.Args[1], os.Args[2]+"-helper-death")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err = child.Run(); err != nil {
		return err
	}
	// The crash-case owner exited only after its exact consumer and backend did.
	for _, device := range []string{"nbd14", "nbd15"} {
		if _, err = os.Stat(filepath.Join("/sys/block", device, "pid")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("crash-case inactivity unproven")
		}
	}
	fmt.Println("PASS: helper death retains exclusion; stopped-consumer owner exit postflight")
	return nil
}
func qualify(backend, arena string, crash bool) error {
	if err := os.Mkdir(arena, 0700); err != nil {
		return err
	}
	fmt.Println("retain on any failure:", arena)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	server := exec.Command(backend, "-create", "-dir", filepath.Join(arena, "store"), "-socket", filepath.Join(arena, "backend.sock"), "-size", "16777216", "-limit", "4096")
	server.Stdout = os.Stdout
	server.Stderr = os.Stderr
	if err = server.Start(); err != nil {
		return err
	}
	// Backend teardown must not race an uncertain consumer/device lifecycle.
	for i := 0; i < 200; i++ {
		if _, err = os.Stat(filepath.Join(arena, "backend.sock")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	attachment, err := nbd.Claim(ctx, nbd.Config{Helper: exe, Socket: filepath.Join(arena, "backend.sock"), Arena: arena, Devices: []string{"/dev/nbd15", "/dev/nbd14"}, Size: 16 << 20})
	if err != nil {
		return err
	}
	fmt.Println("owned device:", attachment.Device())
	if !crash {
		if err = rejectSecond(ctx, exe, backend, arena, attachment.Device()); err != nil {
			return err
		}
	}

	_, err = attachment.ExposeInto(ctx, 65534, 65534)
	if err != nil {
		return fmt.Errorf("expose: %w", err)
	}
	jail, err := os.Open(filepath.Join(arena, "jail"))
	if err != nil {
		return err
	}
	defer jail.Close()
	arg := "--consumer"
	if crash {
		arg = "--consumer-hold"
	}
	consumer := exec.Command(exe, arg)
	consumer.ExtraFiles = []*os.File{jail}
	var ready *bufio.Reader
	if crash {
		pipe, e := consumer.StdoutPipe()
		if e != nil {
			return e
		}
		ready = bufio.NewReader(pipe)
	} else {
		consumer.Stdout = os.Stdout
	}
	consumer.Stderr = os.Stderr
	consumer.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, NoSetGroups: true}}
	if err = attachment.StartConsumer(consumer); err != nil {
		return err
	}
	if crash {
		if line, e := ready.ReadString('\n'); e != nil || line != "consumer-ready\n" {
			return fmt.Errorf("consumer readiness: %q %v", line, e)
		}
		raw, e := os.ReadFile(filepath.Join(arena, "claim.json"))
		if e != nil {
			return e
		}
		var record struct {
			PID         int
			ProcessStat string
		}
		if e = json.Unmarshal(raw, &record); e != nil || record.PID <= 0 {
			return errors.New("helper identity missing")
		}
		pidfd, e := unix.PidfdOpen(record.PID, 0)
		if e != nil {
			return e
		}
		defer unix.Close(pidfd)
		current, e := os.ReadFile(fmt.Sprintf("/proc/%d/stat", record.PID))
		if e != nil {
			return e
		}
		start := func(stat string) string {
			parts := strings.Fields(stat[strings.LastIndex(stat, ")")+1:])
			if len(parts) < 20 {
				return ""
			}
			return parts[19]
		}
		if start(string(current)) == "" || start(string(current)) != start(record.ProcessStat) {
			return errors.New("helper process identity changed")
		}
		if e = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0); e != nil {
			return e
		}

		// A failed control call alone could be a timeout. Poll the exact pidfd
		// for process exit before attributing exclusion to the controller.
		for {
			if e = ctx.Err(); e != nil {
				return e
			}
			fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
			_, e = unix.Poll(fds, 100)
			if errors.Is(e, unix.EINTR) {
				continue
			}
			if e != nil {
				return e
			}
			if fds[0].Revents&unix.POLLIN != 0 {
				break
			}
		}
		fmt.Println("verified helper exit via pidfd before second claim")

		if e = attachment.Flush(ctx); e == nil {
			return errors.New("dead helper accepted flush")
		}
		if e = rejectSecond(ctx, exe, backend, arena, attachment.Device()); e != nil {
			return e
		}
		if e = attachment.Release(ctx); e == nil {
			return errors.New("dead-helper cleanup falsely succeeded")
		}
		e = attachment.WaitConsumer(ctx)
		var exitErr *exec.ExitError
		if !errors.As(e, &exitErr) {
			return fmt.Errorf("consumer exit unproven: %v", e)
		}
		if e = server.Process.Kill(); e != nil {
			return e
		}
		_ = server.Wait()
		fmt.Println("PASS: helper death refused fresh claim while consumer alive; cleanup stayed unproven; consumer reaped")
		return nil // Process exit drops the retained descriptor only after consumer exit.
	}
	if err = attachment.WaitConsumer(ctx); err != nil {
		return err
	}
	if err = attachment.Flush(ctx); err != nil {
		return err
	}
	if err = server.Process.Kill(); err != nil {
		return err
	}
	_ = server.Wait()
	// Death of the backend must not be reported as a successful flush.
	if err = attachment.Flush(ctx); err == nil {
		return errors.New("flush succeeded after backend death")
	}
	if err = attachment.Release(ctx); err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join("/sys/block", filepath.Base(attachment.Device()), "pid")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("postflight device still active")
	}
	fmt.Println("PASS: jail UID access, 4KiB IO, FLUSH, daemon death, consumer exit, device cleanup")
	return nil // Keep the journal as qualification evidence, even on success.
}
func consume(hold bool) error {
	fd, err := unix.Openat(3, "computer.nbd", unix.O_RDWR|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "computer.nbd")
	defer f.Close()
	want := bytes.Repeat([]byte{0xa7}, 4096)
	if n, e := f.WriteAt(want, 4096); e != nil || n != len(want) {
		return fmt.Errorf("write: %d %v", n, e)
	}
	if err = f.Sync(); err != nil {
		return err
	}
	direct, err := unix.Openat(3, "computer.nbd", unix.O_RDONLY|unix.O_DIRECT|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	df := os.NewFile(uintptr(direct), "direct")
	defer df.Close()
	got, err := unix.Mmap(-1, 0, len(want), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return err
	}
	defer unix.Munmap(got)
	if _, err = df.ReadAt(got, 4096); err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("readback mismatch")
	}
	if hold {
		fmt.Println("consumer-ready")
		for {
			time.Sleep(time.Hour)
		}
	}
	return nil
}

func rejectSecond(ctx context.Context, exe, backend, arena, device string) error {
	var err error
	collisionArena := filepath.Join(arena, "collision")
	if err = os.Mkdir(collisionArena, 0700); err != nil {
		return err
	}
	second := exec.Command(backend, "-create", "-dir", filepath.Join(collisionArena, "store"), "-socket", filepath.Join(collisionArena, "backend.sock"), "-size", "16777216", "-limit", "4096")
	second.Stderr = os.Stderr
	if err = second.Start(); err != nil {
		return err
	}
	for i := 0; i < 200; i++ {
		if _, err = os.Stat(filepath.Join(collisionArena, "backend.sock")); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rejected, claimErr := nbd.Claim(ctx, nbd.Config{Helper: exe, Socket: filepath.Join(collisionArena, "backend.sock"), Arena: collisionArena, Devices: []string{device}, Size: 16 << 20})
	if claimErr == nil || !strings.Contains(claimErr.Error(), "no available allowlisted NBD device") {
		return fmt.Errorf("same-device claim was not refused: %v", claimErr)
	}
	if rejected == nil {
		return errors.New("missing rejected claim handle")
	}
	if err = rejected.Release(ctx); err != nil {
		return err
	}
	if err = second.Process.Kill(); err != nil {
		return err
	}
	_ = second.Wait()
	fmt.Println("same-device second claim refused")

	return nil
}
