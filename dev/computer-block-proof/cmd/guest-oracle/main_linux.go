// guest-oracle is a dev-only PID 1 fixture reached through the existing boot shell.
package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
)

const size = 4096

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stdout, "HELMR_ORACLE_ERROR", err)
	}
	for {
		syscall.Pause()
	}
}
func run() error {
	// /run is a tmpfs mounted by the serial bootstrap; scratch is already mounted.
	if err := os.MkdirAll("/run/computer", 0700); err != nil {
		return err
	}
	if err := syscall.Mount("/dev/vdc", "/run/computer", "ext4", 0, ""); err != nil {
		return err
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return err
	}
	expected := sha256.Sum256(data)
	paths := []string{"/run/computer/oracle.data", "/run/scratch/oracle.data"}
	for _, path := range paths {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if err == nil {
			err = f.Sync()
		}
		ce := f.Close()
		if err == nil {
			err = ce
		}
		if err != nil {
			return err
		}
	}
	// Include file metadata and directory entries in the intentional guest barrier.
	syscall.Sync()
	fmt.Printf("HELMR_ORACLE_READY %s\n", hex.EncodeToString(nonce[:]))
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "verify" || len(fields[1]) != 64 {
			continue
		}
		result := map[string]any{"nonce": hex.EncodeToString(nonce[:]), "computer": false, "scratch": false, "challenge": fields[1], "phase": "resumed"}
		for i, path := range paths {
			got, err := directDigest(path)
			key := []string{"computer", "scratch"}[i]
			result[key] = err == nil && bytes.Equal(got[:], expected[:])
			if err != nil {
				result[key+"_error"] = err.Error()
			}
		}
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		fmt.Printf("HELMR_ORACLE_RESULT %s\n", b)
	}
	return scanner.Err()
}
func directDigest(path string) ([32]byte, error) {
	var zero [32]byte
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECT, 0)
	if err != nil {
		return zero, err
	}
	defer syscall.Close(fd)
	// mmap guarantees page alignment. O_DIRECT must succeed; no buffered fallback.
	b, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON)
	if err != nil {
		return zero, err
	}
	defer syscall.Munmap(b)
	n, err := syscall.Pread(fd, b, 0)
	if err != nil {
		return zero, err
	}
	if n != len(b) {
		return zero, fmt.Errorf("short direct read %d", n)
	}
	return sha256.Sum256(b), nil
}
