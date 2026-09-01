package email

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestSMTPSenderCancellationClosesActiveConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	sender := NewSMTPSender(listener.Addr().String(), "", "", "noreply@example.test")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- sender.SendEmail(ctx, Message{To: "owner@example.test", Subject: "Hello"})
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
		defer conn.Close()
	case <-time.After(time.Second):
		t.Fatal("SMTP connection was not opened")
	}
	cancel()
	select {
	case sendErr := <-done:
		if sendErr == nil || !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("SendEmail error = %v, want cancellation", sendErr)
		}
	case <-time.After(time.Second):
		t.Fatal("SMTP send did not stop after cancellation")
	}
}
