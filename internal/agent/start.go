package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/lifetime"
	"github.com/helmrdotdev/helmr/internal/secretbinding"

	"github.com/helmrdotdev/helmr/internal/content"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// StartRequest contains submitted identity, never resolved defaults. Runtime
// Agent references are definition keys in the caller's pinned Deployment.
type StartRequest struct {
	CloseAfterIdle lifetime.Policy
	SlackPreflight SlackStartPreflight
	PreparedSlack  *SlackStartRoute
	EnvironmentID  uuid.UUID
	Agent          string
	ComputerID     uuid.UUID
	SessionKey     *string
	SlackChannelID *string
	IdempotencyKey string
	Input          json.RawMessage
}

type ComputerTrustIssuer interface {
	GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error)
}

func Start(ctx context.Context, pool db.TxBeginner, trust ComputerTrustIssuer, caller Caller, req StartRequest) (Admission, error) {
	return admitSession(ctx, pool, trust, caller, req, "create")
}
func Spawn(ctx context.Context, pool db.TxBeginner, trust ComputerTrustIssuer, caller Caller, req StartRequest) (Admission, error) {
	return admitSession(ctx, pool, trust, caller, req, "spawn")
}

// Creation commits a durable Session receipt, optionally with its first Turn and
// Computer admission. Empty Sessions pin code without allocating execution.
func admitSession(ctx context.Context, pool db.TxBeginner, trust ComputerTrustIssuer, caller Caller, req StartRequest, method string) (Admission, error) {
	if !req.CloseAfterIdle.Valid() || req.EnvironmentID == uuid.Nil() || !definition.ValidDeclaredID(req.Agent) || len(req.IdempotencyKey) > 512 || !utf8.ValidString(req.IdempotencyKey) || strings.ContainsRune(req.IdempotencyKey, 0) || (req.SessionKey != nil && (len(*req.SessionKey) == 0 || len(*req.SessionKey) > 512 || !utf8.ValidString(*req.SessionKey) || strings.ContainsRune(*req.SessionKey, 0))) {
		return Admission{}, ErrInvalidInput
	}
	if req.SlackChannelID != nil && (!ValidSlackChannelID(*req.SlackChannelID) || caller.Kind == "session") {
		return Admission{}, ErrInvalidInput
	}
	if caller.ID == uuid.Nil() || (method == "spawn" && caller.Kind != "session") {
		return Admission{}, ErrDenied
	}
	if caller.Kind == "session" && (caller.Execution.EnvironmentID != req.EnvironmentID || caller.Execution.SessionID != caller.ID || req.SessionKey != nil) {
		return Admission{}, ErrDenied
	}
	if caller.Kind == "session" && req.IdempotencyKey == "" {
		return Admission{}, ErrInvalidInput
	}
	hasInput := req.Input != nil
	if hasInput || caller.Kind == "session" || method == "spawn" {
		var err error
		req.Input, err = content.Input(req.Input)
		if err != nil {
			return Admission{}, err
		}
		if bytes.Equal(req.Input, []byte("[]")) {
			return Admission{}, ErrInvalidInput
		}
	}
	raw, err := json.Marshal(struct {
		CloseAfterIdle lifetime.Policy `json:",omitzero"`
		Agent          string
		Computer       any
		SessionKey     *string
		Input          json.RawMessage
		SlackChannelID *string
	}{req.CloseAfterIdle, req.Agent, nullableID(req.ComputerID), req.SessionKey, req.Input, req.SlackChannelID})
	if err != nil {
		return Admission{}, ErrInvalidInput
	}
	digest, err := digestJSON(raw)
	if err != nil {
		return Admission{}, ErrInvalidInput
	}
	receipt, prepared, err := prepareStart(ctx, pool, caller, req, method, digest)
	if err != nil {
		return Admission{}, err
	}
	if receipt != nil {
		return *receipt, nil
	}
	var result Admission
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if caller.Kind == "session" {
			if err := lockRuntimeHost(ctx, tx, caller); err != nil {
				return err
			}
			if err := requireRootCaller(ctx, tx, caller); err != nil {
				return err
			}
		} else if err := authorizeStart(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		routeActive := true
		if prepared != nil {
			pubs := []uuid.UUID{prepared.Target.ConnectionID}
			if prepared.Source != nil {
				pubs = append(pubs, prepared.Source.Connection.ConnectionID)
			}
			active, err := LockSlackConnections(ctx, tx, req.EnvironmentID, pubs)
			if err != nil {
				return err
			}
			// Exact retries are checked below before fresh connection eligibility.
			routeActive = active
		}
		var binding slackAdmissionBinding
		if caller.Kind == "session" && method == "spawn" {
			sourceRoute, err := slackRouteOf(ctx, tx, req.EnvironmentID, caller.ID)
			if err != nil {
				return err
			}
			if sourceRoute != nil {
				if _, err := LockSlackChannels(ctx, tx, req.EnvironmentID, []uuid.UUID{*sourceRoute}); err != nil {
					return err
				}
			}
			binding, err = resolveSlackChildBinding(ctx, tx, req.EnvironmentID, caller.ID)
			if err != nil {
				return err
			}
		}
		policy, err := lockAdmissionEnvironment(ctx, tx, req.EnvironmentID)
		if err != nil {
			return err
		}
		var agentID uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE environment_id=$1 AND key=$2`, req.EnvironmentID, req.Agent).Scan(&agentID); err != nil {
			return err
		}
		prior, err := sessionCreationReceipt(ctx, tx, caller, req.EnvironmentID, agentID, method, req.IdempotencyKey, digest)
		if err != nil {
			return err
		}
		if prior != nil {
			result = *prior
			return nil
		}
		if req.SessionKey != nil {
			var occupied bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND agent_id=$2 AND key=$3)`, req.EnvironmentID, agentID, *req.SessionKey).Scan(&occupied); err != nil {
				return err
			}
			if occupied {
				return ErrConflict
			}
		}
		var deployment uuid.UUID
		if caller.Kind == "session" && method == "spawn" {
			if err = tx.QueryRow(ctx, `SELECT deployment_id FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, caller.ID).Scan(&deployment); err != nil {
				return err
			}
		} else {
			if policy.deployment == nil {
				return ErrNotReady
			}
			deployment = *policy.deployment
		}
		var revoked bool
		if err = tx.QueryRow(ctx, `SELECT execution_revoked_at IS NOT NULL FROM deployments WHERE environment_id=$1 AND id=$2 FOR SHARE`, req.EnvironmentID, deployment).Scan(&revoked); err != nil {
			return err
		}
		if revoked {
			return ErrDenied
		}
		var templateKey string
		var duration *int64
		var never bool
		if err = tx.QueryRow(ctx, `SELECT t.key,d.close_after_idle_ms,d.close_after_idle_never FROM agent_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) WHERE d.environment_id=$1 AND d.agent_id=$2 AND d.deployment_id=$3`, req.EnvironmentID, agentID, deployment).Scan(&templateKey, &duration, &never); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotReady
			}
			return err
		}
		var route *uuid.UUID
		if prepared != nil {
			if !routeActive {
				return ErrConversationChanged
			}
			if err = validateSlackStart(ctx, tx, req.EnvironmentID, agentID, deployment, caller, prepared); err != nil {
				return err
			}
			routeID, err := ResolveSlackRoute(ctx, tx, prepared.Target)
			if err != nil {
				return err
			}
			route = &routeID
		} else if req.SlackChannelID != nil {
			return ErrConversationChanged
		}
		computer := req.ComputerID
		if computer == uuid.Nil() && hasInput {
			computer, err = createAdmissionComputer(ctx, tx, trust, req.EnvironmentID, deployment, templateKey, policy)
			if err != nil {
				return err
			}
		}
		var callerIDs, computerIDs []uuid.UUID
		if caller.Kind == "session" {
			callerIDs = append(callerIDs, caller.ID)
		}
		if computer != uuid.Nil() {
			computerIDs = append(computerIDs, computer)
		}
		locked, err := lockSessionsAndComputers(ctx, tx, req.EnvironmentID, callerIDs, computerIDs)
		if err != nil {
			return err
		}
		if caller.Kind == "session" {
			if err = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, locked[caller.ID]); err != nil {
				return err
			}
		} else if err = authorizeStart(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		if computer != uuid.Nil() {
			if err = computerAcceptsAdmission(ctx, tx, req.EnvironmentID, computer, true); err != nil {
				return err
			}
			if req.ComputerID != uuid.Nil() {
				var profile string
				if err = tx.QueryRow(ctx, `SELECT spec.seed->>'profile' FROM computers c JOIN image_specs spec ON (spec.environment_id,spec.id)=(c.environment_id,c.image_spec_id) WHERE c.environment_id=$1 AND c.id=$2`, req.EnvironmentID, computer).Scan(&profile); err != nil {
					return err
				}
				if profile != definition.ComputerSeedProfile {
					return ErrInvalidInput
				}
			}
		}
		if hasInput {
			err = consumeAdmission(ctx, tx, req.EnvironmentID, policy)
		} else {
			err = consumeAdmissionToken(ctx, tx, req.EnvironmentID, policy)
		}
		if err != nil {
			return err
		}
		result.SessionID, result.Created = uuid.NewV7(), true
		result.CloseAfterIdleMs = lifetime.Resolve(req.CloseAfterIdle, duration, never)
		if hasInput {
			result.TurnID, result.Sequence = uuid.NewV7(), 1
		}
		root := result.SessionID
		var parent, requester, origin any
		causal := 0
		if caller.Kind == "session" {
			if err = tx.QueryRow(ctx, `SELECT causal_depth FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, caller.ID).Scan(&causal); err != nil {
				return err
			}
			causal++
			requester, origin = caller.ID, nullableID(caller.TurnID)
			if method == "spawn" {
				parent, root = caller.ID, locked[caller.ID].root
			}
			if causal > policy.maxCausal {
				return ErrInvalidInput
			}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,origin_turn_id,causal_depth,key,close_after_idle_ms,history_retention_mode,history_retention_seconds,creation_caller_kind,creation_caller_id,creation_method,creation_idempotency_key,creation_digest,creation_turn_id,idle_since,next_turn_seq)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,history_retention_mode,history_retention_seconds,$13,$14,$15,NULLIF($16,''),$17,$18,CASE WHEN $18::uuid IS NULL THEN clock_timestamp() END,CASE WHEN $18::uuid IS NULL THEN 1 ELSE 2 END FROM environments WHERE id=$1`, req.EnvironmentID, result.SessionID, agentID, deployment, nullableID(computer), root, parent, requester, origin, causal, req.SessionKey, result.CloseAfterIdleMs, caller.Kind, caller.ID, method, req.IdempotencyKey, digest[:], nullableID(result.TurnID)); err != nil {
			return err
		}
		if parent != nil {
			if _, err = tx.Exec(ctx, `UPDATE sessions SET idle_since=NULL WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, parent); err != nil {
				return err
			}
		}
		if binding.thread != uuid.Nil() {
			if err = insertSlackSource(ctx, tx, req.EnvironmentID, result.SessionID, binding.thread); err != nil {
				return err
			}
		}
		if hasInput {
			if _, err = tx.Exec(ctx, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,idempotency_key,request_digest,input,origin_turn_id) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,NULLIF($9,''),$10,$11,$12)`, req.EnvironmentID, result.TurnID, result.SessionID, computer, caller.Kind, caller.ID, method, agentID, req.IdempotencyKey, digest[:], req.Input, nullableID(caller.TurnID)); err != nil {
				return err
			}
		}
		if prepared != nil {
			if err = createSlackRoot(ctx, tx, req.EnvironmentID, result.SessionID, result.TurnID, *route, *prepared); err != nil {
				return err
			}
		}
		if hasInput {
			return event(ctx, tx, req.EnvironmentID, result.SessionID, result.TurnID, "turn.queued")
		}
		return nil
	})
	if err != nil {
		var invalid *pgconn.PgError
		if errors.As(err, &invalid) && strings.HasPrefix(invalid.Code, "22") {
			return Admission{}, fmt.Errorf("%w: input cannot be stored", ErrInvalidInput)
		}
		return Admission{}, hideMissing(err)
	}
	return result, nil
}

// A creation receipt never depends on a Turn existing or the Agent remaining in
// current. Caller authority is still checked on each observation.
func sessionCreationReceipt(ctx context.Context, tx pgx.Tx, caller Caller, env, agentID uuid.UUID, method, retry string, digest [32]byte) (*Admission, error) {
	if retry == "" {
		return nil, nil
	}
	var result Admission
	var turn *uuid.UUID
	var prior []byte
	err := tx.QueryRow(ctx, `SELECT id,creation_turn_id,creation_digest,close_after_idle_ms FROM sessions WHERE environment_id=$1 AND creation_caller_kind=$2 AND creation_caller_id=$3 AND creation_method=$4 AND agent_id=$5 AND creation_idempotency_key=$6`, env, caller.Kind, caller.ID, method, agentID, retry).Scan(&result.SessionID, &turn, &prior, &result.CloseAfterIdleMs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if caller.Kind == "session" {
		owner, err := lockSession(ctx, tx, env, caller.ID)
		if err != nil {
			return nil, err
		}
		if err = executionReceipt(ctx, tx, caller.Execution, owner); err != nil {
			return nil, err
		}
	} else if err = authorizeStart(ctx, tx, caller, env); err != nil {
		return nil, err
	}
	if !bytes.Equal(prior, digest[:]) {
		return nil, ErrConflict
	}
	result.Created = true
	if turn != nil {
		result.TurnID, result.Sequence = *turn, 1
	}
	return &result, nil
}

func authorizeStart(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID) error {
	var allowed bool
	var err error
	switch caller.Kind {
	case "user":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environments e JOIN org_members m ON m.org_id=e.org_id JOIN users u ON u.id=m.user_id WHERE e.id=$1 AND m.user_id=$2 AND m.disabled_at IS NULL AND u.disabled_at IS NULL AND m.role IN ('owner','admin','developer'))`, env, caller.ID).Scan(&allowed)
	case "api_key":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys k JOIN environments e ON (e.id,e.project_id,e.org_id)=(k.environment_id,k.project_id,k.org_id) WHERE e.id=$1 AND k.id=$2 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()) AND k.role IN ('owner','admin','developer') AND 'sessions.create'=ANY(k.permissions))`, env, caller.ID).Scan(&allowed)
	default:
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if !allowed {
		return ErrDenied
	}
	return nil
}

func computerAcceptsAdmission(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, newPlacement bool) error {
	var available bool
	if err := tx.QueryRow(ctx, `SELECT preparation_failed_at IS NULL AND deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2) AND (NOT $3 OR (integrity_fault_at IS NULL AND (initial_root_id IS NOT NULL OR preparation_deadline_at>clock_timestamp()))) FROM computers WHERE environment_id=$1 AND id=$2`, env, computer, newPlacement).Scan(&available); err != nil {
		return err
	}
	if !available {
		return ErrNotReady
	}
	_, err := tx.Exec(ctx, `UPDATE computers SET unused_since=NULL WHERE environment_id=$1 AND id=$2`, env, computer)
	return err
}

func createAdmissionComputer(ctx context.Context, tx pgx.Tx, trust ComputerTrustIssuer, env, deployment uuid.UUID, key string, policy admissionEnvironment) (uuid.UUID, error) {
	return createComputer(ctx, tx, trust, env, deployment, key, policy, nil)
}

// CreateStandaloneComputer joins the external owner's transaction. Explicit
// runtime bindings are actual-Computer declarations; Agent-created Computers
// instead copy their immutable definition bindings. Both share preparation,
// trust, resource validation and reservation admission.
func CreateStandaloneComputer(ctx context.Context, tx pgx.Tx, trust ComputerTrustIssuer, env, deployment uuid.UUID, key string, bindings []secretbinding.Reference) (uuid.UUID, error) {
	policy, err := lockAdmissionEnvironment(ctx, tx, env)
	if err != nil {
		return uuid.Nil(), err
	}
	if bindings == nil {
		bindings = []secretbinding.Reference{}
	}
	return createComputer(ctx, tx, trust, env, deployment, key, policy, &bindings)
}

func createComputer(ctx context.Context, tx pgx.Tx, trust ComputerTrustIssuer, env, deployment uuid.UUID, key string, policy admissionEnvironment, explicit *[]secretbinding.Reference) (uuid.UUID, error) {
	var spec, template uuid.UUID
	var cpuMillis, memoryBytes int64
	var diskBytes *int64
	var seed []byte
	var maxAge, deleteUnused *int64
	var deleteNever bool
	if err := tx.QueryRow(ctx, `SELECT d.template_id,d.image_spec_id,d.cpu_millis,d.memory_bytes,d.disk_bytes,d.max_image_age_ms,s.seed,d.delete_after_unused_ms,d.delete_after_unused_never FROM template_definitions d JOIN templates t ON (t.environment_id,t.id)=(d.environment_id,d.template_id) JOIN image_specs s ON (s.environment_id,s.id)=(d.environment_id,d.image_spec_id) WHERE d.environment_id=$1 AND d.deployment_id=$2 AND t.key=$3`, env, deployment, key).Scan(&template, &spec, &cpuMillis, &memoryBytes, &diskBytes, &maxAge, &seed, &deleteUnused, &deleteNever); err != nil {
		return uuid.Nil(), err
	}
	var base definition.ComputerSeedManifest
	if json.Unmarshal(seed, &base) != nil || base.Profile != definition.ComputerSeedProfile {
		return uuid.Nil(), ErrInvalidInput
	}
	physicalCPU, err := vm.ReservedCPUMillis(cpuMillis)
	if err != nil || (diskBytes != nil && *diskBytes != disk.SeedCapacity) || physicalCPU > policy.maxCPU || memoryBytes > policy.maxMemory || disk.SeedCapacity > policy.maxStorage {
		return uuid.Nil(), ErrInvalidInput
	}
	var reserved int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(storage_reservation_bytes),0) FROM computers WHERE environment_id=$1`, env).Scan(&reserved); err != nil {
		return uuid.Nil(), err
	}
	if reserved > policy.maxStorage-disk.SeedCapacity {
		return uuid.Nil(), ErrNotReady
	}
	id := uuid.NewV7()
	if _, err := tx.Exec(ctx, `INSERT INTO computers(environment_id,id,image_spec_id,origin_deployment_id,origin_template_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes,preparation_deadline_at,preparation_max_age_ms,delete_after_unused_ms) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp()+$10*interval '1 millisecond',$11,$12)`, env, id, spec, deployment, template, cpuMillis, memoryBytes, diskBytes, disk.SeedCapacity, policy.preparationTimeout, maxAge, lifetime.Resolve(lifetime.Policy{}, deleteUnused, deleteNever)); err != nil {
		return uuid.Nil(), err
	}
	if explicit == nil {
		if err := copyDefinitionSecretBindings(ctx, tx, env, deployment, template, id); err != nil {
			return uuid.Nil(), err
		}
	} else {
		resolved, err := resolveSecretReferences(ctx, tx, env, *explicit)
		if err != nil {
			return uuid.Nil(), err
		}
		bindings, err := registrationSecretBindings(*explicit, resolved, nil, nil, nil)
		if err != nil {
			return uuid.Nil(), err
		}
		for i := range bindings {
			bindings[i].Computer = &id
		}
		if err = registerSecretBindings(ctx, tx, env, bindings); err != nil {
			return uuid.Nil(), err
		}
	}

	var protected bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_bindings WHERE environment_id=$1 AND computer_id=$2 AND mode='protected')`, env, id).Scan(&protected); err != nil {
		return uuid.Nil(), err
	}
	if protected {
		if trust == nil {
			return uuid.Nil(), ErrNotReady
		}
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&createdAt); err != nil {
			return uuid.Nil(), err
		}
		material, err := trust.GenerateProxyTrust(env, id, createdAt)
		if err != nil {
			return uuid.Nil(), err
		}
		if _, err = tx.Exec(ctx, `UPDATE computers SET proxy_ca_certificate=$3,proxy_ca_not_after=$4,proxy_ca_private_key_nonce=$5,proxy_ca_private_key_ciphertext=$6 WHERE environment_id=$1 AND id=$2`, env, id, material.Certificate, material.NotAfter, material.PrivateKeyNonce, material.PrivateKeyCiphertext); err != nil {
			return uuid.Nil(), err
		}
	}

	if _, err := pinComputerImage(ctx, tx, env, id); err == nil {
		return id, nil
	} else if !errors.Is(err, ErrNotReady) {
		return uuid.Nil(), err
	}
	if _, err := attachComputerPreparation(ctx, tx, env, id); err != nil && !errors.Is(err, ErrNotReady) {
		return uuid.Nil(), err
	}
	return id, nil
}
