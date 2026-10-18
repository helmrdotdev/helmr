//go:build linux && computerproof

package controlplane

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/websocket"
)

const protectedModelIP = "93.184.216.34"
const protectedModelToken = "synthetic-protected-native-key"

type protectedModelObservation struct {
	DeniedRenewals int               `json:"deniedRenewals"`
	Operations     []int             `json:"operations"`
	Versions       []int             `json:"versions"`
	Handshakes     int               `json:"handshakes"`
	Closed         int               `json:"closed"`
	Requests       []json.RawMessage `json:"requests"`
	ActiveHTTP     int               `json:"activeHTTP"`
	HTTP           int               `json:"http"`
	Unauthorized   int               `json:"unauthorized"`
	Fallback       int               `json:"fallback"`
}

type protectedModelFixture struct {
	Origin   string `json:"origin"`
	State    string `json:"state"`
	CA       string `json:"-"`
	mu       sync.Mutex
	observed map[string]*protectedModelObservation
}

// The synthetic upstream lives in a separate network namespace, outside the
// worker host-address deny set. No fixture request leaves the disposable host.
// All other addresses remain denied by the ordinary worker egress policy.
func startProtectedModel(t *testing.T, evidence string) *protectedModelFixture {
	t.Helper()
	if os.Geteuid() != 0 || os.Getenv("HELMR_NATIVE_EXECUTION_PROOF") != "1" {
		t.Fatal("protected model requires the authorized disposable host")
	}
	namespace := fmt.Sprintf("helmr-model-%d", os.Getpid())
	hostLink := fmt.Sprintf("hpm%d", os.Getpid())
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture network %v: %v %s", args, err, out)
		}
	}
	run("netns", "add", namespace)
	t.Cleanup(func() { run("netns", "del", namespace) })
	run("link", "add", hostLink, "type", "veth", "peer", "name", "model0", "netns", namespace)
	t.Cleanup(func() { run("link", "del", hostLink) })
	run("address", "add", "93.184.216.33/30", "dev", hostLink)
	run("link", "set", hostLink, "up")
	run("-n", namespace, "address", "add", protectedModelIP+"/30", "dev", "model0")
	run("-n", namespace, "link", "set", "model0", "up")
	run("-n", namespace, "link", "set", "lo", "up")
	run("-n", namespace, "route", "add", "default", "via", "93.184.216.33")
	handle, err := ns.GetNS("/var/run/netns/" + namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	var listener net.Listener
	var resolver net.PacketConn
	if err := handle.Do(func(_ ns.NetNS) error {
		var err error
		listener, err = net.Listen("tcp4", protectedModelIP+":0")
		if err != nil {
			return err
		}
		resolver, err = net.ListenPacket("udp4", protectedModelIP+":53")
		return err
	}); err != nil {
		if listener != nil {
			listener.Close()
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	resolverDone := make(chan struct{})
	go func() { defer close(resolverDone); serveProtectedDNS(resolver) }()
	t.Cleanup(func() { resolver.Close(); <-resolverDone })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Protected native fixture"}, DNSNames: []string{"protected.fixture"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(2 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, err := tls.X509KeyPair(ca, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	f := &protectedModelFixture{State: filepath.Join(evidence, "protected-model-state.json"), CA: filepath.Join(evidence, "protected-model-ca.pem"), observed: map[string]*protectedModelObservation{}}
	if err := os.WriteFile(f.CA, ca, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	server.Listener.Close()
	server.Listener = listener
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	f.Origin = fmt.Sprintf("https://protected.fixture:%d", listener.Addr().(*net.TCPAddr).Port)
	f.record("", func(*protectedModelObservation) {})
	return f
}

func (f *protectedModelFixture) record(owner string, change func(*protectedModelObservation)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	observation := f.observed[owner]
	if observation == nil {
		observation = &protectedModelObservation{}
		f.observed[owner] = observation
	}
	change(observation)
	body, _ := json.Marshal(f.observed)
	// A single writer and atomic rename give the external driver coherent reads.
	if err := os.WriteFile(f.State+".tmp", body, 0600); err != nil {
		panic(err)
	}
	if err := os.Rename(f.State+".tmp", f.State); err != nil {
		panic(err)
	}
}

// Host-side control avoids accidentally holding later turns because a native
// client includes an earlier hold prompt in its retained conversation history.
func (f *protectedModelFixture) holding(owner string) bool {
	raw, err := os.ReadFile(f.State + ".hold")
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		panic(err)
	}
	var owners map[string]bool
	if err := json.Unmarshal(raw, &owners); err != nil {
		panic(err)
	}
	return owners[owner]
}

func (f *protectedModelFixture) serve(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	kind, owner := parts[0], parts[1]
	version := 0
	for index, token := range []string{protectedModelToken, protectedModelToken + "-rotated"} {
		if r.Header.Get("Authorization") == "Bearer "+token || r.Header.Get("X-Api-Key") == token {
			version = index + 1
		}
	}
	if version == 0 {
		f.record(owner, func(o *protectedModelObservation) { o.Unauthorized++ })
		http.Error(w, "fixture authentication failed", 401)
		return
	}
	f.record(owner, func(o *protectedModelObservation) { o.Versions = append(o.Versions, version) })
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		websocket.Server{Handshake: func(_ *websocket.Config, _ *http.Request) error { return nil }, Handler: func(c *websocket.Conn) {
			connectionIndex := 0
			f.record(owner, func(o *protectedModelObservation) {
				connectionIndex = o.Handshakes
				o.Handshakes++
				o.Operations = append(o.Operations, 0)
			})
			defer f.record(owner, func(o *protectedModelObservation) { o.Closed++ })
			defer c.Close()
			for {
				var raw string
				if err := websocket.Message.Receive(c, &raw); err != nil {
					return
				}
				var body map[string]any
				if json.Unmarshal([]byte(raw), &body) != nil {
					return
				}
				f.record(owner, func(o *protectedModelObservation) {
					o.Requests = append(o.Requests, json.RawMessage(raw))
					if body["generate"] != false {
						o.Operations[connectionIndex]++
					}
				})
				if f.holding(owner) && body["generate"] != false {
					continue
				}
				for _, event := range protectedResponses(body) {
					encoded, _ := json.Marshal(event)
					if websocket.Message.Send(c, string(encoded)) != nil {
						return
					}
				}
			}
		}}.ServeHTTP(w, r)
		return
	}
	if (kind == "codex" || kind == "opencode-openai") && strings.HasSuffix(r.URL.Path, "/responses") {
		f.record(owner, func(o *protectedModelObservation) { o.Fallback++ })
		http.Error(w, "Responses HTTP fallback is forbidden", 400)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		http.Error(w, "fixture request limit", 400)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		io.WriteString(w, `{"input_tokens":10}`)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/messages") && !strings.HasSuffix(r.URL.Path, "/responses") && !strings.HasSuffix(r.URL.Path, "/chat/completions") {
		io.WriteString(w, `{"data":[]}`)
		return
	}
	f.record(owner, func(o *protectedModelObservation) {
		o.HTTP++
		o.ActiveHTTP++
		o.Requests = append(o.Requests, json.RawMessage(body))
	})
	defer f.record(owner, func(o *protectedModelObservation) { o.ActiveHTTP-- })
	if f.holding(owner) {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		http.Error(w, "invalid fixture body", 400)
		return
	}
	var events []map[string]any
	if strings.HasSuffix(r.URL.Path, "/messages") {
		events = protectedAnthropic()
	} else if strings.HasSuffix(r.URL.Path, "/responses") {
		events = protectedResponses(request)
	} else {
		events = protectedChat()
	}
	for _, event := range events {
		raw, _ := json.Marshal(event)
		if strings.HasSuffix(r.URL.Path, "/messages") {
			fmt.Fprintf(w, "event: %s\n", event["type"])
		}
		fmt.Fprintf(w, "data: %s\n\n", raw)
	}
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		io.WriteString(w, "data: [DONE]\n\n")
	}
}

