package secretproxy

import (
	"bufio"
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
	"os"
	"os/exec"
	"path/filepath"
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
	proxy       *Proxy
	url         *url.URL
	ca          []byte
	client      *http.Client
	mu          sync.Mutex
	value       string
	available   bool
	resolutions atomic.Int32
	hits        atomic.Int32
	authed      atomic.Int32
}

func newFixture(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *fixture {
	t.Helper()
	f := &fixture{value: "synthetic-first-token", available: true}
	cert, ca, pool := testCertificate(t, "api.github.com", "public.example")
	f.ca = ca
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
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
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)
	p, err := New(Config{Origins: []string{"https://api.github.com"}, Certificate: func(context.Context, string) (tls.Certificate, error) { return cert, nil },
		Resolve: func(_ context.Context, o string, selectors []string) (map[string][]byte, error) {
			f.resolutions.Add(1)
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.available || o != "https://api.github.com" || len(selectors) != 1 || selectors[0] != testMarker {
				return nil, errors.New("unavailable")
			}
			return map[string][]byte{testMarker: []byte(f.value)}, nil
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", upstream.Listener.Addr().String())
		},
		UpstreamTLS: &tls.Config{RootCAs: pool},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.proxy = p
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = p.Serve(listener) }()
	t.Cleanup(func() { _ = p.Close() })
	f.url, _ = url.Parse("http://" + listener.Addr().String())
	f.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(f.url), TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: false}, Timeout: 10 * time.Second}
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

func TestOrdinaryConnectDoesNotResolve(t *testing.T) {
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

func TestNativeClients(t *testing.T) {
	if os.Getenv("HELMR_TEST_NATIVE_SECRET_PROXY") != "1" {
		t.Skip("set HELMR_TEST_NATIVE_SECRET_PROXY=1 for real gh/Node fixtures")
	}
	f := newFixture(t, nil)
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, f.ca, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"gh", "node"} {
		t.Run(tool, func(t *testing.T) {
			binary, err := exec.LookPath(tool)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"api", "/user"}
			if tool == "node" {
				args = []string{"-e", `fetch('https://api.github.com/user',{headers:{Authorization:'Bearer '+process.env.GH_TOKEN}}).then(async r=>{if(!r.ok)throw Error('request failed');console.log(await r.text())}).catch(()=>process.exit(1))`}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "GH_CONFIG_DIR=" + filepath.Join(dir, "gh"), "GH_TOKEN=" + testMarker, "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "HTTPS_PROXY=" + f.url.String(), "HTTP_PROXY=" + f.url.String(), "NO_PROXY=", "SSL_CERT_FILE=" + caPath, "NODE_EXTRA_CA_CERTS=" + caPath, "NODE_USE_ENV_PROXY=1"}
			before := f.authed.Load()
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s: %v: %s", tool, err, output)
			}
			if !strings.Contains(string(output), "synthetic-user") || f.authed.Load() != before+1 {
				t.Fatalf("%s did not traverse authenticated proxy: %s", tool, output)
			}
			if strings.Contains(string(output), f.value) {
				t.Fatal("credential leaked")
			}
		})
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

func TestCloseOwnsHijackedTunnel(t *testing.T) {
	f := newFixture(t, nil)
	conn, err := net.Dial("tcp4", f.url.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "CONNECT public.example:443 HTTP/1.1\r\nHost: public.example:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "CONNECT"})
	if err != nil || response.StatusCode != 200 {
		t.Fatal("tunnel did not open", err)
	}
	if err := f.proxy.Close(); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("hijacked guest socket stayed open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("proxy close did not close hijacked socket")
	}
}

func TestOrdinaryHTTPPassThroughSanitizesHopHeaders(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Hop") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("hop credentials forwarded")
		}
		conn, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		buffer.WriteString("HTTP/1.1 204 No Content\r\nConnection: X-Hop-Response\r\nX-Hop-Response: private-hop\r\n\r\n")
		buffer.Flush()
	}))
	defer upstream.Close()
	p, err := New(Config{Origins: nil, Certificate: func(context.Context, string) (tls.Certificate, error) {
		t.Error("ordinary request asked for leaf")
		return tls.Certificate{}, errors.New("unexpected")
	}, Resolve: func(context.Context, string, []string) (map[string][]byte, error) {
		t.Error("ordinary request resolved a Secret")
		return nil, errors.New("unexpected")
	}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "1.1.1.1:80" {
			return nil, errors.New("fixture wrong target")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp4", upstream.Listener.Addr().String())
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go p.Serve(listener)
	proxyURL, _ := url.Parse("http://" + listener.Addr().String())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	request, _ := http.NewRequest("GET", "http://1.1.1.1/", nil)
	request.Header.Set("Connection", "X-Hop")
	request.Header.Set("X-Hop", "hop-value")
	request.Header.Set("Proxy-Authorization", "proxy-only")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 204 || response.Header.Get("X-Hop-Response") != "" || hits.Load() != 1 {
		t.Fatalf("ordinary HTTP status=%d hop=%q hits=%d", response.StatusCode, response.Header.Get("X-Hop-Response"), hits.Load())
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
