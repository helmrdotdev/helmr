package secretproxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testMarker = MarkerPrefix + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testCertificate(t *testing.T, hosts ...string) (tls.Certificate, []byte, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic fixture"},
		DNSNames: hosts, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return cert, certPEM, pool
}

type fixture struct {
	proxy               *Proxy
	upstreamCertificate []byte
	upstreamProtocols   chan int
	url                 *url.URL
	ca                  []byte
	client              *http.Client
	mu                  sync.Mutex
	value               string
	available           bool
	resolutions         atomic.Int32
	hits                atomic.Int32
	authed              atomic.Int32
}

func newFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *fixture {
	return newFixtureAt(t, 443, handler)
}

func newFixtureAt(t *testing.T, port uint16, handler func(http.ResponseWriter, *http.Request)) *fixture {
	t.Helper()
	f := &fixture{value: "synthetic-first-token", available: true, upstreamProtocols: make(chan int, 256)}
	origin := "https://api.github.com"
	if port != 443 {
		origin += ":" + strconv.Itoa(int(port))
	}
	cert, ca, pool := testCertificate(t, "api.github.com", "public.example")
	f.ca = ca
	upstreamCert, _, upstreamPool := testCertificate(t, "api.github.com", "public.example", "93.184.216.34")
	f.upstreamCertificate = upstreamCert.Certificate[0]
	pool.AddCert(func() *x509.Certificate { c, _ := x509.ParseCertificate(upstreamCert.Certificate[0]); return c }())
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.upstreamProtocols <- r.ProtoMajor
		f.mu.Lock()
		value := f.value
		f.mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer "+value || r.Header.Get("Authorization") == "token "+value {
			f.authed.Add(1)
		}
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"login":"synthetic-user"}`)
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{upstreamCert}}
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	p, err := New(Config{AllowedDestination: func(netip.Addr) bool { return true }, Origins: []string{origin}, Certificate: func(context.Context, string) (tls.Certificate, error) { return cert, nil },
		Resolve: func(_ context.Context, o string, selectors []string) (map[string][]byte, error) {
			f.resolutions.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.available || o != origin || len(selectors) != 1 || selectors[0] != testMarker {
				return nil, errors.New("unavailable")
			}
			return map[string][]byte{testMarker: []byte(f.value)}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", upstream.Listener.Addr().String())
		},
		UpstreamTLS: &tls.Config{RootCAs: upstreamPool},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.proxy = p
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = p.Serve(&destinationListener{Listener: listener, destination: netip.AddrPortFrom(netip.MustParseAddr("93.184.216.34"), port)})
	}()
	t.Cleanup(func() { _ = p.Close() })
	f.url, _ = url.Parse("http://" + listener.Addr().String())
	f.client = &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", f.url.Host)
	}, TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: false}, Timeout: 10 * time.Second}
	t.Cleanup(func() { f.client.CloseIdleConnections() })
	return f
}

func request(t *testing.T, f *fixture, target, marker string) int {
	t.Helper()
	r, _ := http.NewRequest("GET", target, nil)
	if marker != "" {
		r.Header.Set("Authorization", "Bearer "+marker)
	}
	response, err := f.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func TestProtectedRotationRevocationAndAnonymous(t *testing.T) {
	f := newFixture(t, nil)
	if status := request(t, f, "https://api.github.com/user", testMarker); status != 200 {
		t.Fatal(status)
	}
	f.mu.Lock()
	f.value = "synthetic-rotated-token"
	f.mu.Unlock()
	if status := request(t, f, "https://api.github.com/user", testMarker); status != 200 {
		t.Fatal(status)
	}
	if f.authed.Load() != 2 || f.resolutions.Load() != 2 {
		t.Fatal("per-request current material was not used")
	}
	f.mu.Lock()
	f.available = false
	f.mu.Unlock()
	if status := request(t, f, "https://api.github.com/user", testMarker); status != 502 {
		t.Fatal(status)
	}
	if f.hits.Load() != 2 {
		t.Fatal("revoked request reached upstream")
	}
	if status := request(t, f, "https://api.github.com/public", ""); status != 200 {
		t.Fatal(status)
	}
	if f.resolutions.Load() != 3 {
		t.Fatal("anonymous request resolved secrets")
	}
}

func TestForgedAndMalformedMarkersFailBeforeUpstream(t *testing.T) {
	for _, marker := range []string{MarkerPrefix + strings.Repeat("a", 64), MarkerPrefix + "bad"} {
		t.Run(marker[len(MarkerPrefix):], func(t *testing.T) {
			f := newFixture(t, nil)
			if request(t, f, "https://api.github.com/user", marker) != 502 || f.hits.Load() != 0 {
				t.Fatal("marker failure forwarded")
			}
		})
	}
}

func TestUnrelatedTLSDoesNotResolve(t *testing.T) {
	f := newFixture(t, nil)
	if request(t, f, "https://public.example/discovery", "") != 200 || f.resolutions.Load() != 0 || f.hits.Load() != 1 {
		t.Fatal("ordinary HTTPS failed")
	}
}

func TestUncertainSendIsOneWireAttempt(t *testing.T) {
	for _, idempotency := range []bool{false, true} {
		t.Run(map[bool]string{false: "get", true: "idempotency-header"}[idempotency], func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
			})
			r, _ := http.NewRequest("GET", "https://api.github.com/uncertain", nil)
			r.Header.Set("Authorization", "Bearer "+testMarker)
			if idempotency {
				r.Header.Set("Idempotency-Key", "synthetic")
			}
			response, err := f.client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode != 502 || f.hits.Load() != 1 || f.resolutions.Load() != 1 {
				t.Fatalf("status=%d hits=%d resolutions=%d", response.StatusCode, f.hits.Load(), f.resolutions.Load())
			}
		})
	}
}

func TestDialPolicyPreservesEffectiveDenyAndIPv4(t *testing.T) {
	d := &Dialer{Blocked: []netip.Prefix{netip.MustParsePrefix("8.8.8.0/24"), netip.MustParsePrefix("1.1.1.1/32")}}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "127.0.0.1", "169.254.169.254", "10.0.0.1", "2606:4700:4700::1111", "::ffff:8.8.4.4"} {
		if d.Allowed(netip.MustParseAddr(address)) {
			t.Fatalf("allowed %s", address)
		}
	}
	if !d.Allowed(netip.MustParseAddr("8.8.4.4")) {
		t.Fatal("public IPv4 denied")
	}
}

func TestConnectionNominatedMarkerFailsClosed(t *testing.T) {
	f := newFixture(t, nil)
	request, _ := http.NewRequest("GET", "https://api.github.com/", nil)
	request.Header.Set("Authorization", "Bearer "+testMarker)
	request.Header.Set("Connection", "authorization")
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 502 || f.hits.Load() != 0 || f.resolutions.Load() != 0 {
		t.Fatal("hop header marker was downgraded")
	}
}

func TestExpiredTrustGivesSafeRecreationError(t *testing.T) {
	response := httptest.NewRecorder()
	denyTransportError(response, ErrTrustExpired)
	if response.Code != 502 || !strings.Contains(response.Body.String(), "create a new Workspace") {
		t.Fatal("expiry did not explain recreation")
	}
	response = httptest.NewRecorder()
	denyTransportError(response, errors.New("synthetic-sensitive-value"))
	if strings.Contains(response.Body.String(), "synthetic-sensitive-value") {
		t.Fatal("transport disclosed arbitrary error")
	}
}

// Models the kernel-owned original-destination socket fact in portable protocol
// tests. Only the privileged namespace test proves actual TPROXY delivery.
type destinationListener struct {
	net.Listener
	destination netip.AddrPort
}

func (l *destinationListener) Accept() (net.Conn, error) {
	c, e := l.Listener.Accept()
	if e != nil {
		return nil, e
	}
	return &destinationConn{Conn: c, destination: l.destination}, nil
}

type destinationConn struct {
	net.Conn
	destination netip.AddrPort
}

func (c *destinationConn) LocalAddr() net.Addr { return net.TCPAddrFromAddrPort(c.destination) }
func (c *destinationConn) CloseWrite() error   { return c.Conn.(*net.TCPConn).CloseWrite() }
