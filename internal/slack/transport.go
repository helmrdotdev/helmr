package slack

// Transport is fixed by the deployment, not by organization administrators.
// Disabled installations do not construct a CredentialStore or receive work.
type Transport string

const (
	TransportHTTP     Transport = "http"
	TransportSocket   Transport = "socket"
	TransportDisabled Transport = "disabled"
)

func (t Transport) enabled() bool { return t == TransportHTTP || t == TransportSocket }

func (s *CredentialStore) DeliveryTransport() Transport { return s.mode }
