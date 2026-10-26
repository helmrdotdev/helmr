package definition

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

var ErrInvalidNetworkPolicy = errors.New("invalid_network_policy")
var ErrInvalidRuntimeMcpPolicy = errors.New("invalid_runtime_mcp_policy")

// ResolveTemplatePolicies validates authored phase settings and returns explicit
// authority for runtime and preparation. Seed-only preparation always blocks.
func ResolveTemplatePolicies(network, preparation json.RawMessage, prepare bool, secrets, prepareSecrets []SecretBinding) (networkpolicy.Policy, networkpolicy.Policy, error) {
	runtime, err := networkpolicy.ParsePublic(network)
	if err != nil {
		return networkpolicy.Policy{}, networkpolicy.Policy{}, fmt.Errorf("%w network: %w", ErrInvalidNetworkPolicy, err)
	}
	stage := networkpolicy.BlockedPolicy()
	if !prepare && (len(preparation) > 0 || len(prepareSecrets) > 0) {
		return runtime, stage, fmt.Errorf("%w prepareNetwork: preparation policy and Secrets require prepare", ErrInvalidNetworkPolicy)
	}
	if prepare {
		stage = runtime
		if len(preparation) > 0 {
			stage, err = networkpolicy.ParsePublic(preparation)
			if err != nil {
				return runtime, stage, fmt.Errorf("%w prepareNetwork: %w", ErrInvalidNetworkPolicy, err)
			}
		}
	}
	for _, phase := range []struct {
		name   string
		policy networkpolicy.Policy
		refs   []SecretBinding
	}{{"secrets", runtime, secrets}, {"prepareSecrets", stage, prepareSecrets}} {
		refs, err := secretbinding.CanonicalReferences(phase.refs)
		if err != nil {
			return runtime, stage, err
		}
		for i, ref := range refs {
			if ref.Env != nil && ref.Env.Mode == "protected" {
				for _, origin := range ref.Env.AllowedOrigins {
					if err := phase.policy.ValidateProtectedOrigin(origin); err != nil {
						return runtime, stage, fmt.Errorf("%w %s[%d]: %w", ErrInvalidNetworkPolicy, phase.name, i, err)
					}
				}
			}
		}
	}
	return runtime, stage, nil
}
