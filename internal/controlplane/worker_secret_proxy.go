package controlplane

import (
	"errors"
	"net/http"
	"net/url"
	"slices"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/origin"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (s *Server) workerPrepareSecretProxy(w http.ResponseWriter, r *http.Request) {
	s.workerSecretProxy(w, r, false)
}
func (s *Server) workerResolveSecretProxy(w http.ResponseWriter, r *http.Request) {
	s.workerSecretProxy(w, r, true)
}

// Preparation proves the runtime reservation. Only resolution proves live mounted
// execution authority; a resumed guest can contact transport before that exists.
func (s *Server) workerSecretProxy(w http.ResponseWriter, r *http.Request, resolve bool) {
	var request workerapi.SecretProxyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(errors.New("invalid Secret transport request")))
		return
	}
	runtimeID, err := ids.Parse(request.RuntimeInstanceID)
	if err != nil || s.secretProxy == nil || s.db == nil {
		writeError(w, conflict(secret.ErrDeliveryUnavailable))
		return
	}
	if resolve {
		canonical, e := origin.Canonical(request.Origin)
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
	// These ordinary primary SELECTs each run as a fresh command snapshot. Do
	// not wrap resolution in an inherited transaction or add material lookups.
	if resolve {
		if err = secret.ValidateProtectedSelectors(request.Placeholders); err == nil {
			var captured []db.CaptureProtectedSecretEnvelopesRow
			captured, err = s.db.CaptureProtectedSecretEnvelopes(r.Context(), db.CaptureProtectedSecretEnvelopesParams{
				RuntimeInstanceID: pgvalue.UUID(runtimeID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
				WorkerEpoch: worker.WorkerEpoch, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
				ClaimVersion: worker.ClaimVersion, GroupClaimVersion: worker.GroupClaimVersion,
				Origin: request.Origin, Placeholders: request.Placeholders,
			})
			if err == nil {
				resolution.Values, err = s.secretProxy.OpenProtected(captured, request.Placeholders)
			}
		}
	} else {
		var captured db.CaptureSecretProxyPreparationRow
		captured, err = s.db.CaptureSecretProxyPreparation(r.Context(), db.CaptureSecretProxyPreparationParams{
			RuntimeInstanceID: pgvalue.UUID(runtimeID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
			ClaimVersion: worker.ClaimVersion, GroupClaimVersion: worker.GroupClaimVersion,
		})
		if err == nil && len(captured.Origins) != 0 {
			hosts := []string{}
			for _, o := range captured.Origins {
				var u *url.URL
				u, err = url.Parse(o)
				if err != nil {
					break
				}
				if !slices.Contains(hosts, u.Hostname()) {
					hosts = append(hosts, u.Hostname())
				}
			}
			if err == nil {
				preparation.Origins = captured.Origins
				preparation.Certificate, preparation.PrivateKey, err = s.secretProxy.ProxyLeaf(secret.ProxyTrust{
					EnvironmentID: pgvalue.MustUUIDValue(captured.EnvironmentID), WorkspaceID: pgvalue.MustUUIDValue(captured.WorkspaceID),
					Certificate: captured.Certificate, NotAfter: captured.NotAfter.Time,
					PrivateKeyNonce: captured.PrivateKeyNonce, PrivateKeyCiphertext: captured.PrivateKeyCiphertext,
				}, hosts)
			}
		}
	}
	if err != nil {
		if errors.Is(err, secret.ErrProxyTrustExpired) {
			writeError(w, conflict(secret.ErrProxyTrustExpired))
		} else {
			writeError(w, conflict(secret.ErrDeliveryUnavailable))
		}
		return
	}
	if resolve {
		writeJSON(w, http.StatusOK, resolution)
	} else {
		writeJSON(w, http.StatusOK, preparation)
	}
}
