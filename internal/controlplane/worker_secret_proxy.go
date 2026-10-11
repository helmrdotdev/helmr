package controlplane

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func (s *Server) workerPrepareSecretProxy(w http.ResponseWriter, r *http.Request) {
	s.workerSecretProxy(w, r, false)
}
func (s *Server) workerResolveSecretProxy(w http.ResponseWriter, r *http.Request) {
	s.workerSecretProxy(w, r, true)
}

// Preparation proves the instance reservation. Only resolution proves live mounted
// execution authority; a resumed guest can contact transport before that exists.
func (s *Server) workerSecretProxy(w http.ResponseWriter, r *http.Request, resolve bool) {
	var request workerapi.SecretProxyRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid Secret transport request: %w", err))
		return
	}
	instanceID, err := ids.Parse(request.ComputerInstanceID)
	if err != nil || s.secretProxy == nil {
		writeError(w, conflict(secret.ErrDeliveryUnavailable))
		return
	}
	if resolve {
		canonical, e := secretbinding.CanonicalOrigin(request.Origin)
		if e != nil || canonical != request.Origin || len(request.Placeholders) == 0 || len(request.Placeholders) > 64 {
			writeError(w, badRequest(errors.New("invalid Secret transport selection")))
			return
		}
	} else if request.Origin != "" || len(request.Placeholders) != 0 {
		writeError(w, badRequest(errors.New("invalid Secret transport preparation")))
		return
	}
	worker := workerFromContext(r.Context())
	var preparation workerapi.SecretProxyPreparation
	var resolution workerapi.SecretProxyResolution
	defer func() {
		clear(preparation.PrivateKey)
		for _, value := range resolution.Values {
			clear(value)
		}
	}()
	isPreparation, err := agent.IsPreparationProxyInstance(r.Context(), s.tx, instanceID)
	if err != nil {
		s.writeAllocationError(w, err)
		return
	}
	// These ordinary primary SELECTs each run as a fresh command snapshot. Do
	// not wrap resolution in an inherited transaction or add material lookups.
	if isPreparation {
		if resolve {
			var captured []secret.ProtectedCapture
			captured, err = agent.CapturePreparationProtectedSecrets(r.Context(), s.tx, worker, instanceID, request.Origin, request.Placeholders)
			if err == nil {
				resolution.Values, err = s.secretProxy.OpenProtectedCapture(captured, request.Placeholders)
			}
		} else {
			var captured agent.PreparationProxyTrust
			captured, err = agent.CapturePreparationProxyTrust(r.Context(), s.tx, worker, instanceID)
			if err == nil && len(captured.Origins) > 0 {
				hosts := []string{}
				for _, origin := range captured.Origins {
					var parsed *url.URL
					parsed, err = url.Parse(origin)
					if err != nil {
						break
					}
					if !slices.Contains(hosts, parsed.Hostname()) {
						hosts = append(hosts, parsed.Hostname())
					}
				}
				if err == nil {
					preparation.Origins = captured.Origins
					preparation.Certificate, preparation.PrivateKey, err = s.secretProxy.PreparationProxyLeaf(captured.Trust, hosts)
				}
			}
		}
	} else if resolve {
		var captured []secret.ProtectedCapture
		captured, err = agent.CaptureComputerProtectedSecrets(r.Context(), s.tx, worker, instanceID, request.Origin, request.Placeholders)
		if err == nil {
			resolution.Values, err = s.secretProxy.OpenProtectedCapture(captured, request.Placeholders)
		}
	} else {
		var captured agent.ComputerProxyTrust
		captured, err = agent.CaptureComputerProxyTrust(r.Context(), s.tx, worker, instanceID)
		if err == nil && len(captured.Origins) > 0 {
			hosts := []string{}
			for _, origin := range captured.Origins {
				var parsed *url.URL
				parsed, err = url.Parse(origin)
				if err != nil {
					break
				}
				if !slices.Contains(hosts, parsed.Hostname()) {
					hosts = append(hosts, parsed.Hostname())
				}
			}
			if err == nil {
				preparation.Origins = captured.Origins
				preparation.Certificate, preparation.PrivateKey, err = s.secretProxy.ComputerProxyLeaf(captured.Trust, hosts)
			}
		}
	}

	// The store refuses stale captures too; both answer re-authentication.
	if errors.Is(err, secret.ErrWorkerClaimsStale) {
		err = workergroup.ErrStaleClaims
	}
	if writeStaleWorkerClaims(w, err) {
		return
	}
	if err != nil {
		if errors.Is(err, secret.ErrProxyTrustExpired) {
			writeError(w, conflict(secret.ErrProxyTrustExpired))
		} else {
			writeError(w, conflict(secret.ErrDeliveryUnavailable))
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if resolve {
		writeJSON(w, http.StatusOK, resolution)
	} else {
		writeJSON(w, http.StatusOK, preparation)
	}
}
