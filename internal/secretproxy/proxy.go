// Package secretproxy owns host-only, transparent HTTPS credential mediation.
package secretproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
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
const Port = 3128 // Namespace-local TPROXY target; never a guest proxy endpoint.
const maxHeaderBytes = 64 << 10

// These are per-runtime host allocation bounds for captured traffic only.
// Kernel-forwarded ports and guest loopback do not consume admissions.
// See resource_test.go for overload, release, cancellation and isolation.
const maxCapturedConnections = 256
const maxProtectedRequests = maxCapturedConnections

var ErrTrustExpired = errors.New("workspace Secret transport has expired; create a new Workspace")
var markerPattern = regexp.MustCompile(`hlmr_protected_[a-f0-9]{64}`)

type Config struct {
	Origins            []string
	Certificate        func(context.Context, string) (tls.Certificate, error)
	Resolve            func(context.Context, string, []string) (map[string][]byte, error)
	AllowedDestination func(netip.Addr) bool
	DialContext        func(context.Context, string, string) (net.Conn, error)
	// UpstreamTLS is host-owned trust, never guest configuration.
	UpstreamTLS *tls.Config
}

type Proxy struct {
	config         Config
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	closed         bool
	listener       net.Listener
	connections    map[net.Conn]struct{}
	workers        sync.WaitGroup
	handlers       sync.WaitGroup
	requests       chan struct{}
	captured       chan struct{}
	activeRequests chan struct{}
}

