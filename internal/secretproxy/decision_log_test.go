package secretproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/time/rate"
)

func TestNetworkDecisionEventsCarrySafeAllocationIdentity(t *testing.T) {
	var buffer bytes.Buffer
	policy := networkpolicy.Policy{Kind: networkpolicy.Allowlist, Domains: []networkpolicy.Domain{{Domain: "api.example.com"}}}
	decisions := NewDecisionLog(slog.New(slog.NewJSONHandler(&buffer, nil)), "actual-computer", "allocation", "instance", "physical-owner", networkpolicy.PhaseRuntime, policy)
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("8.8.8.8")}, nil
	}
	proxy, err := New(Config{NetworkPolicy: policy, Decisions: decisions, LookupIP: lookup, AllowedDestination: func(netip.Addr) bool { return true }, DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("SECRET-TRANSPORT-ERROR")
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	_, _ = proxy.dialHostname(t.Context(), "api.example.com", 443)
	_, _ = proxy.dialHostname(t.Context(), "denied.example.com", 443)
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		proxy.serveRestricted(server, netip.MustParseAddrPort("8.8.4.4:443"))
	}()
	_, _ = client.Write([]byte("SECRET-PAYLOAD"))
	client.Close()
	<-done
	dns := &DNS{policy: policy, lookup: lookup, decisions: decisions}
	for _, host := range []string{"api.example.com.", "denied.example.com."} {
		name, _ := dnsmessage.NewName(host)
		builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 1})
		_ = builder.StartQuestions()
		_ = builder.Question(dnsmessage.Question{Name: name, Class: dnsmessage.ClassINET, Type: dnsmessage.TypeA})
		packet, _ := builder.Finish()
		if dns.Reply(t.Context(), packet) == nil {
			t.Fatal("expected DNS reply")
		}
	}
	if strings.Contains(buffer.String(), "SECRET") {
		t.Fatal("diagnostics included transport error or network payload")
	}
	found := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"computer_id": "actual-computer", "allocation_id": "allocation", "owner_kind": "instance", "owner_id": "physical-owner", "phase": "runtime", "policy_id": policy.Digest()} {
			if event[key] != want {
				t.Fatalf("%s lost trusted identity: %+v", key, event)
			}
		}
		if event["session_id"] != nil || event["turn_id"] != nil {
			t.Fatal("invented Session/Turn attribution")
		}
		found[event["protocol"].(string)+":"+event["decision"].(string)+":"+event["reason"].(string)] = true
	}
	for _, want := range []string{"tls:allow:destination_allowed", "tls:deny:address_denied", "tls:deny:hostname_denied", "tls:deny:client_hello_rejected", "dns:allow:address_allowed", "dns:deny:address_denied", "dns:deny:query_denied"} {
		if !found[want] {
			t.Fatalf("missing %s in %s", want, buffer.String())
		}
	}
}

func TestPreparationDecisionEventsDoNotClaimComputerAndAreBounded(t *testing.T) {
	var buffer bytes.Buffer
	policy := networkpolicy.BlockedPolicy()
	decisions := NewDecisionLog(slog.New(slog.NewJSONHandler(&buffer, nil)), "preparation-owner-is-not-computer", "prep-allocation", "instance", "prep-physical", networkpolicy.PhasePreparation, policy)
	decisions.denied = rate.NewLimiter(0, 2)
	for range 10 {
		decisions.record(t.Context(), "dns", "api.example.com", netip.Addr{}, 53, false, "query_denied")
	}
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("unbounded diagnostics: %d", len(lines))
	}
	for _, line := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event["computer_id"] != nil || event["session_id"] != nil || event["turn_id"] != nil || event["owner_id"] != "prep-physical" || event["phase"] != "preparation" {
			t.Fatalf("false preparation attribution: %s", line)
		}
	}
}

func TestAllowedFloodCannotSuppressDeniedEvidence(t *testing.T) {
	var buffer bytes.Buffer
	decisions := NewDecisionLog(slog.New(slog.NewJSONHandler(&buffer, nil)), "computer", "allocation", "instance", "owner", networkpolicy.PhaseRuntime, networkpolicy.OrdinaryPolicy())
	decisions.allowed = rate.NewLimiter(0, 1)
	decisions.denied = rate.NewLimiter(0, 1)
	for range 101 {
		decisions.record(t.Context(), "dns", "api.example.com", netip.Addr{}, 53, true, "address_allowed")
	}
	decisions.record(t.Context(), "tls", "denied.example.com", netip.Addr{}, 443, false, "hostname_denied")
	for range 100 {
		decisions.record(t.Context(), "tls", "denied.example.com", netip.Addr{}, 443, false, "hostname_denied")
	}
	// A later admitted record reports suppressed denies without an extra queue,
	// timer or unbounded per-destination state.
	decisions.allowed = rate.NewLimiter(0, 1)
	decisions.record(t.Context(), "dns", "api.example.com", netip.Addr{}, 53, true, "address_allowed")
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("unexpected admitted record count %d", len(lines))
	}
	var denial, summary map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &denial); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &summary); err != nil {
		t.Fatal(err)
	}
	if denial["decision"] != "deny" || denial["reason"] != "hostname_denied" || denial["suppressed_allow"] != float64(100) || denial["suppressed_deny"] != float64(0) {
		t.Fatalf("allowed flood hid denial or its suppression evidence: %+v", denial)
	}
	if summary["suppressed_deny"] != float64(100) || summary["suppressed_allow"] != float64(0) {
		t.Fatalf("denial suppression was not counted: %+v", summary)
	}
}
