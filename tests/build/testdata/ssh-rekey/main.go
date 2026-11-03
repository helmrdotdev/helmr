// Exercise the OpenSSH client in the builder when a trusted server switches
// between two known host keys during a single connection.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

type rotatingHostKey struct {
	first, second ssh.Signer
	exchanges     atomic.Int32
}

func (k *rotatingHostKey) current() ssh.Signer {
	if k.exchanges.Load() == 0 {
		return k.first
	}
	return k.second
}

func (k *rotatingHostKey) PublicKey() ssh.PublicKey { return k.current().PublicKey() }

func (k *rotatingHostKey) Sign(r io.Reader, data []byte) (*ssh.Signature, error) {
	signature, err := k.current().Sign(r, data)
	if err == nil {
		k.exchanges.Add(1)
	}
	return signature, err
}

func newHostKey() ssh.Signer {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		log.Fatal(err)
	}
	return signer
}

func sendPayload(listener net.Listener, key *rotatingHostKey, payload []byte) error {
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.RekeyThreshold = 32768
	config.AddHostKey(key)
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return err
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	channel, ok := <-channels
	if !ok {
		return fmt.Errorf("client disconnected before opening a channel")
	}
	stream, commands, err := channel.Accept()
	if err != nil {
		return err
	}
	for cmd := range commands {
		if cmd.Type != "exec" {
			cmd.Reply(false, nil)
			continue
		}
		cmd.Reply(true, nil)
		if _, err := io.Copy(stream, bytes.NewReader(payload)); err != nil {
			return err
		}
		if _, err := stream.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0})); err != nil {
			return err
		}
		stream.Close()
		server.Wait() // Let outstanding writes and key exchanges drain.
		return nil
	}
	return fmt.Errorf("client disconnected before requesting data")
}

func main() {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	key := &rotatingHostKey{first: newHostKey(), second: newHostKey()}
	knownHosts := filepath.Join(os.TempDir(), "known_hosts")
	var hosts bytes.Buffer
	for _, signer := range []ssh.Signer{key.first, key.second} {
		fmt.Fprintf(&hosts, "[127.0.0.1]:%d %s", port, ssh.MarshalAuthorizedKey(signer.PublicKey()))
	}
	if err := os.WriteFile(knownHosts, hosts.Bytes(), 0600); err != nil {
		log.Fatal(err)
	}
	payload := bytes.Repeat([]byte("rekey"), 400000)
	done := make(chan error, 1)
	go func() { done <- sendPayload(listener, key, payload) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", "-oBatchMode=yes", "-oStrictHostKeyChecking=yes",
		"-oUserKnownHostsFile="+knownHosts, "-oRekeyLimit=32K", "-p", fmt.Sprint(port), "127.0.0.1", "transfer")
	cmd.Stderr = os.Stderr
	got, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	if err := <-done; err != nil {
		log.Fatal(err)
	}
	if !bytes.Equal(got, payload) || key.exchanges.Load() < 2 {
		log.Fatalf("incomplete transfer: bytes=%d key exchanges=%d", len(got), key.exchanges.Load())
	}
	fmt.Printf("transferred %d bytes across %d key exchanges\n", len(got), key.exchanges.Load())
}
