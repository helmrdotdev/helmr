package email

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTPServer accepts STARTTLS sessions and records each delivered message
// with the connection that carried it.
type fakeSMTPServer struct {
	listener net.Listener
	config   *tls.Config
	roots    *x509.CertPool
	// rejectRecipient is refused at RCPT; dropQuit closes the session on QUIT
	// without replying.
	rejectRecipient string
	dropQuit        bool

	mu          sync.Mutex
	connections int
	messages    []fakeSMTPMessage
}

type fakeSMTPMessage struct {
	connection int
	from       string
	to         string
	data       string
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server := &fakeSMTPServer{
		listener: listener,
		config:   &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12},
		roots:    roots,
	}
	go server.serve()
	return server
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.connections++
		connection := s.connections
		s.mu.Unlock()
		go s.session(conn, connection)
	}
}

func (s *fakeSMTPServer) session(conn net.Conn, connection int) {
	defer conn.Close()
	text := textproto.NewConn(conn)
	reply := func(line string) { _ = text.PrintfLine("%s", line) }
	reply("220 fake ready")
	var from, to string
	for {
		line, err := text.ReadLine()
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch command {
		case "EHLO":
			reply("250-fake")
			reply("250 STARTTLS")
		case "STARTTLS":
			reply("220 go ahead")
			secure := tls.Server(conn, s.config)
			if secure.Handshake() != nil {
				return
			}
			conn = secure
			text = textproto.NewConn(secure)
			reply = func(line string) { _ = text.PrintfLine("%s", line) }
		case "MAIL":
			from = line
			reply("250 ok")
		case "RCPT":
			if s.rejectRecipient != "" && strings.Contains(line, "<"+s.rejectRecipient+">") {
				reply("550 mailbox unavailable")
				continue
			}
			to = line
			reply("250 ok")
		case "DATA":
			reply("354 send")
			data, err := text.ReadDotLines()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.messages = append(s.messages, fakeSMTPMessage{connection: connection, from: from, to: to, data: strings.Join(data, "\n")})
			s.mu.Unlock()
			reply("250 queued")
		case "RSET":
			from, to = "", ""
			reply("250 reset")
		case "QUIT":
			if !s.dropQuit {
				reply("221 bye")
			}
			return
		default:
			reply("502 unsupported")
		}
	}
}

func TestSMTPSenderSendsEveryMessageOverOneSession(t *testing.T) {
	server := newFakeSMTPServer(t)
	sender := NewSMTPSender(server.listener.Addr().String(), "", "", "Helmr <noreply@example.test>")
	sender.rootCAs = server.roots

	err := sender.SendEmails(t.Context(), []Message{
		{To: "ada@example.test", Subject: "First", PlainText: "one"},
		{To: "grace@example.test", Subject: "Second", PlainText: "two"},
		{To: "lin@example.test", Subject: "Third", PlainText: "three"},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.connections != 1 || len(server.messages) != 3 {
		t.Fatalf("connections = %d, messages = %+v", server.connections, server.messages)
	}
	for i, want := range []struct{ to, subject, body string }{
		{"ada@example.test", "First", "one"},
		{"grace@example.test", "Second", "two"},
		{"lin@example.test", "Third", "three"},
	} {
		message := server.messages[i]
		if message.connection != 1 || message.from != "MAIL FROM:<noreply@example.test>" || message.to != "RCPT TO:<"+want.to+">" {
			t.Fatalf("message %d envelope = %+v", i, message)
		}
		if !strings.Contains(message.data, "Subject: "+want.subject) || !strings.HasSuffix(message.data, "\n\n"+want.body) {
			t.Fatalf("message %d data = %q", i, message.data)
		}
	}
}

func TestSMTPSenderRejectsABatchWithAnInvalidRecipientBeforeConnecting(t *testing.T) {
	server := newFakeSMTPServer(t)
	sender := NewSMTPSender(server.listener.Addr().String(), "", "", "noreply@example.test")
	sender.rootCAs = server.roots
	if err := sender.SendEmails(t.Context(), []Message{{To: "ada@example.test"}, {To: "not an address"}}); err == nil {
		t.Fatal("expected invalid recipient error")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.connections != 0 {
		t.Fatalf("connections = %d, want none", server.connections)
	}
}

func TestSMTPSenderNamesTheRefusedRecipientAfterSendingEarlierMessages(t *testing.T) {
	server := newFakeSMTPServer(t)
	server.rejectRecipient = "grace@example.test"
	sender := NewSMTPSender(server.listener.Addr().String(), "", "", "noreply@example.test")
	sender.rootCAs = server.roots
	err := sender.SendEmails(t.Context(), []Message{{To: "ada@example.test", Subject: "First"}, {To: "grace@example.test", Subject: "Second"}, {To: "lin@example.test", Subject: "Third"}})
	var refused RecipientError
	if !errors.As(err, &refused) || refused.Address != "grace@example.test" {
		t.Fatalf("error = %v, want the refused recipient", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.messages) != 1 || server.messages[0].to != "RCPT TO:<ada@example.test>" {
		t.Fatalf("messages = %+v, want only the message before the refused one", server.messages)
	}
}

func TestSMTPSenderEncodesNonASCIISubjectsAndIgnoresAFailedQuit(t *testing.T) {
	server := newFakeSMTPServer(t)
	server.dropQuit = true
	sender := NewSMTPSender(server.listener.Addr().String(), "", "", "noreply@example.test")
	sender.rootCAs = server.roots
	if err := sender.SendEmails(t.Context(), []Message{{To: "ada@example.test", Subject: "Zoë invited you to Café", PlainText: "body"}, {To: "lin@example.test", Subject: "Plain"}}); err != nil {
		t.Fatalf("send with a dropped QUIT = %v", err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.messages) != 2 {
		t.Fatalf("messages = %+v", server.messages)
	}
	if !strings.Contains(server.messages[0].data, "Subject: =?utf-8?q?Zo=C3=AB_invited_you_to_Caf=C3=A9?=") || !strings.Contains(server.messages[1].data, "Subject: Plain\n") {
		t.Fatalf("subjects = %q / %q", server.messages[0].data, server.messages[1].data)
	}
}