func protectedResponses(body map[string]any) []map[string]any {
	id := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	output := []any{}
	if body["generate"] != false {
		output = append(output, map[string]any{"type": "message", "role": "assistant", "id": "msg_" + id, "content": []any{map[string]any{"type": "output_text", "text": "protected fixture reply", "annotations": []any{}}}})
	}
	events := []map[string]any{{"type": "response.created", "response": map[string]any{"id": id, "status": "in_progress", "output": []any{}}}}
	for _, item := range output {
		message := item.(map[string]any)
		events = append(events,
			map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "role": "assistant", "id": message["id"], "status": "in_progress", "content": []any{}}},
			map[string]any{"type": "response.content_part.added", "item_id": message["id"], "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}},
			map[string]any{"type": "response.output_text.delta", "item_id": message["id"], "output_index": 0, "content_index": 0, "delta": "protected fixture reply"},
			map[string]any{"type": "response.output_text.done", "item_id": message["id"], "output_index": 0, "content_index": 0, "text": "protected fixture reply"},
			map[string]any{"type": "response.content_part.done", "item_id": message["id"], "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "protected fixture reply", "annotations": []any{}}},
			map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item},
		)
	}
	return append(events, map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 2, "total_tokens": 12}}})
}
func protectedAnthropic() []map[string]any {
	return []map[string]any{
		{"type": "message_start", "message": map[string]any{"id": fmt.Sprintf("msg_%d", time.Now().UnixNano()), "type": "message", "role": "assistant", "model": "probe", "content": []any{}, "stop_reason": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 1}}},
		{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "protected fixture reply"}},
		{"type": "content_block_stop", "index": 0},
		{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 2}},
		{"type": "message_stop"},
	}
}
func protectedChat() []map[string]any {
	return []map[string]any{
		{"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1, "model": "probe", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": "protected fixture reply"}, "finish_reason": nil}}},
		{"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1, "model": "probe", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}},
	}
}

