package secretproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"golang.org/x/net/dns/dnsmessage"
)

func TestRestrictedProtectedAndEndToEndTLS(t *testing.T) {
	for _, protected := range []bool{true, false} {
		t.Run(map[bool]string{true: "protected", false: "end_to_end"}[protected], func(t *testing.T) {
			var lookup, dials atomic.Int32
			f := newFixtureProtocols(t, 443, false, nil, func(config *Config) {
				config.NetworkPolicy = networkpolicy.Policy{Kind: networkpolicy.Allowlist, Domains: []networkpolicy.Domain{{Domain: "api.github.com"}}}
				config.LookupIP = func(ctx context.Context, network, name string) ([]netip.Addr, error) {
					lookup.Add(1)
					if network != "ip4" || name != "api.github.com." {
						t.Errorf("untrusted DNS authority %q %q", network, name)
					}
					return []netip.Addr{netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("93.184.216.35")}, nil
				}
				dial := config.DialContext
				config.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					dials.Add(1)
					if address != "93.184.216.35:443" {
						t.Errorf("dial used guest/unvalidated authority: %q", address)
					}
					return dial(ctx, network, address)
				}
				if !protected {
					config.Origins = nil
				}
			})
			if !protected {
				f.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: f.proxy.config.UpstreamTLS.RootCAs}
			}
			if request(t, f, "https://api.github.com/", "") != 200 {
				t.Fatal("allowed HTTPS failed")
			}
			if lookup.Load() != 1 || dials.Load() != 1 {
				t.Fatalf("lookups/dials %d/%d", lookup.Load(), dials.Load())
			}
		})
	}
}

func TestRestrictedRejectsBeforeDNSAndDial(t *testing.T) {
	for _, payload := range [][]byte{[]byte("GET / HTTP/1.1\r\n\r\n"), record(testHello("denied.example.com", false)), record(testHello("api.github.com", true)), record(testHello("", false))} {
		var lookup, dials atomic.Int32
		p, err := New(Config{NetworkPolicy: networkpolicy.Policy{Kind: networkpolicy.Allowlist, Domains: []networkpolicy.Domain{{Domain: "api.github.com"}}}, LookupIP: func(context.Context, string, string) ([]netip.Addr, error) {
			lookup.Add(1)
			return nil, errors.New("unexpected DNS")
		}, AllowedDestination: func(netip.Addr) bool { return true }, DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected dial")
		}})
		if err != nil {
			t.Fatal(err)
		}
		client, server := net.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer server.Close()
			p.serveRestricted(server, netip.MustParseAddrPort("8.8.8.8:443"))
		}()
		_, _ = client.Write(payload)
		client.Close()
		<-done
		p.Close()
		if lookup.Load() != 0 || dials.Load() != 0 {
			t.Fatal("denied flow contacted upstream")
		}
	}
}

func TestSelectiveDNSUsesSamePerEntryAddressDecision(t *testing.T) {
	var lookups atomic.Int32
	d := &DNS{policy: networkpolicy.Policy{Kind: networkpolicy.Allowlist, Domains: []networkpolicy.Domain{{Domain: "*.example.com"}, {Domain: "private.example.net", IPRanges: []string{"10.60.0.0/16"}}}}, installation: networkpolicy.Installation{PrivateBusinessCIDRs: []netip.Prefix{netip.MustParsePrefix("10.60.0.0/16")}}, lookup: func(ctx context.Context, network, name string) ([]netip.Addr, error) {
		lookups.Add(1)
		if network != "ip4" || !strings.HasSuffix(name, ".") {
			t.Fatal("non-absolute IPv4 lookup")
		}
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.60.1.1"), netip.MustParseAddr("169.254.169.254")}, nil
	}}
	for _, c := range []struct {
		host   string
		kind   dnsmessage.Type
		code   dnsmessage.RCode
		answer string
		lookup bool
	}{{"api.example.com", dnsmessage.TypeA, 0, "8.8.8.8", true}, {"private.example.net", dnsmessage.TypeA, 0, "10.60.1.1", true}, {"api.example.com", dnsmessage.TypeAAAA, 0, "", false}, {"example.com", dnsmessage.TypeA, dnsmessage.RCodeRefused, "", false}, {"api.example.com", dnsmessage.TypeTXT, dnsmessage.RCodeRefused, "", false}} {
		before := lookups.Load()
		name, _ := dnsmessage.NewName(c.host + ".")
		builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
		_ = builder.StartQuestions()
		_ = builder.Question(dnsmessage.Question{Name: name, Type: c.kind, Class: dnsmessage.ClassINET})
		query, _ := builder.Finish()
		raw := d.Reply(t.Context(), query)
		var message dnsmessage.Message
		if err := message.Unpack(raw); err != nil {
			t.Fatal(err)
		}
		if message.Header.RCode != c.code || (lookups.Load() > before) != c.lookup {
			t.Fatalf("%s %d response %+v", c.host, c.kind, message)
		}
		if c.answer == "" {
			if len(message.Answers) != 0 {
				t.Fatal("unexpected DNS answer")
			}
		} else {
			if len(message.Answers) != 1 || netip.AddrFrom4(message.Answers[0].Body.(*dnsmessage.AResource).A).String() != c.answer {
				t.Fatalf("bad addresses %+v", message.Answers)
			}
		}
	}
}
