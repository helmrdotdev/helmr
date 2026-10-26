// Package mcppolicy validates immutable Runtime MCP operation grants.
package mcppolicy

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
)

var ErrInvalid = errors.New("invalid runtime MCP policy")

type Rule struct {
	Operation string   `json:"operation"`
	Agents    []string `json:"agents,omitempty"`
	Computer  string   `json:"computer,omitempty"`
}

// UnmarshalJSON preserves field presence so irrelevant null/empty members cannot
// disappear into Go zero values before operation-specific validation.
func (r *Rule) UnmarshalJSON(raw []byte) error {
	type plain Rule
	var fields map[string]json.RawMessage
	if err := strict(raw, &fields); err != nil {
		return err
	}
	var value plain
	if err := strict(raw, &value); err != nil {
		return err
	}
	if err := operationError(value.Operation); err != nil {
		return err
	}
	_, agents := fields["agents"]
	_, computer := fields["computer"]
	if value.Operation == "list_computers" {
		if agents || computer {
			return fmt.Errorf("%w: list_computers is targetless", ErrInvalid)
		}
	} else {
		if !agents || bytes.Equal(bytes.TrimSpace(fields["agents"]), []byte("null")) {
			return ErrInvalid
		}
		if value.Operation == "start" || value.Operation == "spawn" {
			if computer && value.Computer != "new" && value.Computer != "caller" {
				return ErrInvalid
			}
		} else if computer {
			return fmt.Errorf("%w: irrelevant computer member", ErrInvalid)
		}
	}
	*r = Rule(value)
	return nil
}

var validAgentKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Policy is a normalized private storage representation. A zero value is invalid.
type Policy struct {
	Kind  string `json:"kind"`
	Rules []Rule `json:"rules,omitempty"`
}

func (p Policy) MarshalJSON() ([]byte, error) {
	if p.Kind == "allow" {
		return json.Marshal(struct {
			Kind  string `json:"kind"`
			Rules []Rule `json:"rules"`
		}{p.Kind, append([]Rule{}, p.Rules...)})
	}
	return json.Marshal(struct {
		Kind string `json:"kind"`
	}{p.Kind})
}
func strict(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrInvalid)
	}
	// Duplicate members are ambiguous even when encoding/json would accept them.
	var walk func() error
	tokens := json.NewDecoder(bytes.NewReader(raw))
	walk = func() error {
		t, e := tokens.Token()
		if e != nil {
			return e
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for tokens.More() {
					key, e := tokens.Token()
					if e != nil {
						return e
					}
					s := key.(string)
					if seen[s] {
						return ErrInvalid
					}
					seen[s] = true
					if e = walk(); e != nil {
						return e
					}
				}
				_, e = tokens.Token()
				return e
			case '[':
				for tokens.More() {
					if e = walk(); e != nil {
						return e
					}
				}
				_, e = tokens.Token()
				return e
			}
		}
		return nil
	}
	if err := walk(); err != nil {
		return fmt.Errorf("%w: ambiguous JSON", ErrInvalid)
	}
	return nil
}
func known(op string) bool {
	switch op {
	case "start", "spawn", "enqueue", "send_turn", "interrupt_turn", "interrupt_session", "resume_session", "close_session", "cancel_session", "inspect_session", "inspect_turn", "list_computers":
		return true
	}
	return false
}

// operationError is shared by JSON decoding and canonical normalization so
// derived operations retain useful guidance even when no agents were supplied.
func operationError(operation string) error {
	if known(operation) {
		return nil
	}
	hint := "unknown operation"
	switch operation {
	case "wait_turn":
		hint = "wait_turn is derived from inspect_turn; author inspect_turn instead"
	case "list_sessions":
		hint = "list_sessions is derived from inspect_session; author inspect_session instead"
	case "list_agents":
		hint = "list_agents is derived from start/spawn grants; author start or spawn instead"
	case "skills":
		hint = "skills is local guidance derived from descriptor capabilities, not an authored business operation"
	}
	return fmt.Errorf("%w: operation: %s", ErrInvalid, hint)
}

