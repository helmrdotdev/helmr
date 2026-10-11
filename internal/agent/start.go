package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"math"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/conversation"
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
	SlackPreflight SlackStartPreflight
	PreparedSlack  *SlackStartRoute
	EnvironmentID  uuid.UUID
	Agent          string
	ComputerID     uuid.UUID
	SessionKey     *string
	SlackChannelID *string
	RetryKey       string
	Input          json.RawMessage
}

type ComputerTrustIssuer interface {
	GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error)
}

func Start(ctx context.Context, pool db.TxBeginner, trust ComputerTrustIssuer, caller Caller, req StartRequest) (Admission, error) {
	return admitSession(ctx, pool, trust, caller, req, "start")
}
func Spawn(ctx context.Context, pool db.TxBeginner, trust ComputerTrustIssuer, caller Caller, req StartRequest) (Admission, error) {
	return admitSession(ctx, pool, trust, caller, req, "spawn")
}

// Admission owns the first Turn, Session, fresh Computer, preparation demand and
// capacity together. No executor can observe any of them before commit.
func admitSession(ctx context.Context, pool db.TxBeginner, trust ComputerTrustIssuer, caller Caller, req StartRequest, method string) (Admission, error) {
	if req.EnvironmentID == uuid.Nil() || !definition.ValidDeclaredID(req.Agent) || len(req.RetryKey) > 512 || !utf8.ValidString(req.RetryKey) || (req.SessionKey != nil && (len(*req.SessionKey) == 0 || len(*req.SessionKey) > 512 || !utf8.ValidString(*req.SessionKey))) {
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
	if caller.Kind == "session" && req.RetryKey == "" {
		return Admission{}, ErrInvalidInput
	}
	var inputErr error
	req.Input, inputErr = conversation.Input(req.Input)
	if inputErr != nil {
		return Admission{}, inputErr
	}
	raw, err := json.Marshal(struct {
		Agent          string
		Computer       any
		SessionKey     *string
		Input          json.RawMessage
		SlackChannelID *string
	}{req.Agent, nullableID(req.ComputerID), req.SessionKey, req.Input, req.SlackChannelID})
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
		var route *uuid.UUID
		routeActive := true
		var slackBinding slackAdmissionBinding
		if prepared != nil {
			pubs := []uuid.UUID{prepared.Target.PublicationID}
			if prepared.Source != nil {
				pubs = append(pubs, prepared.Source.Connection.PublicationID)
			}
			var routeErr error
			routeActive, routeErr = LockSlackPublications(ctx, tx, req.EnvironmentID, pubs)
			if routeErr != nil {
				return routeErr
			}
		}
		if caller.Kind == "session" && method == "spawn" {
			var sourceRoute *uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT slack_channel_id FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, caller.ID).Scan(&sourceRoute); err != nil {
				return err
			}
			if sourceRoute != nil {
				if _, err := LockSlackChannels(ctx, tx, req.EnvironmentID, []uuid.UUID{*sourceRoute}); err != nil {
					return err
				}
			}
			var bindErr error
			slackBinding, bindErr = resolveSlackChildBinding(ctx, tx, req.EnvironmentID, caller.ID)
			if bindErr != nil {
				return bindErr
			}
		}
		policy, err := lockAdmissionEnvironment(ctx, tx, req.EnvironmentID)
		if err != nil {
			return err
		}
		var agentID, deployment uuid.UUID
		if caller.Kind == "session" && method == "spawn" {
			if err = tx.QueryRow(ctx, `SELECT d.agent_id,s.deployment_id FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.deployment_id)=(s.environment_id,s.deployment_id) WHERE s.environment_id=$1 AND s.id=$2 AND d.definition_key=$3`, req.EnvironmentID, caller.ID, req.Agent).Scan(&agentID, &deployment); err != nil {
				return err
			}
		} else {
			if err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE environment_id=$1 AND name=$2`, req.EnvironmentID, req.Agent).Scan(&agentID); err != nil {
				return err
			}
		}
		// Retry lookup precedes live defaults, placement and budget consumption.
		if req.RetryKey != "" {
			var prior []byte
			err = tx.QueryRow(ctx, `SELECT session_id,id,seq,seq=1,request_digest FROM turns WHERE environment_id=$1 AND caller_kind=$2 AND caller_id=$3 AND admission_method=$4 AND target_id=$5 AND retry_key=$6`, req.EnvironmentID, caller.Kind, caller.ID, method, agentID, req.RetryKey).Scan(&result.SessionID, &result.TurnID, &result.Sequence, &result.Created, &prior)
			if err == nil {
				if caller.Kind == "session" {
					owner, lockErr := lockSession(ctx, tx, req.EnvironmentID, caller.ID)
					if lockErr != nil {
						return lockErr
					}
					if err = executionReceipt(ctx, tx, caller.Execution, owner); err != nil {
						return err
					}
				} else {
					if err = authorizeStart(ctx, tx, caller, req.EnvironmentID); err != nil {
						return err
					}
					if !result.Created {
						if err = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, owners{}); err != nil {
							return err
						}
					}
				}
				if !bytes.Equal(prior, digest[:]) {
					return ErrConflict
				}
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var computer uuid.UUID
		existing := false
		if req.SessionKey != nil {
			err = tx.QueryRow(ctx, `SELECT id,deployment_id,computer_id FROM sessions WHERE environment_id=$1 AND agent_id=$2 AND session_key=$3`, req.EnvironmentID, agentID, *req.SessionKey).Scan(&result.SessionID, &deployment, &computer)
			if err == nil {
				existing = true
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		if existing {
			var pinned *uuid.UUID
			if err = tx.QueryRow(ctx, `SELECT slack_channel_id FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, result.SessionID).Scan(&pinned); err != nil {
				return err
			}
			if req.SlackChannelID != nil {
				if pinned == nil {
					return ErrSlackDestinationConflict
				}
				var channel string
				if err = tx.QueryRow(ctx, `SELECT slack_channel_id FROM slack_channels WHERE id=$1`, *pinned).Scan(&channel); err != nil {
					return err
				}
				if channel != *req.SlackChannelID {
					return ErrSlackDestinationConflict
				}
			}
			route = pinned
		} else if (prepared != nil && !routeActive) || (prepared == nil && req.SlackChannelID != nil) {
			return ErrConversationChanged
		}
		if !existing && (caller.Kind != "session" || method == "start") {
			if policy.deployment == nil {
				return ErrNotReady
			}
			deployment = *policy.deployment
		}
		if !existing && prepared != nil {
			if err = validateSlackStart(ctx, tx, req.EnvironmentID, agentID, deployment, caller, prepared); err != nil {
				return err
			}
			routeID, routeErr := ResolveSlackRoute(ctx, tx, prepared.Target)
			if routeErr != nil {
				return routeErr
			}
			route = &routeID
		}
		var revoked bool
		if err = tx.QueryRow(ctx, `SELECT execution_revoked_at IS NOT NULL FROM deployments WHERE environment_id=$1 AND id=$2 FOR SHARE`, req.EnvironmentID, deployment).Scan(&revoked); err != nil {
			return err
		}
		if revoked {
			return ErrDenied
		}
		req.Input, err = conversation.Input(req.Input)
		if err != nil {
			return err
		}
		if !existing {
			if !policy.configured {
				return ErrNotReady
			}
			var definitionKey string
			if err = tx.QueryRow(ctx, `SELECT computer_definition_key FROM agent_definitions WHERE environment_id=$1 AND agent_id=$2 AND deployment_id=$3`, req.EnvironmentID, agentID, deployment).Scan(&definitionKey); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotReady
				}
				return err
			}
			computer = req.ComputerID
			if computer == uuid.Nil() {
				computer, err = createAdmissionComputer(ctx, tx, trust, req.EnvironmentID, deployment, definitionKey, policy)
				if err != nil {
					return err
				}
			}
		}
		ids := []uuid.UUID{}
		if existing {
			ids = append(ids, result.SessionID)
		}
		if caller.Kind == "session" {
			ids = append(ids, caller.ID)
		}
		locked, err := lockSessionsAndComputers(ctx, tx, req.EnvironmentID, ids, []uuid.UUID{computer})
		if err != nil {
			return err
		}
		if caller.Kind == "session" {
			if err = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, locked[caller.ID]); err != nil {
				return err
			}
		} else {
			if err = authorizeStart(ctx, tx, caller, req.EnvironmentID); err != nil {
				return err
			}
			if existing {
				if err = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, owners{}); err != nil {
					return err
				}
			}
		}
		if existing {
			if locked[result.SessionID].lifecycle != "open" || (req.ComputerID != uuid.Nil() && req.ComputerID != computer) {
				return ErrConflict
			}
		}
		if err = computerAcceptsAdmission(ctx, tx, req.EnvironmentID, computer, !existing); err != nil {
			return err
		}
		if !existing && req.ComputerID != uuid.Nil() {
			// Current Programs and registered Computers use one supported seed/runtime
			// profile. Explicit placement preserves the actual Computer configuration.
			var profile string
			if err = tx.QueryRow(ctx, `SELECT spec.seed->>'profile' FROM computers c JOIN computer_preparation_specs spec ON (spec.environment_id,spec.id)=(c.environment_id,c.preparation_spec_id) WHERE c.environment_id=$1 AND c.id=$2 AND c.resources IS NOT NULL`, req.EnvironmentID, computer).Scan(&profile); err != nil {
				return err
			}
			if profile != definition.ComputerSeedProfile {
				return ErrInvalidInput
			}
		}
		if err = consumeAdmission(ctx, tx, req.EnvironmentID, policy); err != nil {
			return err
		}
		if !existing {
			result.SessionID = uuid.NewV7()
			root := result.SessionID
			var parent, requester, origin any
			causal := 0
			if caller.Kind == "session" {
				if err = tx.QueryRow(ctx, `SELECT causal_depth FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, caller.ID).Scan(&causal); err != nil {
					return err
				}
				causal++
				requester = caller.ID
				origin = nullableID(caller.TurnID)
				if method == "spawn" {
					parent = caller.ID
					root = locked[caller.ID].root
				}
				if causal > policy.maxCausal {
					return ErrInvalidInput
				}
			}
			if _, err = tx.Exec(ctx, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,origin_turn_id,causal_depth,session_key,slack_channel_id,history_retention_mode,history_retention_seconds) SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,history_retention_mode,history_retention_seconds FROM environments WHERE id=$1`, req.EnvironmentID, result.SessionID, agentID, deployment, computer, root, parent, requester, origin, causal, req.SessionKey, route); err != nil {
				return err
			}
			result.Created = true
			if slackBinding.thread != uuid.Nil() {
				if err = insertSlackSource(ctx, tx, req.EnvironmentID, result.SessionID, slackBinding.thread); err != nil {
					return err
				}
			}
		}
		result.TurnID = uuid.NewV7()
		if err = tx.QueryRow(ctx, `UPDATE sessions SET next_turn_seq=next_turn_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_turn_seq-1`, req.EnvironmentID, result.SessionID).Scan(&result.Sequence); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO turns(environment_id,id,session_id,computer_id,seq,caller_kind,caller_id,admission_method,target_id,retry_key,request_digest,input,origin_turn_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,$13)`, req.EnvironmentID, result.TurnID, result.SessionID, computer, result.Sequence, caller.Kind, caller.ID, method, agentID, req.RetryKey, digest[:], req.Input, nullableID(caller.TurnID)); err != nil {
			return err
		}
		if !existing && prepared != nil {
			if route == nil {
				return ErrConversationChanged
			}
			if err = createSlackRoot(ctx, tx, req.EnvironmentID, result.SessionID, result.TurnID, *route, *prepared); err != nil {
				return err
			}
		}
		return event(ctx, tx, req.EnvironmentID, result.SessionID, result.TurnID, "turn.queued")
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

func authorizeStart(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID) error {
	var allowed bool
	var err error
	switch caller.Kind {
	case "user":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environments e JOIN org_members m ON m.org_id=e.org_id JOIN users u ON u.id=m.user_id WHERE e.id=$1 AND m.user_id=$2 AND m.disabled_at IS NULL AND u.disabled_at IS NULL AND m.role IN ('owner','admin','developer'))`, env, caller.ID).Scan(&allowed)
	case "api_key":
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM api_keys k JOIN environments e ON (e.id,e.project_id,e.org_id)=(k.environment_id,k.project_id,k.org_id) WHERE e.id=$1 AND k.id=$2 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()) AND k.role IN ('owner','admin','developer') AND 'agents.start'=ANY(k.permissions))`, env, caller.ID).Scan(&allowed)
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
	if err := tx.QueryRow(ctx, `SELECT preparation_failed_at IS NULL AND deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2) AND (NOT $3 OR (integrity_fault_at IS NULL AND (initial_root_id IS NOT NULL OR preparation_deadline_at>clock_timestamp()))) FROM computers WHERE environment_id=$1 AND id=$2`, env, computer, newPlacement).Scan(&available); err != nil {
		return err
	}
	if !available {
		return ErrNotReady
	}
	return nil
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
	var spec uuid.UUID
	var resources, seed []byte
	var maxAge *int64
	if err := tx.QueryRow(ctx, `SELECT d.preparation_spec_id,d.resources,d.max_image_age_ms,s.seed FROM computer_definitions d JOIN computer_preparation_specs s ON (s.environment_id,s.id)=(d.environment_id,d.preparation_spec_id) WHERE d.environment_id=$1 AND d.deployment_id=$2 AND d.definition_key=$3`, env, deployment, key).Scan(&spec, &resources, &maxAge, &seed); err != nil {
		return uuid.Nil(), err
	}
	var actual definition.ResourcesManifest
	var base definition.ComputerSeedManifest
	if json.Unmarshal(resources, &actual) != nil || definition.ValidateResourcesManifest(actual) != nil || json.Unmarshal(seed, &base) != nil || base.Profile != definition.ComputerSeedProfile {
		return uuid.Nil(), ErrInvalidInput
	}
	physicalCPU, err := vm.ReservedCPUMillis(actual.MilliCPU)
	if err != nil || (actual.DiskMiB != nil && *actual.DiskMiB != disk.SeedCapacity/(1<<20)) || actual.MemoryMiB > math.MaxInt64/(1<<20) || physicalCPU > policy.maxCPU || actual.MemoryMiB*(1<<20) > policy.maxMemory || disk.SeedCapacity > policy.maxStorage {
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
	if _, err := tx.Exec(ctx, `INSERT INTO computers(environment_id,id,preparation_spec_id,origin_deployment_id,origin_definition_key,resources,storage_reservation_bytes,preparation_deadline_at,preparation_max_age_ms) VALUES($1,$2,$3,$4,$5,$6,$7,clock_timestamp()+$8*interval '1 millisecond',$9)`, env, id, spec, deployment, key, resources, disk.SeedCapacity, policy.preparationTimeout, maxAge); err != nil {
		return uuid.Nil(), err
	}
	if explicit == nil {
		if err := copyDefinitionSecretBindings(ctx, tx, env, deployment, key, id); err != nil {
			return uuid.Nil(), err
		}
	} else {
		bindings, err := registrationSecretBindings(*explicit, nil, nil, nil)
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
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_secret_bindings WHERE environment_id=$1 AND computer_id=$2 AND mode='protected')`, env, id).Scan(&protected); err != nil {
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
