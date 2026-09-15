//go:build linux

package firecracker

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
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/vishvananda/netns"
)

// This is a real kernel/namespace test, not a Firecracker VM test. The earlier
// hook supplies the BPF mark to a veth standing in for TAP. BPF itself has its
// own privileged tests. No packet in this fixture leaves the disposable host.
func TestProtectedEgressKernelNamespace(t *testing.T) {
	if os.Getenv("HELMR_TEST_NAMESPACE") != "1" {
		t.Skip("requires disposable privileged Linux namespace fixture")
	}
	if err := (&Connector{cfg: Config{NFTPath: "nft"}}).checkSecretEgressKernel(t.Context()); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
		return string(out)
	}
	owner := fmt.Sprintf("he-owner-%d", os.Getpid())
	guest := fmt.Sprintf("he-guest-%d", os.Getpid())
	run("ip", "netns", "add", owner)
	t.Cleanup(func() { exec.Command("ip", "netns", "del", owner).Run() })
	run("ip", "netns", "add", guest)
	t.Cleanup(func() { exec.Command("ip", "netns", "del", guest).Run() })
	run("ip", "link", "add", "tap0", "netns", owner, "type", "veth", "peer", "name", "eth0", "netns", guest)
	nsrun := func(ns string, args ...string) string {
		return run(append([]string{"ip", "netns", "exec", ns}, args...)...)
	}
	nsrun(owner, "ip", "addr", "add", "192.168.127.1/30", "dev", "tap0")
	nsrun(owner, "ip", "link", "set", "tap0", "up")
	nsrun(owner, "ip", "link", "set", "lo", "up")
	nsrun(guest, "ip", "addr", "add", "192.168.127.2/30", "dev", "eth0")
	nsrun(guest, "ip", "link", "set", "eth0", "up")
	nsrun(guest, "ip", "link", "set", "lo", "up")
	nsrun(guest, "ip", "route", "add", "default", "via", "192.168.127.1")
	handle, err := netns.GetFromName(owner)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if err := withNetworkNamespace(handle, func() error {
		for _, path := range []string{"/proc/sys/net/ipv4/conf/all/rp_filter", "/proc/sys/net/ipv4/conf/default/rp_filter"} {
			if e := os.WriteFile(path, []byte("0\n"), 0644); e != nil {
				return e
			}
		}
		return os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
	}); err != nil {
		t.Fatal(err)
	}
	var listener net.Listener
	if err := withNetworkNamespace(handle, func() error {
		var e error
		listener, e = listenSecretEgress()
		if e != nil {
			return e
		}
		return installSecretRoute("tap0", 71)
	}); err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := withNetworkNamespace(handle, func() error { return verifySecretRoute("tap0", 71) }); err != nil {
		t.Fatal(err)
	}
	script, err := renderNetworkPolicy(networkPolicyInput{Tap: "tap0", Peer: "host0", Mark: 71, GuestIPv4: "192.168.127.2", TranslationIPv4: "100.96.0.2", ResolverIPv4: "10.0.0.2", BlockedIPv4CIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("192.168.0.0/16")}, ProtectedPorts: []uint16{443, 8443}})
	if err != nil {
		t.Fatal(err)
	}
	// Inject only the ingress identity normally supplied by the TAP BPF program.
	script += "add table inet fixture_mark\nadd chain inet fixture_mark pre { type filter hook prerouting priority -160; policy accept; }\nadd rule inet fixture_mark pre iifname \"tap0\" meta mark set 71\n"
	cmd := exec.Command("ip", "netns", "exec", owner, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rendered policy: %v %s", err, out)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Synthetic egress fixture"}, DNSNames: []string{"api.example", "public.example"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	cert, err := tls.X509KeyPair(ca, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	marker := "hlmr_protected_" + strings.Repeat("a", 64)
	var hits, resolves atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		want := "Bearer fixture-secret"
		if r.Host == "public.example:8443" {
			want = "Bearer " + marker
		}
		if r.Header.Get("Authorization") != want {
			t.Errorf("wrong synthetic auth for %s", r.Host)
			w.WriteHeader(400)
			return
		}
		if r.URL.Path == "/redirect-private" {
			http.Redirect(w, r, "https://169.254.169.254:8443/", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	upstream.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	proxyConfig := secretproxy.Config{Origins: []string{"https://api.example:8443"}, AllowedDestination: func(ip netip.Addr) bool { return ip == netip.MustParseAddr("93.184.216.34") }, Certificate: func(context.Context, string) (tls.Certificate, error) { return cert, nil }, Resolve: func(_ context.Context, o string, m []string) (map[string][]byte, error) {
		resolves.Add(1)
		if o != "https://api.example:8443" || len(m) != 1 || m[0] != marker {
			return nil, fmt.Errorf("invalid authority")
		}
		return map[string][]byte{marker: []byte("fixture-secret")}, nil
	}, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "93.184.216.34:8443" {
			return nil, fmt.Errorf("original destination changed")
		}
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}, UpstreamTLS: &tls.Config{RootCAs: roots}}
	proxy, err := secretproxy.New(proxyConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { proxy.Close() }()
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(resetOnCloseListener{listener}) }()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, ca, 0600); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"api.example", "public.example"} {
		output := nsrun(guest, "curl", "--silent", "--show-error", "--fail", "--max-time", "5", "--noproxy", "*", "--cacert", caPath, "--resolve", host+":8443:93.184.216.34", "-H", "Authorization: Bearer "+marker, "https://"+host+":8443/")
		if output != "ok" {
			t.Fatal(output)
		}
	}
	if hits.Load() != 2 || resolves.Load() != 1 {
		t.Fatalf("hits=%d resolves=%d", hits.Load(), resolves.Load())
	}
	// Ordinary Node direct HTTPS requires CA trust only, no proxy setup.
	node := `const https=require('https');const r=https.get({host:'93.184.216.34',port:8443,servername:'api.example',headers:{Host:'api.example:8443',Authorization:'Bearer ` + marker + `'}},r=>{let b='';r.on('data',d=>b+=d);r.on('end',()=>{if(b!=='ok')process.exit(2);console.log('direct node TLS ok')})});r.on('error',e=>{console.error(e);process.exit(3)});r.setTimeout(5000,()=>process.exit(4));`
	nsrun(guest, "env", "-u", "HTTP_PROXY", "-u", "HTTPS_PROXY", "-u", "ALL_PROXY", "-u", "NO_PROXY", "-u", "NODE_USE_ENV_PROXY", "NODE_EXTRA_CA_CERTS="+caPath, "node", "-e", node)
	if bun := os.Getenv("HELMR_TEST_BUN_BINARY"); bun != "" {
		nsrun(guest, "env", "-u", "HTTP_PROXY", "-u", "HTTPS_PROXY", "-u", "ALL_PROXY", "-u", "NO_PROXY", "-u", "NODE_USE_ENV_PROXY", "NODE_EXTRA_CA_CERTS="+caPath, bun, "-e", node)
	} else {
		t.Log("Bun fixture not selected")
	}
	for _, ip := range []string{"192.168.127.1", "169.254.169.254", "10.0.0.1"} {
		cmd := exec.Command("ip", "netns", "exec", guest, "curl", "--silent", "--max-time", "1", "--noproxy", "*", "http://"+ip+":8443/")
		if cmd.Run() == nil {
			t.Fatalf("blocked destination %s reached listener", ip)
		}
	}
	// Follow the actual redirect with the guest's unchanged placeholder even
	// across hosts. The metadata hop must fail and must never reach host Dial.
	beforeHits, beforeResolves := hits.Load(), resolves.Load()
	redirect := exec.Command("ip", "netns", "exec", guest, "curl", "--silent", "--location-trusted", "--max-time", "2", "--noproxy", "*", "--cacert", caPath, "--resolve", "api.example:8443:93.184.216.34", "-H", "Authorization: Bearer "+marker, "--output", "/dev/null", "--write-out", "%{url_effective}", "https://api.example:8443/redirect-private")
	output, redirectErr := redirect.Output()
	if redirectErr == nil || string(output) != "https://169.254.169.254:8443/" {
		t.Fatalf("private redirect was not followed and denied: %q %v", output, redirectErr)
	}
	if hits.Load() != beforeHits+1 || resolves.Load() != beforeResolves+1 {
		t.Fatalf("private redirect escaped: hits=%d resolves=%d", hits.Load()-beforeHits, resolves.Load()-beforeResolves)
	}
	// Model a guest socket surviving loss of its host-side TCP state. Drop
	// close notifications while retiring the proxy, then resume guest IO against
	// a new listener. Linger=0 on the test listener prevents old FIN retries
	// from substituting for the reset that the new namespace policy must send.
	staleCtx, staleCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer staleCancel()
	staleProgram := `const tls=require('tls');const c=tls.connect({host:'93.184.216.34',port:8443,servername:'api.example'},()=>{console.log('ready');process.stdin.once('data',()=>{c.write('GET / HTTP/1.1\r\nHost: api.example:8443\r\n\r\n');setTimeout(()=>{console.error('stale socket timed out');process.exit(4)},1500)})});c.on('error',e=>{if(e.code==='ECONNRESET'){console.log('reset');process.exit(0)}console.error(e);process.exit(3)});c.on('end',()=>process.exit(5));`
	stale := exec.CommandContext(staleCtx, "ip", "netns", "exec", guest, "env", "NODE_EXTRA_CA_CERTS="+caPath, "node", "-e", staleProgram)
	staleInput, err := stale.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	staleOutput, err := stale.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var staleErrors strings.Builder
	stale.Stderr = &staleErrors
	if err := stale.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stale.Process.Kill() })
	reader := bufio.NewReader(staleOutput)
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("stale client setup %q %v", line, err)
	}
	nsrun(owner, "nft", "add", "table", "inet", "fixture_pause")
	nsrun(owner, "nft", "add", "chain", "inet", "fixture_pause", "out", "{ type filter hook output priority -10; policy drop; }")
	nsrun(owner, "nft", "add", "rule", "inet", "fixture_pause", "out", "counter", "drop")
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := withNetworkNamespace(handle, func() error { var e error; listener, e = listenSecretEgress(); return e }); err != nil {
		t.Fatal(err)
	}
	proxy, err = secretproxy.New(proxyConfig)
	if err != nil {
		t.Fatal(err)
	}
	go func() { done <- proxy.Serve(resetOnCloseListener{listener}) }()
	if out := nsrun(owner, "nft", "list", "chain", "inet", "fixture_pause", "out"); !regexp.MustCompile(`counter packets [1-9][0-9]* bytes`).MatchString(out) {
		t.Fatal("fixture did not drop old close notifications", out)
	}
	nsrun(owner, "nft", "delete", "table", "inet", "fixture_pause")
	beforeResolves = resolves.Load()
	if _, err := staleInput.Write([]byte("resume\n")); err != nil {
		t.Fatal(err)
	}
	line, readErr := reader.ReadString('\n')
	waitErr := stale.Wait()
	if readErr != nil || line != "reset\n" || waitErr != nil {
		t.Fatalf("stale flow not reset: %q read=%v exit=%v stderr=%s", line, readErr, waitErr, staleErrors.String())
	}
	if resolves.Load() != beforeResolves {
		t.Fatal("stale TLS flow regained credential authority")
	}
	if out := nsrun(guest, "curl", "--silent", "--fail", "--max-time", "3", "--noproxy", "*", "--cacert", caPath, "--resolve", "api.example:8443:93.184.216.34", "-H", "Authorization: Bearer "+marker, "https://api.example:8443/"); out != "ok" {
		t.Fatal("fresh connection failed after restart", out)
	}
	// Drift of either routing primitive must be detected, including while the
	// same namespace/listener remains alive.
	nsrun(owner, "ip", "rule", "del", "priority", "100")
	if err := withNetworkNamespace(handle, func() error { return verifySecretRoute("tap0", 71) }); err == nil {
		t.Fatal("rule drift accepted")
	}
	nsrun(owner, "ip", "rule", "add", "priority", "100", "fwmark", "2147483719/0xffffffff", "iif", "tap0", "lookup", "100")
	if err := withNetworkNamespace(handle, func() error { return verifySecretRoute("tap0", 71) }); err != nil {
		t.Fatal(err)
	}
	nsrun(owner, "ip", "route", "del", "local", "0.0.0.0/0", "dev", "lo", "table", "100")
	if err := withNetworkNamespace(handle, func() error { return verifySecretRoute("tap0", 71) }); err == nil {
		t.Fatal("route drift accepted")
	}
	cmd = exec.Command("ip", "netns", "exec", guest, "curl", "--silent", "--max-time", "1", "--noproxy", "*", "http://93.184.216.34:8443/")
	if cmd.Run() == nil {
		t.Fatal("missing local route forwarded captured traffic")
	}
	if err := proxy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// netns.New unshares AND enters the scratch namespace. Repeated qualification
