//go:build linux || darwin

// generation-proof serves synthetic encrypted fixture data through the production
// generation backend. Its public fixed key is never suitable for customer data.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	dir := flag.String("dir", "", "new private fixture directory")
	socket := flag.String("socket", "", "private Unix socket")
	size := flag.Int64("size", 16<<20, "fixture disk size")
	limit := flag.Int("limit", 256, "dirty block budget")
	create := flag.Bool("create", false, "create synthetic fixture")
	verify := flag.Bool("verify", false, "verify qualifier's committed data after backend death")
	flag.Parse()
	if os.Getenv("HELMR_DISPOSABLE_NBD_PROOF") != "1" || !filepath.IsAbs(*dir) || *create == *verify {
		return errors.New("explicit disposable proof and exactly one of -create/-verify required")
	}
	syscall.Umask(0077)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *create {
		if err := os.Mkdir(*dir, 0700); err != nil {
			return err
		}
	}
	if *verify {
		info, err := os.Lstat(filepath.Join(*dir, "base"))
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("fixture base directory missing")
		}
	}
	store, err := cas.NewFile(filepath.Join(*dir, "base"))
	if err != nil {
		return err
	}
	const key = "01992000-0000-7000-8000-000000000001"
	cfg := computer.LocalGenerationConfig{Directory: filepath.Join(*dir, "local"), BaseSource: store, Scope: "synthetic-generation-proof", ActiveKey: key, Keys: map[string][]byte{key: bytes.Repeat([]byte{7}, 32)}, DirtyBlocks: *limit, StagedBytes: 64 << 20, PackLimit: blockformat.MinPackLimit}
	var disk *computer.LocalGeneration
	if *create {
		writer := blockformat.Writer{Source: store, Sink: store, Scope: cfg.Scope, ActiveKey: key, Keys: cfg.Keys, PackLimit: cfg.PackLimit}
		locator, e := writer.Empty(ctx, *size, 64)
		if e != nil {
			return e
		}
		cfg.Base, e = computer.NewGenerationRoot(locator, *size)
		if e != nil {
			return e
		}
		raw, e := json.Marshal(cfg.Base)
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(*dir, "base.json"), raw, 0600); e != nil {
			return e
		}
		disk, err = computer.CreateLocalGeneration(ctx, cfg)
	} else {
		raw, e := os.ReadFile(filepath.Join(*dir, "base.json"))
		if e != nil {
			return e
		}
		if e = json.Unmarshal(raw, &cfg.Base); e != nil {
			return e
		}
		disk, err = computer.OpenLocalGeneration(ctx, cfg)
	}
	if err != nil {
		return err
	}
	defer disk.Close()
	if *verify {
		data := make([]byte, 4096)
		if _, err = disk.ReadAt(ctx, data, 4096); err != nil {
			return err
		}
		if !bytes.Equal(data, bytes.Repeat([]byte{0xa7}, 4096)) {
			return errors.New("committed guest data mismatch")
		}
		fmt.Println("PASS: authenticated generation reopened after backend death")
		return nil
	}
	if !filepath.IsAbs(*socket) {
		return errors.New("absolute socket required")
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	fmt.Println("ready", *socket)
	return disk.ServeNBD(ctx, listener)
}
