package secretproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"
)

const maxClientHelloBytes = 64 << 10
const classificationTimeout = 10 * time.Second

func peek(conn net.Conn) (*bufio.Reader, <-chan struct{}) {
	reader := bufio.NewReader(conn)
	done := make(chan struct{})
	go func() { _, _ = reader.Peek(1); close(done) }()
	return reader, done
}

func (p *Proxy) serveConn(conn net.Conn) {
	address, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	destination := address.AddrPort()
	if !destination.Addr().Is4() || destination.Port() == 0 || !p.config.AllowedDestination(destination.Addr()) {
		return
	}
	// A server-first protocol sharing a protected port must receive its greeting
	// without waiting for a ClientHello. No client bytes enter this speculative
	// upstream until classification chooses opaque forwarding.
	upstream, err := p.dial(p.ctx, destination.String())
	if err != nil {
		return
	}
	clientReader, clientReady := peek(conn)
	upstreamReader, upstreamReady := peek(upstream)
	defer func() { conn.Close(); upstream.Close(); <-clientReady; <-upstreamReady }()
	select {
	case <-upstreamReady:
		p.relay(conn, upstream, clientReader, upstreamReader, clientReady, upstreamReady)
		return
	case <-clientReady:
	}
	first, err := clientReader.Peek(1)
	if err != nil {
		return
	}
	if first[0] != 22 { // TLS handshake record; crypto/tls owns all further parsing.
		p.relay(conn, upstream, clientReader, upstreamReader, clientReady, upstreamReady)
		return
	}
	// Idle TCP is not a TLS handshake. Start the parser deadline only after a
	// handshake record arrives, preserving pre-warming and server-first traffic.
	_ = conn.SetReadDeadline(time.Now().Add(classificationTimeout))
	observed := &helloConn{Conn: conn, reader: clientReader}
	var host string
	stopped := errors.New("ClientHello inspected")
	parser := tls.Server(observed, &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) { host = hello.ServerName; return nil, stopped }})
	_ = parser.HandshakeContext(p.ctx)
	replay := &readerConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(observed.record.Bytes()), clientReader)}
	target, err := authority(net.JoinHostPort(host, strconv.Itoa(int(destination.Port()))), "https")
	if err != nil || host == "" || !slices.Contains(p.config.Origins, target) {
		// Including no SNI/ECH/unknown TLS: never infer Secret authority from an IP.
		p.relay(replay, upstream, nil, upstreamReader, closedSignal(), upstreamReady)
		return
	}
	// Hand the checked speculative socket to exactly one request transport.
	// Its peek reader must finish before that transport reads the TLS response.
	// This preserves immediate server-first traffic without an empty extra TCP
	// connection for protected HTTPS. No credentials or TLS session are pooled.
	var firstMu sync.Mutex
	firstUpstream := upstream
	dialRequest := func(ctx context.Context) (net.Conn, error) {
		firstMu.Lock()
		conn := firstUpstream
		firstUpstream = nil
		firstMu.Unlock()
		if conn != nil {
			select {
			case <-upstreamReady:
				if upstreamReader.Buffered() == 0 {
					// The speculative peer closed while the guest was idle.
					conn.Close()
					return p.dial(ctx, destination.String())
				}
			default:
			}
			return &readerConn{Conn: conn, reader: upstreamReader, ready: upstreamReady}, nil
		}
		return p.dial(ctx, destination.String())
	}
	certificate, err := p.config.Certificate(p.ctx, host)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	listener := &connectionListener{conn: replay, done: make(chan struct{})}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.forward(w, r, target, destination, dialRequest) }),
		// Bound pre-handler H2 streams/headers as well as DATA buffering. A
		// runtime request admission alone cannot bound library-parsed streams.
		HTTP2:     &http.HTTP2Config{MaxConcurrentStreams: 4, MaxReadFrameSize: 64 << 10, MaxReceiveBufferPerConnection: 256 << 10, MaxReceiveBufferPerStream: 64 << 10},
		Protocols: protocols, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}},
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: maxHeaderBytes, ErrorLog: discardLogger(),
		BaseContext: func(net.Listener) context.Context { return p.ctx },
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				listener.Close()
			}
		},
	}
	defer server.Close()
	_ = server.ServeTLS(listener, "", "")
}

// helloConn reuses the standard TLS parser, retaining exactly the bytes it read.
// Aborting GetConfigForClient causes crypto/tls to write an alert: those writes
// MUST NOT reach the guest or alter unrelated end-to-end TLS.
type helloConn struct {
	net.Conn
	reader io.Reader
	record bytes.Buffer
}

func (c *helloConn) Read(b []byte) (int, error) {
	remaining := maxClientHelloBytes - c.record.Len()
	if remaining == 0 {
		return 0, io.EOF
	}
	if len(b) > remaining {
		b = b[:remaining]
	}
	n, err := c.reader.Read(b)
	c.record.Write(b[:n])
	return n, err
}
func (c *helloConn) Write(b []byte) (int, error) { return len(b), nil }

type readerConn struct {
	net.Conn
	reader io.Reader
	ready  <-chan struct{}
}

func (c *readerConn) Read(b []byte) (int, error) {
	if c.ready != nil {
		<-c.ready
	}
	return c.reader.Read(b)
}
func (c *readerConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func closedSignal() <-chan struct{} { done := make(chan struct{}); close(done); return done }

// A read EOF only half-closes the opposite writer; the peer may still return a
// response. Close/fencing closes both sockets and interrupts either copy loop.
func (p *Proxy) relay(client, upstream net.Conn, clientReader, upstreamReader *bufio.Reader, clientReady, upstreamReady <-chan struct{}) {
	_ = client.SetReadDeadline(time.Time{})
	_ = upstream.SetReadDeadline(time.Time{})
	var wait sync.WaitGroup
	pump := func(dst, src net.Conn, reader *bufio.Reader, ready <-chan struct{}) {
		defer wait.Done()
		<-ready
		var input io.Reader = src
		if reader != nil {
			input = reader
		}
		_, err := io.Copy(dst, input)
		if err != nil {
			client.Close()
			upstream.Close()
			return
		}
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		}
	}
	wait.Add(2)
	go pump(upstream, client, clientReader, clientReady)
	go pump(client, upstream, upstreamReader, upstreamReady)
	wait.Wait()
}

type connectionListener struct {
	conn     net.Conn
	done     chan struct{}
	once     sync.Once
	accepted bool
}

func (l *connectionListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *connectionListener) Close() error {
	l.once.Do(func() { close(l.done); _ = l.conn.Close() })
	return nil
}
func (l *connectionListener) Addr() net.Addr { return l.conn.LocalAddr() }
