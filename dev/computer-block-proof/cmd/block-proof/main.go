// block-proof is a private local NBD development harness, never a runtime service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/dev/computer-block-proof/internal/blockproof"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	dir := flag.String("dir", "", "private local state directory")
	socket := flag.String("socket", "", "private Unix socket path (must not exist)")
	size := flag.Int64("size", 64<<20, "disk size in bytes; used only with -create")
	limit := flag.Int("limit", 128, "maximum retained dirty blocks")
	create := flag.Bool("create", false, "initialize a missing root; otherwise reopen existing root")
	flag.Parse()
	if *dir == "" || *socket == "" {
		return fmt.Errorf("-dir and -socket are required")
	}
	var disk *blockproof.Persistent
	var err error
	if *create {
		disk, err = blockproof.CreatePersistent(*dir, *size, *limit)
	} else {
		disk, err = blockproof.OpenPersistent(*dir, *limit)
	}
	if err != nil {
		return err
	}
	defer disk.Close()
	// Restrictive umask applies before bind; chmod alone would leave a brief window.
	syscall.Umask(0077)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: *socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); listener.Close() }()
	fmt.Fprintln(os.Stdout, "ready", *socket)
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		// Cancellation closes the active stream so shutdown cannot wait on a client.
		conn.SetDeadline(time.Now().Add(5 * time.Minute))
		finished := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				conn.Close()
			case <-finished:
			}
		}()
		err = blockproof.Serve(conn, disk)
		close(finished)
		conn.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "connection:", err)
		}
	}
}
