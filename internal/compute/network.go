package compute

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

type NetworkPolicy struct {
	Internet bool     `json:"internet"`
	Allow    []string `json:"allow,omitempty"`
	Deny     []string `json:"deny,omitempty"`
}

func DefaultNetworkPolicy() NetworkPolicy {
	return NetworkPolicy{Internet: true}
}

func (p NetworkPolicy) Validate() error {
	var problems []error
	for _, entry := range p.Allow {
		if strings.TrimSpace(entry) == "" {
			problems = append(problems, errors.New("network allow entries must not be empty"))
		}
	}
	for _, entry := range p.Deny {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			problems = append(problems, errors.New("network deny entries must not be empty"))
			continue
		}
		if _, err := netip.ParsePrefix(entry); err != nil {
			problems = append(problems, fmt.Errorf("network deny entry %q must be a CIDR prefix: %w", entry, err))
		}
	}
	return errors.Join(problems...)
}