// must neither install probe state in the caller nor change its namespace.
func TestSecretEgressPreflightIsolation(t *testing.T) {
	if os.Getenv("HELMR_TEST_NAMESPACE") != "1" {
		t.Skip("requires disposable privileged Linux namespace fixture")
	}
	listener, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", secretproxy.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	snapshot := func() string {
		t.Helper()
		var state strings.Builder
		for _, args := range [][]string{{"ip", "-j", "rule", "show"}, {"ip", "-j", "route", "show", "table", "all"}, {"nft", "list", "ruleset"}} {
			out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
			if err != nil {
				t.Fatalf("snapshot %v: %v %s", args, err, out)
			}
			state.Write(out)
		}
		return state.String()
	}
	before := snapshot()
	original, err := netns.Get()
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	for range 3 {
		if err := (&Connector{cfg: Config{NFTPath: "nft"}}).checkSecretEgressKernel(t.Context()); err != nil {
			t.Fatal(err)
		}
		after, err := netns.Get()
		if err != nil {
			t.Fatal(err)
		}
		same := original.Equal(after)
		after.Close()
		if !same || snapshot() != before {
			t.Fatal("preflight changed caller namespace or network state")
		}
	}
}

// Only the restart fixture uses this: model socket-state loss without delayed
// FIN retransmissions delivering a close notification after the simulated pause.
type resetOnCloseListener struct{ net.Listener }

func (l resetOnCloseListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		if e := c.(*net.TCPConn).SetLinger(0); e != nil {
			c.Close()
			return nil, e
		}
	}
	return c, err
}
