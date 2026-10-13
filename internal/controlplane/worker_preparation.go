package controlplane

import (
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func preparationExecutor(in workerapi.PreparationExecutor) (computer.ImageBuildExecutor, error) {
	identity, err := allocationIdentity(in.Identity)
	if err != nil || identity.Kind != "preparation" || len(in.ChannelCredential) != 32 {
		return computer.ImageBuildExecutor{}, session.ErrInvalidInput
	}
	return computer.ImageBuildExecutor{EnvironmentID: identity.EnvironmentID, ImageBuildID: identity.OwnerID, InstanceID: identity.InstanceID, Epoch: identity.Epoch, ChannelCredential: in.ChannelCredential}, nil
}
func (server *Server) workerRenewPreparation(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationExecutor
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.ChannelCredential)
	ref, err := preparationExecutor(request)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	result, err := computer.RenewImageBuild(r.Context(), server.tx, workerFromContext(r.Context()), ref)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.PreparationRenewal{ExpiresAt: result})
}
func (server *Server) workerPreparationWriteKey(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationExecutor
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.ChannelCredential)
	ref, err := preparationExecutor(request)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	result, err := server.preparationKeys.WriteKey(r.Context(), workerFromContext(r.Context()), ref)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(result.Key)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workerapi.ComputerKeyMaterial{Scope: result.Scope, ID: result.ID.String(), Key: result.Key})
}
func (server *Server) workerFailPreparation(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationFailure
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.Executor.ChannelCredential)
	ref, err := preparationExecutor(request.Executor)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	err = computer.FailImageBuild(r.Context(), server.tx, workerFromContext(r.Context()), ref, request.Code)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerBeginPreparationCapture(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationCaptureBegin
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.Executor.ChannelCredential)
	ref, err := preparationExecutor(request.Executor)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	err = server.preparationPublisher.BeginCapture(r.Context(), workerFromContext(r.Context()), ref, request.LogicalBytes)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerRegisterPreparationObject(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationObject
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.Executor.ChannelCredential)
	ref, err := preparationExecutor(request.Executor)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	err = server.preparationPublisher.Register(r.Context(), workerFromContext(r.Context()), ref, request.Inspection)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerCertifyPreparationObject(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationObject
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.Executor.ChannelCredential)
	ref, err := preparationExecutor(request.Executor)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	err = server.preparationPublisher.Certify(r.Context(), workerFromContext(r.Context()), ref, request.Inspection)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerRecordPreparationCapture(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationPublication
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.Executor.ChannelCredential)
	ref, err := preparationExecutor(request.Executor)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	err = server.preparationPublisher.Capture(r.Context(), workerFromContext(r.Context()), ref, request.Root, request.Evidence)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerPublishPreparation(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationPublication
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.Executor.ChannelCredential)
	ref, err := preparationExecutor(request.Executor)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	err = server.preparationPublisher.Publish(r.Context(), workerFromContext(r.Context()), ref, request.Root, request.Evidence)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Exposure must commit before decryption, and authority must still hold after
// the opener returns. Reconnection uses the attempt's recorded Secret versions.
func (server *Server) workerPreparationSecrets(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationExecutor
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.ChannelCredential)
	ref, err := preparationExecutor(request)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	host := workerFromContext(r.Context())
	exposed, err := computer.RecordImageBuildExposure(r.Context(), server.tx, host, ref)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	envelopes := make([]secret.DeliveryEnvelope, 0, len(exposed))
	hasProtected := false
	for _, e := range exposed {
		if e.Mode == "protected" {
			hasProtected = true
		}
		envelopes = append(envelopes, secret.DeliveryEnvelope{Mode: e.Mode, PlacementKind: e.Kind, PlacementTarget: e.Target,
			Secret:  db.Secret{ID: pgvalue.UUID(e.SecretID), EnvironmentID: pgvalue.UUID(ref.EnvironmentID), Status: "active"},
			Version: db.SecretVersion{ID: pgvalue.UUID(e.VersionID), SecretID: pgvalue.UUID(e.SecretID), Version: e.Version, Nonce: e.Nonce, Ciphertext: e.Ciphertext}})
	}
	var protected *workerapi.ProtectedEnv
	if hasProtected {
		captured, err := computer.CaptureImageBuildProxyTrust(r.Context(), server.tx, host, ref.InstanceID)
		if err != nil {
			server.writeAllocationError(w, err)
			return
		}
		if captured.Trust.PreparationID != ref.ImageBuildID || captured.Trust.EnvironmentID != ref.EnvironmentID {
			server.writeAllocationError(w, session.ErrDenied)
			return
		}
		protected = &workerapi.ProtectedEnv{Env: captured.Env, CA: captured.Trust.Certificate}
	}
	materials, err := server.secretDelivery.OpenDeliveries(ref.EnvironmentID, envelopes)
	defer func() {
		for _, material := range materials {
			clear(material.Value)
		}
	}()
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	if _, err = computer.RecordImageBuildExposure(r.Context(), server.tx, host, ref); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	deliveries, err := projectSecretDeliveries(materials)
	defer func() {
		for _, delivery := range deliveries {
			clear(delivery.Value)
		}
	}()
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, workerapi.PreparationSecrets{Secrets: deliveries, Protected: protected})
}

func (server *Server) workerPreparationStart(w http.ResponseWriter, r *http.Request) {
	var request workerapi.PreparationExecutor
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer clear(request.ChannelCredential)
	ref, err := preparationExecutor(request)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	host := workerFromContext(r.Context())
	start, err := computer.StartImageBuild(r.Context(), server.tx, host, ref)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	manifest, err := server.readExecutableBundle(r.Context(), start.BundleDigest)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	if manifest.Program.Artifact != start.Spec.Program {
		server.writeAllocationError(w, session.ErrConflict)
		return
	}
	var computerID *definition.TemplateManifest
	for _, d := range manifest.Program.Metadata.Definitions {
		if d.Kind == definition.KindTemplate && d.DeclaredID == start.Spec.TemplateDefinitionID {
			computerID = d.Template
			break
		}
	}
	if computerID == nil {
		server.writeAllocationError(w, session.ErrConflict)
		return
	}
	var seedObject workerapi.CASObject
	for _, seed := range manifest.ComputerSeeds {
		if seed.DeclaredID == start.Spec.TemplateDefinitionID {
			seedObject = workerapi.CASObject{Digest: seed.Artifact.Digest, SizeBytes: seed.Artifact.SizeBytes, MediaType: seed.Artifact.MediaType}
			break
		}
	}
	if seedObject.Digest != computerID.Seed.ArtifactDigest {
		server.writeAllocationError(w, session.ErrConflict)
		return
	}
	program, err := runtimeProgram(start.DeploymentID.String(), manifest)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	_, err = computer.StartImageBuild(r.Context(), server.tx, host, ref)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.PreparationStart{Program: program, TemplateDefinitionID: start.Spec.TemplateDefinitionID, Seed: computerID.Seed, SeedObject: seedObject, RootfsDigest: start.RootfsDigest})
}