// A complement of one local fixture address preserves the proof's deny-all
// posture for every other guest destination without changing production code.
func protectedModelBlocked() string {
	address := netip.MustParseAddr(protectedModelIP)
	prefix := netip.PrefixFrom(address, 32)
	var blocked []string
	for bits := 32; bits > 0; bits-- {
		raw := prefix.Addr().As4()
		bit := bits - 1
		raw[bit/8] ^= 1 << uint(7-bit%8)
		blocked = append(blocked, netip.PrefixFrom(netip.AddrFrom4(raw), bits).Masked().String())
		prefix = netip.PrefixFrom(address, bits-1).Masked()
	}
	raw, _ := json.Marshal(blocked)
	return string(raw)
}

// Resolve only the fixture hostname; never forward guest DNS to the Internet.
func serveProtectedDNS(conn net.PacketConn) {
	buffer := make([]byte, 4096)
	for {
		n, peer, err := conn.ReadFrom(buffer)
		if err != nil {
			return
		}
		var query dnsmessage.Message
		if query.Unpack(buffer[:n]) != nil || query.Header.Response || len(query.Questions) != 1 {
			continue
		}
		question := query.Questions[0]
		answer := dnsmessage.Message{Header: dnsmessage.Header{ID: query.Header.ID, Response: true, Authoritative: true, RecursionDesired: query.Header.RecursionDesired}, Questions: query.Questions}
		if strings.EqualFold(question.Name.String(), "protected.fixture.") {
			if question.Type == dnsmessage.TypeA && question.Class == dnsmessage.ClassINET {
				answer.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 1}, Body: &dnsmessage.AResource{A: netip.MustParseAddr(protectedModelIP).As4()}}}
			}
		} else {
			answer.Header.RCode = dnsmessage.RCodeNameError
		}
		packet, err := answer.Pack()
		if err == nil {
			_, _ = conn.WriteTo(packet, peer)
		}
	}
}

// Simulate control-plane unavailability from this disposable Worker's point of
// view. Public client routes remain available to observe terminal state. The
// driver enables this only when the single-slot Worker owns the authority case.
func (f *protectedModelFixture) blockWorkerControl(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/worker/") {
		return false
	}
	selected, err := os.ReadFile(f.State + ".lease-outage")
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		http.Error(w, "fixture control unavailable", 500)
		return true
	}
	if len(selected) == 0 {
		return false
	}
	if r.URL.Path == "/worker/v1/agent-computers/lease/renew" {
		f.record("lease-expiry", func(o *protectedModelObservation) { o.DeniedRenewals++ })
	}
	http.Error(w, "fixture worker control outage", http.StatusServiceUnavailable)
	return true
}
