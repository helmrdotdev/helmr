package workerapi

type SecretProxyRequest struct {
	RuntimeInstanceID string   `json:"runtime_instance_id"`
	Origin            string   `json:"origin,omitempty"`
	Placeholders      []string `json:"placeholders,omitempty"`
}

// SecretProxyPreparation is host-only. Its private leaf key must never enter guest frames.
type SecretProxyPreparation struct {
	Origins     []string `json:"origins"`
	Certificate []byte   `json:"certificate,omitempty"`
	PrivateKey  []byte   `json:"private_key,omitempty"`
}

type SecretProxyResolution struct {
	Values map[string][]byte `json:"values"`
}

// ProtectedEnv contains only inert selectors and public trust, safe for guest memory.
type ProtectedEnv struct {
	Env map[string]string `json:"env"`
	CA  []byte            `json:"ca"`
}

func (p *ProtectedEnv) Values() map[string]string {
	if p == nil {
		return nil
	}
	return p.Env
}
func (p *ProtectedEnv) PublicCA() []byte {
	if p == nil {
		return nil
	}
	return p.CA
}