func New(config Config) (*Proxy, error) {
	if config.DialContext == nil || config.AllowedDestination == nil || config.Resolve == nil || config.Certificate == nil {
		return nil, errors.New("egress dependencies are required")
	}
	config.Origins = slices.Clone(config.Origins)
	for _, value := range config.Origins {
		canonical, err := origin.Canonical(value)
		if err != nil || canonical != value {
			return nil, errors.New("egress origin is not canonical")
		}
	}
	if config.UpstreamTLS != nil {
		config.UpstreamTLS = config.UpstreamTLS.Clone()
		if config.UpstreamTLS.InsecureSkipVerify {
			return nil, errors.New("upstream certificate verification is required")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Proxy{config: config, ctx: ctx, cancel: cancel, connections: make(map[net.Conn]struct{}), requests: make(chan struct{}, 64), captured: make(chan struct{}, maxCapturedConnections), activeRequests: make(chan struct{}, maxProtectedRequests)}, nil
}

// Ports is the exact derived capture set. A shared port conveys no Secret authority.
func (p *Proxy) Ports() []uint16 {
	var ports []uint16
	for _, o := range p.config.Origins {
		u, _ := url.Parse(o)
		port := u.Port()
		if port == "" {
			port = "443"
		}
		n, _ := strconv.Atoi(port)
		ports = append(ports, uint16(n))
	}
	slices.Sort(ports)
	return slices.Compact(ports)
}

// Serve requires a transparent listener: accepted LocalAddr is the kernel's
// original destination. Ordinary test listeners must model that socket fact.
func (p *Proxy) Serve(listener net.Listener) error {
	p.mu.Lock()
	if p.closed || p.listener != nil {
		p.mu.Unlock()
		listener.Close()
		return net.ErrClosed
	}
	p.listener = listener
	p.mu.Unlock()
	for {
		conn, err := listener.Accept()
		if err != nil {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			conn.Close()
			continue
		}
		// No user-space waiting room: do not allocate a worker or upstream for
		// a socket beyond this runtime's capture envelope.
		select {
		case p.captured <- struct{}{}:
		default:
			p.mu.Unlock()
			conn.Close()
			continue
		}
		p.connections[conn] = struct{}{}
		p.workers.Add(1)
		p.mu.Unlock()
		go func() {
			defer p.workers.Done()
			defer func() { <-p.captured }()
			defer p.release(conn)
			p.serveConn(conn)
		}()
	}
}

// Close fences immediately, including requests still resolving credentials and
// established H2 streams. Connections do not survive parking or restoration.
func (p *Proxy) Close() error {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	if p.listener != nil {
		_ = p.listener.Close()
	}
	for conn := range p.connections {
		_ = conn.Close()
	}
	p.mu.Unlock()
	p.workers.Wait()
	p.handlers.Wait()
	return nil
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
func (p *Proxy) dial(ctx context.Context, address string) (net.Conn, error) {
	conn, err := p.config.DialContext(ctx, "tcp4", address)
	if err != nil {
		return nil, err
	}
	if !p.own(conn) {
		return nil, net.ErrClosed
	}
	return &ownedConn{Conn: conn, release: func() { p.release(conn) }}, nil
}

type ownedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *ownedConn) Close() error { c.once.Do(c.release); return nil }
func (c *ownedConn) CloseWrite() error {
	if tcp, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return tcp.CloseWrite()
	}
	return nil
}

func deny(w http.ResponseWriter) {
	http.Error(w, "Protected HTTPS request unavailable", http.StatusBadGateway)
}
func denyTransportError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrTrustExpired) {
		http.Error(w, ErrTrustExpired.Error(), http.StatusBadGateway)
		return
	}
	deny(w)
}
func authority(value, scheme string) (string, error) {
	u, err := url.Parse(scheme + "://" + value)
	if err != nil || u.User != nil || u.Host != value || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(value, "\\%?#@ \t\r\n") {
		return "", errors.New("invalid destination authority")
	}
	host := strings.ToLower(u.Hostname())
	ip, ipErr := netip.ParseAddr(host)
	if (ipErr == nil && !ip.Is4()) || (ipErr != nil && !origin.ValidHostname(host)) {
		return "", errors.New("invalid destination hostname")
	}
	port := u.Port()
	if strings.HasSuffix(value, ":") {
		return "", errors.New("invalid destination port")
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
		return "", errors.New("invalid destination port")
	}
	canonical := scheme + "://" + host
	if !(scheme == "https" && port == "443" || scheme == "http" && port == "80") {
		canonical += ":" + port
	}
	return canonical, nil
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

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, expected string, destination netip.AddrPort, dialRequest func(context.Context) (net.Conn, error)) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.handlers.Add(1)
	p.mu.Unlock()
	defer p.handlers.Done()
	// Multiplexed H2 requests need their own bound, held through streaming.
	// The smaller Resolve window below is released before any upstream IO.
	select {
	case p.activeRequests <- struct{}{}:
		defer func() { <-p.activeRequests }()
	default:
		http.Error(w, "Protected HTTPS concurrent request limit reached", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodConnect || r.Header.Get("Upgrade") != "" || len(r.Trailer) != 0 {
		http.Error(w, "Protected HTTPS does not support CONNECT, upgrades, or request trailers", http.StatusNotImplemented)
		return
	}
	if containsMarker(r.RequestURI) || containsMarker(r.Host) {
		deny(w)
		return
	}
	target, err := authority(r.Host, "https")
	if err != nil || target != expected {
		http.Error(w, "Protected HTTPS authority does not match TLS destination", http.StatusMisdirectedRequest)
		return
	}
	if r.URL.IsAbs() {
		actual, err := authority(r.URL.Host, r.URL.Scheme)
		if err != nil || actual != target {
			deny(w)
			return
		}
	}
	selectors, err := markers(r.Header)
	if err != nil || !p.config.AllowedDestination(destination.Addr()) {
		deny(w)
		return
	}
	var values map[string][]byte
	if len(selectors) > 0 {
		select {
		case p.requests <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		values, err = p.config.Resolve(r.Context(), target, selectors)
		<-p.requests
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
	header := r.Header.Clone()
	size := 0
	for name, entries := range header {
		for i, value := range entries {
			entries[i] = markerPattern.ReplaceAllStringFunc(value, func(marker string) string { return string(values[marker]) })
			size += len(name) + len(entries[i]) + 4
		}
	}
	if size > maxHeaderBytes {
		deny(w)
		return
	}
	stripHopHeaders(header)
	// A fresh pinned transport bounds each request's credential and destination
	// lifetime. No connection pooling or application-level retry is introduced.
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.config.UpstreamTLS != nil {
		tlsConfig = p.config.UpstreamTLS.Clone()
	}
	u, _ := url.Parse(expected)
	tlsConfig.ServerName = u.Hostname()
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetHTTP2(true)
	transport := &http.Transport{Proxy: nil, Protocols: protocols, TLSClientConfig: tlsConfig,
		HTTP2:             &http.HTTP2Config{MaxReadFrameSize: 64 << 10, MaxReceiveBufferPerConnection: 256 << 10, MaxReceiveBufferPerStream: 64 << 10},
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dialRequest(ctx) },
		DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second, MaxResponseHeaderBytes: maxHeaderBytes}
	defer transport.CloseIdleConnections()
	// Enable streaming uploads with simultaneous responses on HTTP/1 as well as H2.
	_ = http.NewResponseController(w).EnableFullDuplex()
	proxy := httputil.ReverseProxy{Transport: transport, FlushInterval: -1, ErrorLog: discardLogger(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = u.Host
			pr.Out.Host = u.Host
			pr.Out.Header = header
			pr.Out.Header.Del("Forwarded")
			pr.Out.Header.Del("X-Forwarded-For")
			pr.Out.Header.Del("X-Forwarded-Host")
			pr.Out.Header.Del("X-Forwarded-Proto")
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { deny(w) },
	}
	proxy.ServeHTTP(w, r)
}
func validHeaderValue(value []byte) bool {
	for _, c := range value {
		if c == 127 || c < 32 && c != '\t' {
			return false
		}
	}
	return true
}
