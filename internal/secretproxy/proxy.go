// Package secretproxy owns the host-only HTTP credential substitution boundary.
package secretproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/origin"
)

const MarkerPrefix = "hlmr_protected_"
const Port = 3128
const PublicCAPath = "/run/helmr/secret-proxy/ca.pem"
const maxHeaderBytes = 64 << 10

var ErrTrustExpired = errors.New("workspace Secret transport has expired; create a new Workspace")

var markerPattern = regexp.MustCompile(`hlmr_protected_[a-f0-9]{64}`)

type Config struct {
	Origins     []string
	Certificate func(context.Context, string) (tls.Certificate, error)
	Resolve     func(context.Context, string, []string) (map[string][]byte, error)
	DialContext func(context.Context, string, string) (net.Conn, error)
	// UpstreamTLS is constructed by the host; never from guest input.
	UpstreamTLS *tls.Config
}

type Proxy struct {
	config      Config
	transport   *http.Transport
	server      *http.Server
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]struct{}
	slots       chan struct{}
}

func New(config Config) (*Proxy, error) {
	if config.DialContext == nil || config.Resolve == nil || config.Certificate == nil {
		return nil, errors.New("proxy dependencies are required")
	}
	config.Origins = slices.Clone(config.Origins)
	for _, value := range config.Origins {
		canonical, err := origin.Canonical(value)
		if err != nil || value != canonical {
			return nil, errors.New("proxy origin is not canonical")
		}
	}
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	p := &Proxy{config: config, connections: make(map[net.Conn]struct{}), slots: make(chan struct{}, 64)}
	p.transport = &http.Transport{Proxy: nil, DialContext: config.DialContext, TLSClientConfig: config.UpstreamTLS,
		DisableKeepAlives: true, Protocols: protocols, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, MaxResponseHeaderBytes: maxHeaderBytes}
	p.server = p.newServer(http.HandlerFunc(p.serve))
	return p, nil
}

func (p *Proxy) newServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute,
		MaxHeaderBytes: maxHeaderBytes, ErrorLog: discardLogger()}
}

func (p *Proxy) Serve(listener net.Listener) error {
	err := p.server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	for conn := range p.connections {
		_ = conn.Close()
	}
	p.mu.Unlock()
	p.transport.CloseIdleConnections()
	return p.server.Close()
}

func (p *Proxy) own(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = conn.Close()
		return false
	}
	p.connections[conn] = struct{}{}
	return true
}

func (p *Proxy) release(conn net.Conn) {
	_ = conn.Close()
	p.mu.Lock()
	delete(p.connections, conn)
	p.mu.Unlock()
}

func deny(w http.ResponseWriter) {
	http.Error(w, "Secret proxy request unavailable", http.StatusBadGateway)
}

func denyTransportError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrTrustExpired) {
		http.Error(w, ErrTrustExpired.Error(), http.StatusBadGateway)
		return
	}
	deny(w)
}

func (p *Proxy) serve(w http.ResponseWriter, r *http.Request) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		deny(w)
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" {
		deny(w)
		return
	}
	p.forward(w, r, "")
}

func authority(value, scheme string) (string, string, error) {
	u, err := url.Parse(scheme + "://" + value)
	if err != nil || u.User != nil || u.Host != value || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(value, "\\%?#@ \t\r\n") {
		return "", "", errors.New("invalid destination authority")
	}
	host := strings.ToLower(u.Hostname())
	ip, ipErr := netip.ParseAddr(host)
	if (ipErr == nil && !ip.Is4()) || (ipErr != nil && !origin.ValidHostname(host)) {
		return "", "", errors.New("invalid destination hostname")
	}
	port := u.Port()
	if strings.HasSuffix(value, ":") {
		return "", "", errors.New("invalid destination port")
	}
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return "", "", errors.New("invalid destination port")
	}
	canonical := scheme + "://" + host
	if !(scheme == "https" && port == "443" || scheme == "http" && port == "80") {
		canonical += ":" + port
	}
	return canonical, net.JoinHostPort(host, port), nil
}

