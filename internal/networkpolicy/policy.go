// Package networkpolicy owns outbound authority shared by publication and execution.
package networkpolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"sort"
	"strings"
)

type Kind string

const (
	Ordinary  Kind = "ordinary"
	Blocked   Kind = "block"
	Allowlist Kind = "allow"
)

type Domain struct {
	Domain   string   `json:"domain"`
	IPRanges []string `json:"ipRanges,omitempty"`
}
type Policy struct {
	Kind    Kind     `json:"kind"`
	Domains []Domain `json:"domains,omitempty"`
}

func BlockedPolicy() Policy  { return Policy{Kind: Blocked} }
func OrdinaryPolicy() Policy { return Policy{Kind: Ordinary} }
func strict(raw []byte, value any) error {
	if err := uniqueMembers(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
func ParsePublic(raw json.RawMessage) (Policy, error) {
	if len(raw) == 0 {
		return OrdinaryPolicy(), nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Policy{}, errors.New("network policy cannot be null")
	}
	var fields map[string]json.RawMessage
	if err := strict(raw, &fields); err != nil {
		return Policy{}, err
	}
	if len(fields) != 1 {
		return Policy{}, errors.New("network policy requires exactly one of blockAll or allowDomains")
	}
	if value, ok := fields["blockAll"]; ok {
		var block bool
		if string(value) == "null" {
			return Policy{}, errors.New("blockAll must be boolean")
		}
		if err := strict(value, &block); err != nil {
			return Policy{}, err
		}
		if block {
			return BlockedPolicy(), nil
		}
		return OrdinaryPolicy(), nil
	}
	value, ok := fields["allowDomains"]
	if !ok || string(value) == "null" {
		return Policy{}, errors.New("allowDomains must be an array")
	}
	var entries []json.RawMessage
	if err := strict(value, &entries); err != nil {
		return Policy{}, err
	}
	p := Policy{Kind: Allowlist}
	for _, entry := range entries {
		var name string
		if json.Unmarshal(entry, &name) == nil {
			p.Domains = append(p.Domains, Domain{Domain: name})
			continue
		}
		var object struct {
			Domain   string    `json:"domain"`
			IPRanges *[]string `json:"ipRanges"`
		}
		if err := strict(entry, &object); err != nil {
			return Policy{}, err
		}
		if object.IPRanges == nil || len(*object.IPRanges) == 0 {
			return Policy{}, errors.New("structured domain requires nonempty ipRanges")
		}
		p.Domains = append(p.Domains, Domain{Domain: object.Domain, IPRanges: *object.IPRanges})
	}
	return normalize(p, true)
}
func DecodeStored(raw json.RawMessage) (Policy, error) {
	type storedPolicy Policy
	var wire storedPolicy
	var p Policy
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return p, errors.New("missing stored network authority")
	}
	if err := strict(raw, &wire); err != nil {
		return p, err
	}
	p = Policy(wire)
	n, err := normalize(p, false)
	if err != nil {
		return Policy{}, err
	}
	canonical, _ := json.Marshal(n)
	var a, b any
	_ = json.Unmarshal(raw, &a)
	_ = json.Unmarshal(canonical, &b)
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	if !bytes.Equal(ra, rb) {
		return Policy{}, errors.New("stored network authority is not canonical")
	}
	return n, nil
}
func normalize(p Policy, empty bool) (Policy, error) {
	p.Domains = append([]Domain{}, p.Domains...)
	for i := range p.Domains {
		p.Domains[i].IPRanges = append([]string{}, p.Domains[i].IPRanges...)
	}
	switch p.Kind {
	case Ordinary, Blocked:
		if len(p.Domains) != 0 {
			return Policy{}, errors.New("non-allow policy contains domains")
		}
		return Policy{Kind: p.Kind}, nil
	case Allowlist:
	default:
		return Policy{}, errors.New("unknown network policy kind")
	}
	if len(p.Domains) > 256 {
		return Policy{}, errors.New("too many domains")
	}
	if len(p.Domains) == 0 {
		if empty {
			return BlockedPolicy(), nil
		}
		return Policy{}, errors.New("stored allow policy is empty")
	}
	seen := map[string]bool{}
	for i := range p.Domains {
		d := &p.Domains[i]
		name, err := NormalizePattern(d.Domain)
		if err != nil {
			return Policy{}, err
		}
		d.Domain = name
		if seen[name] {
			return Policy{}, errors.New("duplicate domain pattern")
		}
		seen[name] = true
		if len(d.IPRanges) > 64 {
			return Policy{}, errors.New("too many ipRanges")
		}
		ranges := map[string]bool{}
		for _, value := range d.IPRanges {
			prefix, err := parsePrefix(value)
			if err != nil {
				return Policy{}, err
			}
			if ranges[prefix.String()] {
				return Policy{}, errors.New("duplicate ipRange")
			}
			ranges[value] = true
		}
		sort.Strings(d.IPRanges)
	}
	sort.Slice(p.Domains, func(i, j int) bool { return p.Domains[i].Domain < p.Domains[j].Domain })
	return p, nil
}
func NormalizePattern(value string) (string, error) {
	for _, r := range value {
		if r > 127 {
			return "", errors.New("domain must use ASCII A-labels")
		}
	}
	wild := strings.HasPrefix(value, "*.")
	host := value
	if wild {
		host = host[2:]
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if len(host) < 1 || len(host)+boolInt(wild)*2 > 253 || !strings.Contains(host, ".") || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", errors.New("invalid domain pattern")
	}
	numeric := true
	for _, r := range host {
		if r < '0' || r > '9' {
			if r != '.' {
				numeric = false
			}
		}
	}
	if numeric {
		return "", errors.New("numeric domain is invalid")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid domain label")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", errors.New("invalid domain character")
			}
		}
	}
	if wild {
		return "*." + host, nil
	}
	return host, nil
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func (p Policy) Public() json.RawMessage {
	if p.Kind == Ordinary {
		return json.RawMessage(`{"blockAll":false}`)
	}
	if p.Kind == Blocked {
		return json.RawMessage(`{"blockAll":true}`)
	}
	entries := make([]any, 0, len(p.Domains))
	for _, d := range p.Domains {
		if len(d.IPRanges) == 0 {
			entries = append(entries, d.Domain)
		} else {
			entries = append(entries, d)
		}
	}
	raw, _ := json.Marshal(struct {
		AllowDomains []any `json:"allowDomains"`
	}{entries})
	return raw
}
func (p Policy) Digest() string {
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func matches(pattern, host string) bool {
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[1:]
		return len(host) > len(suffix) && strings.HasSuffix(host, suffix)
	}
	return pattern == host
}
func (p Policy) AllowsHostname(host string) bool {
	normalized, err := NormalizePattern(host)
	if err != nil || strings.HasPrefix(normalized, "*.") {
		return false
	}
	if p.Kind == Ordinary {
		return true
	}
	for _, d := range p.Domains {
		if matches(d.Domain, normalized) {
			return true
		}
	}
	return false
}
func (p Policy) ValidateProtectedOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("protected origin must be an exact HTTPS origin")
	}
	if p.Kind == Blocked {
		return errors.New("protected origin blocked by network policy")
	}
	if p.Kind == Allowlist && (u.Port() != "" && u.Port() != "443" || !p.AllowsHostname(u.Hostname())) {
		return errors.New("protected origin not allowed by network policy")
	}
	return nil
}
func parsePrefix(value string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(value)
	if err != nil || !p.Addr().Is4() || p != p.Masked() {
		return netip.Prefix{}, fmt.Errorf("ipRange %q must be canonical IPv4 CIDR", value)
	}
	return p, nil
}

// Validate requires the explicit canonical trusted representation.
func (p Policy) Validate() error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = DecodeStored(raw)
	return err
}
func uniqueMembers(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var scan func() error
	scan = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON member")
				}
				seen[name] = true
				if err := scan(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := scan(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON container")
		}
		_, err = decoder.Token()
		return err
	}
	return scan()
}

func (p *Policy) UnmarshalJSON(raw []byte) error {
	value, err := DecodeStored(raw)
	if err != nil {
		return err
	}
	*p = value
	return nil
}
