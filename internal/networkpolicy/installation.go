package networkpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"sort"
)

type Installation struct {
	PrivateBusinessCIDRs []netip.Prefix `json:"privateBusinessCidrs"`
	BlockedIPv4CIDRs     []netip.Prefix `json:"blockedIpv4Cidrs"`
}

var privateCIDRs = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16")}
var reservedCIDRs = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4")}

func contains(prefixes []netip.Prefix, ip netip.Addr) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
func covers(prefixes []netip.Prefix, p netip.Prefix) bool {
	for _, parent := range prefixes {
		if parent.Bits() <= p.Bits() && parent.Contains(p.Addr()) {
			return true
		}
	}
	if p.Bits() == 32 {
		return false
	}
	bits := p.Bits() + 1
	a := netip.PrefixFrom(p.Addr(), bits)
	v := p.Addr().As4()
	n := uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
	n |= 1 << uint(32-bits)
	b := netip.PrefixFrom(netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), bits)
	intersects := false
	for _, candidate := range prefixes {
		if candidate.Overlaps(p) {
			intersects = true
			break
		}
	}
	return intersects && covers(prefixes, a) && covers(prefixes, b)
}
func ValidateInstallation(in Installation) error {
	for _, p := range append(append([]netip.Prefix{}, in.BlockedIPv4CIDRs...), in.PrivateBusinessCIDRs...) {
		if !p.IsValid() || !p.Addr().Is4() || p != p.Masked() {
			return errors.New("installation CIDR must be canonical IPv4")
		}
	}
	for _, p := range in.PrivateBusinessCIDRs {
		if !covers(privateCIDRs, p) {
			return errors.New("private business CIDR must be RFC1918")
		}
		for _, deny := range in.BlockedIPv4CIDRs {
			if p.Overlaps(deny) {
				return errors.New("private business CIDR overlaps mandatory deny")
			}
		}
	}
	return nil
}
func Compatible(host, control Installation) bool {
	if ValidateInstallation(host) != nil || ValidateInstallation(control) != nil {
		return false
	}
	for _, p := range host.PrivateBusinessCIDRs {
		if !covers(control.PrivateBusinessCIDRs, p) {
			return false
		}
	}
	for _, p := range control.BlockedIPv4CIDRs {
		if !covers(host.BlockedIPv4CIDRs, p) {
			return false
		}
	}
	return true
}
func (in Installation) AllowsAddress(ip netip.Addr) bool {
	if !ip.Is4() || !ip.IsGlobalUnicast() || contains(in.BlockedIPv4CIDRs, ip) || contains(reservedCIDRs, ip) {
		return false
	}
	return !contains(privateCIDRs, ip) || contains(in.PrivateBusinessCIDRs, ip)
}
func (p Policy) AllowsAddress(host string, ip netip.Addr, in Installation) bool {
	if !in.AllowsAddress(ip) {
		return false
	}
	if p.Kind == Ordinary {
		return true
	}
	if p.Kind != Allowlist {
		return false
	}
	host, err := NormalizePattern(host)
	if err != nil {
		return false
	}
	for _, entry := range p.Domains {
		if !matches(entry.Domain, host) {
			continue
		}
		if len(entry.IPRanges) == 0 {
			if !ip.IsPrivate() {
				return true
			}
			continue
		}
		for _, raw := range entry.IPRanges {
			prefix, err := parsePrefix(raw)
			if err == nil && prefix.Contains(ip) {
				return true
			}
		}
	}
	return false
}
func (p Policy) ValidateInstallation(in Installation) error {
	if err := ValidateInstallation(in); err != nil {
		return err
	}
	if _, err := normalize(p, false); err != nil {
		return err
	}
	for _, entry := range p.Domains {
		for _, raw := range entry.IPRanges {
			prefix, err := parsePrefix(raw)
			if err != nil {
				return err
			}
			for _, deny := range append(append([]netip.Prefix{}, reservedCIDRs...), in.BlockedIPv4CIDRs...) {
				if deny.Overlaps(prefix) {
					return errors.New("ipRange overlaps mandatory deny")
				}
			}
			for _, private := range privateCIDRs {
				if private.Overlaps(prefix) && (!private.Contains(prefix.Addr()) || !covers(in.PrivateBusinessCIDRs, prefix)) {
					return errors.New("private ipRange exceeds installation ceiling")
				}
			}
		}
	}
	return nil
}
func (in Installation) Canonical() Installation {
	out := Installation{PrivateBusinessCIDRs: append([]netip.Prefix{}, in.PrivateBusinessCIDRs...), BlockedIPv4CIDRs: append([]netip.Prefix{}, in.BlockedIPv4CIDRs...)}
	less := func(a, b netip.Prefix) bool {
		if a.Addr() != b.Addr() {
			return a.Addr().Less(b.Addr())
		}
		return a.Bits() < b.Bits()
	}
	sort.Slice(out.PrivateBusinessCIDRs, func(i, j int) bool { return less(out.PrivateBusinessCIDRs[i], out.PrivateBusinessCIDRs[j]) })
	sort.Slice(out.BlockedIPv4CIDRs, func(i, j int) bool { return less(out.BlockedIPv4CIDRs[i], out.BlockedIPv4CIDRs[j]) })
	return out
}

func (in Installation) Digest() string {
	raw, _ := json.Marshal(in.Canonical())
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func PrivatePrefixes() []netip.Prefix  { return append([]netip.Prefix{}, privateCIDRs...) }
func ReservedPrefixes() []netip.Prefix { return append([]netip.Prefix{}, reservedCIDRs...) }

func (in Installation) MarshalJSON() ([]byte, error) {
	type wireInstallation Installation
	return json.Marshal(wireInstallation(in.Canonical()))
}
func (in *Installation) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := strict(raw, &fields); err != nil {
		return err
	}
	if len(fields) != 2 || fields["privateBusinessCidrs"] == nil || fields["blockedIpv4Cidrs"] == nil || string(fields["privateBusinessCidrs"]) == "null" || string(fields["blockedIpv4Cidrs"]) == "null" {
		return errors.New("installation network authority requires explicit prefix arrays")
	}
	type wireInstallation Installation
	var value wireInstallation
	if err := strict(raw, &value); err != nil {
		return err
	}
	result := Installation(value)
	if err := ValidateInstallation(result); err != nil {
		return err
	}
	*in = result
	return nil
}