func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	target, address, err := authority(r.Host, "https")
	if err != nil || r.ContentLength > 0 || len(r.TransferEncoding) != 0 || len(r.Trailer) != 0 {
		deny(w)
		return
	}
	if _, err := markers(r.Header); err != nil || containsMarker(r.Host) {
		deny(w)
		return
	}
	for _, values := range r.Header {
		for _, value := range values {
			if containsMarker(value) {
				deny(w)
				return
			}
		}
	}
	protected := slices.Contains(p.config.Origins, target)
	var upstream net.Conn
	var certificate tls.Certificate
	if protected {
		certificate, err = p.config.Certificate(r.Context(), strings.Split(address, ":")[0])
	} else {
		upstream, err = p.config.DialContext(r.Context(), "tcp4", address)
	}
	if err != nil {
		denyTransportError(w, err)
		return
	}
	if upstream != nil {
		defer upstream.Close()
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		deny(w)
		return
	}
	conn, buffer, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if !p.own(conn) {
		return
	}
	defer p.release(conn)
	if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	client := &bufferedConn{Conn: conn, reader: buffer.Reader}
	if !protected {
		if !p.own(upstream) {
			return
		}
		defer p.release(upstream)
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = conn.Close()
		_ = upstream.Close()
		<-done
		return
	}
	host, _, _ := net.SplitHostPort(address)
	tlsConn := tls.Server(client, &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if !strings.EqualFold(hello.ServerName, host) {
				return nil, errors.New("TLS destination mismatch")
			}
			return &certificate, nil
		}})
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.HandshakeContext(r.Context()); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	listener := &singleListener{conn: tlsConn, done: make(chan struct{})}
	server := p.newServer(http.HandlerFunc(func(w http.ResponseWriter, inner *http.Request) { p.forward(w, inner, target) }))
	server.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			_ = listener.Close()
		}
	}
	_ = server.Serve(listener)
}

func containsMarker(value string) bool { return strings.Contains(value, MarkerPrefix) }

func markers(header http.Header) ([]string, error) {
	seen := map[string]bool{}
	for name, values := range header {
		if containsMarker(name) {
			return nil, errors.New("invalid marker location")
		}
		for _, value := range values {
			if !containsMarker(value) {
				continue
			}
			if forbiddenHeader(name) || connectionNominates(header, name) {
				return nil, errors.New("invalid marker header")
			}
			matches := markerPattern.FindAllString(value, -1)
			if containsMarker(markerPattern.ReplaceAllString(value, "")) || len(matches) == 0 {
				return nil, errors.New("invalid marker")
			}
			for _, match := range matches {
				seen[match] = true
			}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	slices.Sort(result)
	return result, nil
}

func forbiddenHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "transfer-encoding", "connection", "proxy-authorization", "proxy-authenticate", "proxy-connection", "keep-alive", "te", "trailer", "upgrade":
		return true
	}
	return false
}

func connectionNominates(header http.Header, name string) bool {
	for _, value := range header.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(key), name) {
				return true
			}
		}
	}
	return false
}

func stripHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, key := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(key))
		}
	}
	for key := range header {
		if forbiddenHeader(key) {
			header.Del(key)
		}
	}
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, expected string) {
	if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" || len(r.Trailer) != 0 || containsMarker(r.RequestURI) || containsMarker(r.Host) {
		deny(w)
		return
	}
	scheme := "http"
	if expected != "" {
		scheme = "https"
	}
	target, _, err := authority(r.Host, scheme)
	if err != nil || expected != "" && target != expected {
		deny(w)
		return
	}
	if r.URL.IsAbs() {
		actual, _, err := authority(r.URL.Host, r.URL.Scheme)
		if err != nil || actual != target {
			deny(w)
			return
		}
	}
	selectors, err := markers(r.Header)
	if err != nil {
		deny(w)
		return
	}
	var values map[string][]byte
	if len(selectors) > 0 {
		if expected == "" || !slices.Contains(p.config.Origins, target) {
			deny(w)
			return
		}
		values, err = p.config.Resolve(r.Context(), target, selectors)
		defer func() {
			for _, value := range values {
				clear(value)
			}
		}()
		if err != nil || len(values) != len(selectors) {
			denyTransportError(w, err)
			return
		}
		for _, selector := range selectors {
			value, ok := values[selector]
			if !ok || len(value) == 0 || !validHeaderValue(value) {
				deny(w)
				return
			}
		}
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme = scheme
	out.URL.Host = r.Host
	out.Header = r.Header.Clone()
	for name, entries := range out.Header {
		for i, value := range entries {
			entries[i] = markerPattern.ReplaceAllStringFunc(value, func(marker string) string { return string(values[marker]) })
		}
		out.Header[name] = entries
	}
	size := 0
	for name, entries := range out.Header {
		for _, value := range entries {
			size += len(name) + len(value) + 4
		}
	}
	if size > maxHeaderBytes {
		deny(w)
		return
	}
	stripHopHeaders(out.Header)
	response, err := p.transport.RoundTrip(out)
	if err != nil {
		deny(w)
		return
	}
	defer response.Body.Close()
	stripHopHeaders(response.Header)
	for name, entries := range response.Header {
		for _, value := range entries {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func validHeaderValue(value []byte) bool {
	for _, c := range value {
		if c == 127 || c < 32 && c != '\t' {
			return false
		}
	}
	return true
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

type singleListener struct {
	conn     net.Conn
	done     chan struct{}
	once     sync.Once
	accepted bool
}

func (l *singleListener) Accept() (net.Conn, error) {
	if !l.accepted {
		l.accepted = true
		return l.conn, nil
	}
	<-l.done
	return nil, net.ErrClosed
}
func (l *singleListener) Close() error {
	l.once.Do(func() { close(l.done); _ = l.conn.Close() })
	return nil
}
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }
