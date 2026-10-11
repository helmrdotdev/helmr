package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (server *Server) workerAgentStart(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentControlRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	execution, err := runtimeExecution(request.Session, host)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	start, err := agent.AuthorizeSessionStart(r.Context(), server.tx, host, execution, request.AttachmentSequence)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	program, err := server.sessionProgram(r.Context(), start)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	exposure, err := agent.RecordSessionStartExposure(r.Context(), server.tx, host, execution, request.AttachmentSequence)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	envelopes := make([]secret.DeliveryEnvelope, 0, len(exposure.Secrets))
	for _, e := range exposure.Secrets {
		envelopes = append(envelopes, secret.DeliveryEnvelope{Mode: e.Mode, PlacementKind: e.Kind, PlacementTarget: e.Target,
			Secret:  db.Secret{ID: pgvalue.UUID(e.SecretID), EnvironmentID: pgvalue.UUID(execution.EnvironmentID), Status: "active"},
			Version: db.SecretVersion{ID: pgvalue.UUID(e.VersionID), SecretID: pgvalue.UUID(e.SecretID), Version: e.Version, Nonce: e.Nonce, Ciphertext: e.Ciphertext}})
	}
	materials, err := server.secretDelivery.OpenDeliveries(execution.EnvironmentID, envelopes)
	defer func() {
		for _, material := range materials {
			clear(material.Value)
		}
	}()
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	deliveries, err := projectSecretDeliveries(materials)
	defer clearComputerSecretDeliveries(deliveries)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	var protected *workerapi.ProtectedEnv
	if len(exposure.ProtectedEnv) > 0 {
		protected = &workerapi.ProtectedEnv{Env: exposure.ProtectedEnv, CA: exposure.ProxyCA}
	}
	// Object reads can outlive authority. Release only after a current business
	// admission check, including the exact transport that will receive setup.
	start, err = agent.AuthorizeSessionStart(r.Context(), server.tx, host, execution, request.AttachmentSequence)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workerapi.AgentStartResponse{Secrets: deliveries, ProtectedEnv: protected, BundleDigest: start.BundleDigest, Authority: workerapi.AgentAuthorityResponse{AuthorityGeneration: start.Authority.Generation, ExpiresAt: start.Authority.ExpiresAt}, Program: program, AgentKey: start.AgentKey, ComputerID: start.ComputerID.String(), SessionKey: start.SessionKey, ParentSessionID: start.ParentSessionID, TerminalSequence: start.TerminalSequence})
}

// The pinned immutable bundle root supplies executable descriptor authority.
func (server *Server) sessionProgram(ctx context.Context, start agent.ProcessStart) (workerapi.RuntimeProgram, error) {
	manifest, err := server.readExecutableBundle(ctx, start.BundleDigest)
	if err != nil {
		return workerapi.RuntimeProgram{}, err
	}
	return runtimeProgram(start.DeploymentID.String(), manifest)
}

func (server *Server) readExecutableBundle(ctx context.Context, digestPin string) (bundle.Manifest, error) {
	if server.cas == nil {
		return bundle.Manifest{}, errors.New("executable Program store is unavailable")
	}
	object, err := server.cas.Stat(ctx, digestPin)
	if err != nil {
		return bundle.Manifest{}, err
	}
	if object.Digest != digestPin || object.MediaType != bundle.MediaType || object.SizeBytes < 1 || object.SizeBytes > bundle.MaxBytes {
		return bundle.Manifest{}, errors.New("invalid executable bundle descriptor")
	}
	reader, err := server.cas.Get(ctx, digestPin)
	if err != nil {
		return bundle.Manifest{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(reader, bundle.MaxBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return bundle.Manifest{}, errors.Join(readErr, closeErr)
	}
	digest, err := bundle.Digest(raw)
	if err != nil || digest != digestPin || int64(len(raw)) != object.SizeBytes {
		return bundle.Manifest{}, errors.New("executable bundle does not match its pin")
	}
	manifest, err := bundle.Parse(raw)
	if err != nil {
		return bundle.Manifest{}, err
	}
	return manifest, nil
}

func runtimeProgram(deploymentID string, manifest bundle.Manifest) (workerapi.RuntimeProgram, error) {
	metadata, err := artifact.CanonicalProgramMetadata(manifest.Program.Metadata)
	if err != nil {
		return workerapi.RuntimeProgram{}, err
	}
	return workerapi.RuntimeProgram{DeploymentID: deploymentID, Runtime: workerapi.CASObject{Digest: manifest.Runtime.Artifact.Digest, SizeBytes: manifest.Runtime.Artifact.SizeBytes, MediaType: manifest.Runtime.Artifact.MediaType}, Artifact: workerapi.CASObject{Digest: manifest.Program.Artifact.Digest, SizeBytes: manifest.Program.Artifact.SizeBytes, MediaType: manifest.Program.Artifact.MediaType}, IndexDigest: sha256sum.DigestBytes(metadata)}, nil
}

func (server *Server) workerAgentStartRelease(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AgentStartReleaseRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	host := workerFromContext(r.Context())
	execution, err := runtimeExecution(request.Session, host)
	if err != nil || !sha256sum.ValidDigest(request.BundleDigest) {
		writeError(w, badRequest(errors.New("invalid Session start release")))
		return
	}
	start, err := agent.AuthorizeSessionStart(r.Context(), server.tx, host, execution, request.AttachmentSequence)
	if err != nil {
		server.writeAgentWorkerError(w, err)
		return
	}
	if start.BundleDigest != request.BundleDigest {
		server.writeAgentWorkerError(w, agent.ErrNotReady)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.AgentAuthorityResponse{AuthorityGeneration: start.Authority.Generation, ExpiresAt: start.Authority.ExpiresAt})
}
