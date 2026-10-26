//go:build linux

package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/secretproxy"
	"github.com/vishvananda/netns"
)

const networkClientToken = "helmr-network-client-synthetic-token"

var networkClientHosts = map[string]string{"model": "model.network.test", "git": "git.network.test", "registry": "registry.network.test", "auth": "auth.network.test", "cdn": "cdn.network.test", "private": "private.network.test"}

type networkClientFixture struct {
	directory  string
	server     *http.Server
	configFile string
	mu         sync.Mutex
	observed   map[string]any
	streams    map[string]chan struct{}
	tarball    []byte
	gitHead    string
}

func networkClientEnv(home string) []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "NPM_CONFIG_USERCONFIG=" + filepath.Join(home, "npmrc"), "NPM_CONFIG_GLOBALCONFIG=" + filepath.Join(home, "npm-globalrc"), "NPM_CONFIG_CACHE=" + filepath.Join(home, "npm-cache"), "NPM_CONFIG_UPDATE_NOTIFIER=false"}
}
func networkClientCommand(t *testing.T, home string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), args[0], args[1:]...)
	command.Env = networkClientEnv(home)
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture command %s: %v: %s", args[0], err, raw)
	}
	return strings.TrimSpace(string(raw))
}

func newNetworkClientFixture(t *testing.T, directory string, listener net.Listener) *networkClientFixture {
	t.Helper()
	fixture := &networkClientFixture{directory: directory, observed: map[string]any{"crossOriginCredentials": 0, "deniedOriginContacts": 0}, streams: map[string]chan struct{}{}}
	home := filepath.Join(directory, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"npmrc", "npm-globalrc"} {
		if err := os.WriteFile(filepath.Join(home, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	versions := map[string]string{"node": networkClientCommand(t, home, "node", "--version"), "npm": networkClientCommand(t, home, "npm", "--version"), "git": networkClientCommand(t, home, "git", "--version"), "anthropic": "0.98.0"}
	if versions["node"] != "v24.22.1" || versions["npm"] != "11.19.1" || versions["git"] != "git version 2.51.2" {
		t.Fatalf("use Product-pinned Node/npm/git, got %v", versions)
	}
	repo := filepath.Join(directory, "source")
	networkClientCommand(t, home, "git", "-c", "init.defaultBranch=main", "init", "--quiet", repo)
	if err := os.WriteFile(filepath.Join(repo, "README"), []byte("Synthetic network fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	networkClientCommand(t, home, "git", "-C", repo, "add", "README")
	networkClientCommand(t, home, "git", "-C", repo, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", "fixture")
	networkClientCommand(t, home, "git", "clone", "--quiet", "--bare", repo, filepath.Join(directory, "repo.git"))
	networkClientCommand(t, home, "git", "--git-dir="+filepath.Join(directory, "repo.git"), "update-server-info")
	fixture.gitHead = networkClientCommand(t, home, "git", "-C", repo, "rev-parse", "HEAD")
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	for name, content := range map[string]string{"package/package.json": `{"name":"@helmr/network-client-fixture","version":"1.0.0","main":"index.js"}`, "package/index.js": "module.exports = { fixture: true }\n"} {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.tarball = archive.Bytes()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Helmr synthetic network fixture CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(directory, "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0600); err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, host := range networkClientHosts {
		names = append(names, host)
	}
	names = append(names, "denied.network.test")
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: names, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: serverKey}
	fixture.server = &http.Server{Handler: http.HandlerFunc(fixture.serve), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: time.Minute, TLSConfig: &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}}
	go func() { _ = fixture.server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = fixture.server.Close() })
	origins := map[string]string{}
	for name, host := range networkClientHosts {
		origins[name] = "https://" + host
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Open(executable)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, binary); err != nil {
		binary.Close()
		t.Fatal(err)
	}
	if err := binary.Close(); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{"caFile": "ca.pem", "origins": origins, "expectedVersions": versions, "gitHead": fixture.gitHead, "fixtureBinarySHA256": fmt.Sprintf("%x", digest.Sum(nil))}
	raw, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	fixture.configFile = filepath.Join(directory, "config.json")
	if err := os.WriteFile(fixture.configFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *networkClientFixture) mark(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed[name] = true
}
func (f *networkClientFixture) credentialLeak() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed["crossOriginCredentials"] = f.observed["crossOriginCredentials"].(int) + 1
}
func (f *networkClientFixture) serve(w http.ResponseWriter, r *http.Request) {
	host := strings.TrimSuffix(r.Host, ":443")
	if host == "denied.network.test" {
		f.mu.Lock()
		f.observed["deniedOriginContacts"] = f.observed["deniedOriginContacts"].(int) + 1
		f.mu.Unlock()
		http.Error(w, "forbidden fixture origin", 403)
		return
	}
	switch host {
	case networkClientHosts["model"]:

		if strings.HasPrefix(r.URL.Path, "/stream-ack/") {
			probe := strings.TrimPrefix(r.URL.Path, "/stream-ack/")
			f.mu.Lock()
			gate := f.streams[probe]
			if gate == nil {
				f.mu.Unlock()
				http.Error(w, "unknown stream probe", 404)
				return
			}
			select {
			case <-gate:
			default:
				close(gate)
			}
			f.observed["streamAcknowledged"] = true
			f.mu.Unlock()
			w.WriteHeader(204)
			return
		}

		if r.URL.Path != "/v1/messages" || r.Method != "POST" || r.Header.Get("X-Api-Key") != networkClientToken {
			http.Error(w, "synthetic model request required", 400)
			return
		}
		probe := r.Header.Get("X-Helmr-Stream-Probe")
		if len(probe) != 36 {
			http.Error(w, "stream probe required", 400)
			return
		}
		f.mu.Lock()
		if len(f.streams) >= 16 || f.streams[probe] != nil {
			f.mu.Unlock()
			http.Error(w, "stream probe limit", 503)
			return
		}
		gate := make(chan struct{})
		f.streams[probe] = gate
		f.mu.Unlock()
		defer func() { f.mu.Lock(); delete(f.streams, probe); f.mu.Unlock() }()
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(kind, body string) {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, body)
			w.(http.Flusher).Flush()
		}
		send("message_start", `{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"synthetic-network-fixture","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)
		send("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first"}}`)
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		case <-time.After(10 * time.Second):
			return
		}
		send("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"second"}}`)
		send("content_block_stop", `{"type":"content_block_stop","index":0}`)
		send("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`)
		send("message_stop", `{"type":"message_stop"}`)
	case networkClientHosts["git"]:
		if r.Header.Get("Authorization") != "" {
			f.credentialLeak()
			http.Error(w, "unexpected fixture credential", 403)
			return
		}
		f.mark("gitRead")
		http.FileServer(http.Dir(f.directory)).ServeHTTP(w, r)
	case networkClientHosts["registry"]:
		if r.URL.Path == "/__report" {
			f.mu.Lock()
			defer f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(f.observed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+networkClientToken {
			w.Header().Set("WWW-Authenticate", `Bearer realm="fixture"`)
			http.Error(w, "synthetic registry token required", 401)
			return
		}
		f.mark("registryAuthenticated")
		if r.URL.Path == "/tarball" {
			http.Redirect(w, r, "https://"+networkClientHosts["auth"]+"/authorize", 302)
			return
		}
		if r.URL.Path == "/denied-tarball" {
			f.mark("deniedRedirect")
			http.Redirect(w, r, "https://denied.network.test/package.tgz", 302)
			return
		}
		name := "@helmr/network-client-fixture"
		tarballPath := "/tarball"
		if strings.Contains(r.URL.Path, "network-client-denied") {
			name = "@helmr/network-client-denied"
			tarballPath = "/denied-tarball"
		}
		integrity := sha512.Sum512(f.tarball)
		version := map[string]any{"name": name, "version": "1.0.0", "dist": map[string]any{"tarball": "https://" + networkClientHosts["registry"] + tarballPath, "integrity": "sha512-" + base64.StdEncoding.EncodeToString(integrity[:])}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name, "dist-tags": map[string]string{"latest": "1.0.0"}, "versions": map[string]any{"1.0.0": version}})
	case networkClientHosts["auth"]:
		if r.Header.Get("Authorization") != "" {
			f.credentialLeak()
			http.Error(w, "cross-origin fixture credential", 403)
			return
		}
		f.mark("authRedirect")
		http.Redirect(w, r, "https://"+networkClientHosts["cdn"]+"/package.tgz", 302)
	case networkClientHosts["cdn"]:
		if r.Header.Get("Authorization") != "" {
			f.credentialLeak()
			http.Error(w, "cross-origin fixture credential", 403)
			return
		}
		f.mark("cdnRead")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(f.tarball)
	case networkClientHosts["private"]:
		f.mark("privateRead")
		_, _ = w.Write([]byte("private fixture"))
	default:
		http.Error(w, "unknown fixture origin", 404)
	}
}

// The endpoint-only mode is reusable on a separately routed fixture namespace
// for a real Computer. It never changes Worker routes, denies, DNS or CA trust.
func TestNetworkClientEndpoint(t *testing.T) {
	directory := os.Getenv("HELMR_NETWORK_CLIENT_SERVER_DIR")
	if directory == "" {
		t.Skip("requires explicit independent fixture endpoint directory and address")
	}
	address, err := netip.ParseAddr(os.Getenv("HELMR_NETWORK_CLIENT_BIND_IPV4"))
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() {
		t.Fatal("fixture prerequisite: explicit routed IPv4 endpoint address")
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal("fixture directory must be new", err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(address.String(), "443"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := newNetworkClientFixture(t, directory, listener)
	t.Logf("synthetic TLS fixture ready: %s", fixture.configFile)
	ctx, cancel := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
}

func TestNetworkRealClientsNamespace(t *testing.T) {
	if os.Getenv("HELMR_TEST_NAMESPACE") != "1" {
		t.Skip("requires disposable privileged Linux network namespace fixture")
	}
	script, sdkRoot := os.Getenv("HELMR_NETWORK_CLIENT_SCRIPT"), os.Getenv("NETWORK_CLIENT_SDK_ROOT")
	if script == "" || sdkRoot == "" {
		t.Fatal("fixture prerequisite: provide network-clients.mjs and installed pinned Anthropic SDK root")
	}
	directory := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if raw, err := exec.CommandContext(t.Context(), args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, raw)
		}
	}
	owner, guest, endpoint := fmt.Sprintf("hncl-host-%d", os.Getpid()), fmt.Sprintf("hncl-guest-%d", os.Getpid()), fmt.Sprintf("hncl-endpoint-%d", os.Getpid())
	for _, name := range []string{owner, guest, endpoint} {
		run("ip", "netns", "add", name)
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", name).Run() })
		run("ip", "-n", name, "link", "set", "lo", "up")
	}
	run("ip", "link", "add", "tap0", "netns", owner, "type", "veth", "peer", "name", "eth0", "netns", guest)
	for _, spec := range []struct{ ns, device, ip string }{{owner, "tap0", "192.168.127.1/30"}, {guest, "eth0", "192.168.127.2/30"}} {
		run("ip", "-n", spec.ns, "addr", "add", spec.ip, "dev", spec.device)
		run("ip", "-n", spec.ns, "link", "set", spec.device, "up")
	}
	run("ip", "-n", guest, "route", "add", "default", "via", "192.168.127.1")
	// Only the disposable container's network is changed. The endpoint owns its
	// addresses in a distinct namespace, outside all Worker-host/interface denies.
	rootVeth := fmt.Sprintf("hncl%d", os.Getpid())
	run("ip", "link", "add", rootVeth, "type", "veth", "peer", "name", "upstream0", "netns", endpoint)
	t.Cleanup(func() { _ = exec.Command("ip", "link", "del", rootVeth).Run() })
	run("ip", "addr", "add", "100.111.0.1/30", "dev", rootVeth)
	run("ip", "link", "set", rootVeth, "up")
	run("ip", "-n", endpoint, "addr", "add", "100.111.0.2/30", "dev", "upstream0")
	run("ip", "-n", endpoint, "link", "set", "upstream0", "up")
	run("ip", "-n", endpoint, "route", "add", "default", "via", "100.111.0.1")
	for _, address := range []string{"8.8.8.8", "10.60.1.2"} {
		run("ip", "-n", endpoint, "addr", "add", address+"/32", "dev", "lo")
		run("ip", "route", "add", address+"/32", "via", "100.111.0.2", "dev", rootVeth)
	}
	endpointHandle, err := netns.GetFromName(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer endpointHandle.Close()
	var tlsListener net.Listener
	if err := withNetworkNamespace(endpointHandle, func() error { var e error; tlsListener, e = net.Listen("tcp4", "0.0.0.0:443"); return e }); err != nil {
		t.Fatal(err)
	}
	fixture := newNetworkClientFixture(t, directory, tlsListener)
	handle, err := netns.GetFromName(owner)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	var listener net.Listener
	var udp net.PacketConn
	var tcp net.Listener
	if err := withNetworkNamespace(handle, func() error {
		if e := os.WriteFile("/proc/sys/net/ipv4/conf/all/rp_filter", []byte("0\n"), 0644); e != nil {
			return e
		}
		var e error
		listener, e = listenSecretEgress()
		if e != nil {
			return e
		}
		if e = installSecretRoute("tap0", 71); e != nil {
			return e
		}
		udp, e = net.ListenPacket("udp4", "0.0.0.0:53")
		if e != nil {
			return e
		}
		tcp, e = net.Listen("tcp4", "0.0.0.0:53")
		return e
	}); err != nil {
		t.Fatal(err)
	}
	policy := networkpolicy.Policy{Kind: networkpolicy.Allowlist}
	for name, host := range networkClientHosts {
		entry := networkpolicy.Domain{Domain: host}
		if name == "private" {
			entry.IPRanges = []string{"10.60.1.2/32"}
		}
		policy.Domains = append(policy.Domains, entry)
	}
	rawPolicy, _ := json.Marshal(policy)
	publicPolicy := policy.Public()
	policy, err = networkpolicy.ParsePublic(publicPolicy)
	if err != nil {
		t.Fatal(string(rawPolicy), err)
	}
	blocked, err := hostIPv4Prefixes(netip.MustParsePrefix("192.168.127.0/30"))
	if err != nil {
		t.Fatal(err)
	}
	blocked = append(blocked, netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("192.168.127.0/30"))
	installation := networkpolicy.Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.1.2/32")}, BlockedIPv4CIDRs: blocked}
	if err := policy.ValidateInstallation(installation); err != nil {
		t.Fatal(err)
	}
	lookup := func(ctx context.Context, network, name string) ([]netip.Addr, error) {
		host := strings.TrimSuffix(name, ".")
		if host == "denied.network.test" && network == "ip4" {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		for kind, allowed := range networkClientHosts {
			if host == allowed && network == "ip4" {
				if kind == "private" {
					return []netip.Addr{netip.MustParseAddr("10.60.1.2")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			}
		}
		return nil, &net.DNSError{Err: "unlisted fixture name", Name: name, IsNotFound: true}
	}
	dialer := &secretproxy.Dialer{Installation: &installation}
	proxy, err := secretproxy.New(secretproxy.Config{NetworkPolicy: policy, Installation: installation, LookupIP: lookup, AllowedDestination: dialer.Allowed, DialContext: dialer.DialContext})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	go func() { _ = proxy.Serve(listener) }()
	dns, err := secretproxy.NewDNS(policy, installation, lookup, udp, tcp, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dns.Close()
	dns.Serve()
	scriptPolicy, err := renderNetworkPolicy(networkPolicyInput{Policy: policy, PrivateBusinessCIDRs: installation.PrivateBusinessCIDRs, Tap: "tap0", Peer: "unused0", Mark: 71, GuestIPv4: "192.168.127.2", TranslationIPv4: "100.96.0.2", ResolverIPv4: "10.0.0.2", BlockedIPv4CIDRs: blocked})
	if err != nil {
		t.Fatal(err)
	}
	scriptPolicy += "add table inet fixture_mark\nadd chain inet fixture_mark pre { type filter hook prerouting priority -160; policy accept; }\nadd rule inet fixture_mark pre iifname \"tap0\" meta mark set 71\n"
	nft := exec.CommandContext(t.Context(), "ip", "netns", "exec", owner, "nft", "-f", "-")
	nft.Stdin = strings.NewReader(scriptPolicy)
	if raw, err := nft.CombinedOutput(); err != nil {
		t.Fatalf("install native client boundary: %v %s", err, raw)
	}
	resolverDirectory := filepath.Join("/etc/netns", guest)
	if err := os.MkdirAll(resolverDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(resolverDirectory) })
	if err := os.WriteFile(filepath.Join(resolverDirectory, "resolv.conf"), []byte("nameserver 10.0.0.2\noptions timeout:1 attempts:1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		client := exec.CommandContext(t.Context(), "ip", "netns", "exec", guest, "node", script)
		client.Env = append(networkClientEnv(directory), "NETWORK_CLIENT_CONFIG="+fixture.configFile, "NETWORK_CLIENT_SDK_ROOT="+sdkRoot, "NODE_EXTRA_CA_CERTS="+filepath.Join(directory, "ca.pem"))
		output, err := client.CombinedOutput()
		if err != nil {
			t.Fatalf("native client matrix: %v: %s", err, output)
		}
		t.Logf("native client receipt attempt %d: %s", attempt+1, output)
	}
}
