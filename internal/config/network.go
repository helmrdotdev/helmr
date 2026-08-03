package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

func parseCanonicalBlockedIPv4Prefixes(raw string) ([]netip.Prefix, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("blocked IPv4 prefix JSON is required")
	}
	var values []string
	decoder := json.NewDecoder(strings.NewReader(raw))
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode blocked IPv4 prefix JSON: %w", err)
	}
	if values == nil {
		return nil, errors.New("blocked IPv4 prefixes must be a JSON array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("blocked IPv4 prefix JSON has trailing data")
	}
	prefixes := make([]netip.Prefix, len(values))
	for index, value := range values {
		if value != strings.TrimSpace(value) {
			return nil, fmt.Errorf("blocked IPv4 prefix %q has surrounding whitespace", value)
		}
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() || prefix.String() != value {
			return nil, fmt.Errorf("blocked IPv4 prefix %q is not canonical IPv4 CIDR", value)
		}
		if index > 0 && comparePrefixes(prefixes[index-1], prefix) >= 0 {
			return nil, errors.New("blocked IPv4 prefixes must be unique and canonically ordered")
		}
		prefixes[index] = prefix
	}
	return prefixes, nil
}

func comparePrefixes(left, right netip.Prefix) int {
	if comparison := left.Addr().Compare(right.Addr()); comparison != 0 {
		return comparison
	}
	if left.Bits() < right.Bits() {
		return -1
	}
	if left.Bits() > right.Bits() {
		return 1
	}
	return 0
}