// Normalize converts public authoring JSON. Only absent bytes mean authoring omission.
// nil agentKeys validates syntax; a non-nil list additionally resolves every exact key.
func Normalize(raw json.RawMessage, agentKeys []string) (Policy, error) {
	if len(raw) == 0 {
		return Policy{Kind: "unrestricted"}, nil
	}
	var value struct {
		Allow *[]Rule `json:"allow"`
	}
	if err := strict(raw, &value); err != nil {
		return Policy{}, err
	}
	if value.Allow == nil {
		return Policy{}, fmt.Errorf("%w: allow is required", ErrInvalid)
	}
	return normalizeRules(*value.Allow, agentKeys)
}
func normalizeRules(rules []Rule, keys []string) (Policy, error) {
	if len(rules) > 256 {
		return Policy{}, fmt.Errorf("%w: at most 256 rules", ErrInvalid)
	}
	available := map[string]bool{}
	for _, key := range keys {
		available[key] = true
	}
	groups := map[string]map[string]bool{}
	computers := false
	for i, r := range rules {
		if err := operationError(r.Operation); err != nil {
			return Policy{}, fmt.Errorf("allow[%d]: %w", i, err)
		}
		if r.Operation == "list_computers" {
			if r.Agents != nil || r.Computer != "" {
				return Policy{}, fmt.Errorf("%w: list_computers is targetless", ErrInvalid)
			}
			computers = true
			continue
		}
		if len(r.Agents) == 0 || len(r.Agents) > 256 {
			return Policy{}, fmt.Errorf("%w: allow[%d].agents requires 1–256 keys", ErrInvalid, i)
		}
		if r.Operation == "start" || r.Operation == "spawn" {
			if r.Computer == "" {
				r.Computer = "new"
			}
			if r.Computer != "new" && r.Computer != "caller" {
				return Policy{}, fmt.Errorf("%w: invalid creation placement", ErrInvalid)
			}
		} else if r.Computer != "" {
			return Policy{}, fmt.Errorf("%w: computer is only valid for creation", ErrInvalid)
		}
		group := r.Operation + "\x00" + r.Computer
		if groups[group] == nil {
			groups[group] = map[string]bool{}
		}
		seen := map[string]bool{}
		for _, key := range r.Agents {
			if !validAgentKey.MatchString(key) || seen[key] || (keys != nil && !available[key]) {
				return Policy{}, fmt.Errorf("%w: allow[%d].agents contains an unknown or duplicate exact key", ErrInvalid, i)
			}
			seen[key] = true
			groups[group][key] = true
		}
	}
	result := Policy{Kind: "allow", Rules: []Rule{}}
	names := []string{}
	for group := range groups {
		names = append(names, group)
	}
	if computers {
		names = append(names, "list_computers\x00")
	}
	sort.Strings(names)
	for _, group := range names {
		parts := bytes.Split([]byte(group), []byte{0})
		op, placement := string(parts[0]), string(parts[1])
		if op == "list_computers" {
			result.Rules = append(result.Rules, Rule{Operation: op})
			continue
		}
		agents := []string{}
		for key := range groups[group] {
			agents = append(agents, key)
		}
		sort.Strings(agents)
		for len(agents) > 0 {
			n := min(256, len(agents))
			result.Rules = append(result.Rules, Rule{Operation: op, Agents: append([]string{}, agents[:n]...), Computer: placement})
			agents = agents[n:]
		}
	}
	return result, nil
}

// Decode rejects missing, malformed or noncanonical trusted storage; it never supplies authority.
func Decode(raw []byte) (Policy, error) {
	var fields map[string]json.RawMessage
	if err := strict(raw, &fields); err != nil {
		return Policy{}, err
	}
	if rules, ok := fields["rules"]; ok && bytes.Equal(bytes.TrimSpace(rules), []byte("null")) {
		return Policy{}, ErrInvalid
	}
	var value struct {
		Kind  string  `json:"kind"`
		Rules *[]Rule `json:"rules,omitempty"`
	}
	if err := strict(raw, &value); err != nil {
		return Policy{}, err
	}
	if value.Kind == "unrestricted" && value.Rules == nil {
		return Policy{Kind: "unrestricted"}, nil
	}
	if value.Kind != "allow" || value.Rules == nil {
		return Policy{}, ErrInvalid
	}
	p, err := normalizeRules(*value.Rules, nil)
	if err != nil {
		return Policy{}, err
	}
	original := Policy{Kind: value.Kind, Rules: *value.Rules}
	a, _ := json.Marshal(p)
	b, _ := json.Marshal(original)
	if !bytes.Equal(a, b) {
		return Policy{}, fmt.Errorf("%w: noncanonical stored rules", ErrInvalid)
	}
	return p, nil
}
func (p Policy) Bytes() []byte           { raw, _ := json.Marshal(p); return raw }
func (p Policy) Digest() [32]byte        { return sha256.Sum256(p.Bytes()) }
func (p Policy) Equal(other Policy) bool { return bytes.Equal(p.Bytes(), other.Bytes()) }
func (p Policy) SubsetOf(maximum Policy) bool {
	if maximum.Kind == "unrestricted" {
		return p.Kind == "allow" || p.Kind == "unrestricted"
	}
	if p.Kind != "allow" || maximum.Kind != "allow" {
		return false
	}
	for _, r := range p.Rules {
		if r.Operation == "list_computers" {
			if !maximum.Allows(r.Operation, "", "") {
				return false
			}
		}
		for _, a := range r.Agents {
			if !maximum.Allows(r.Operation, a, r.Computer) {
				return false
			}
		}
	}
	return true
}
func (p Policy) Allows(operation, agent, computer string) bool {
	if !known(operation) {
		return false
	}
	if p.Kind == "unrestricted" {
		return true
	}
	if p.Kind != "allow" {
		return false
	}
	for _, r := range p.Rules {
		if r.Operation != operation || r.Computer != computer {
			continue
		}
		if operation == "list_computers" {
			return true
		}
		for _, key := range r.Agents {
			if key == agent {
				return true
			}
		}
	}
	return false
}
func (p Policy) Has(operation string) bool {
	if p.Kind == "unrestricted" {
		return known(operation)
	}
	for _, r := range p.Rules {
		if r.Operation == operation {
			return true
		}
	}
	return false
}

// Public returns nil only for a valid symbolic unrestricted policy.
func (p Policy) Public() json.RawMessage {
	if p.Kind == "unrestricted" {
		return nil
	}
	raw, _ := json.Marshal(struct {
		Allow []Rule `json:"allow"`
	}{append([]Rule{}, p.Rules...)})
	return raw
}

// Targets returns the exact Agent union for an operation across placements.
// Unrestricted remains symbolic and returns nil.
func (p Policy) Targets(operation string) []string {
	if p.Kind == "unrestricted" {
		return nil
	}
	found := map[string]bool{}
	for _, rule := range p.Rules {
		if rule.Operation == operation {
			for _, key := range rule.Agents {
				found[key] = true
			}
		}
	}
	targets := []string{}
	for key := range found {
		targets = append(targets, key)
	}
	sort.Strings(targets)
	return targets
}
