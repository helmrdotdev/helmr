package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var waitpointPolicyNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

type resolvedWaitpointPolicy struct {
	Name      string          `json:"name"`
	Label     string          `json:"label"`
	Config    json.RawMessage `json:"config"`
	IsDefault bool            `json:"is_default"`
}

func (s *Server) listWaitpointPolicies(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("waitpoint policy storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.waitpointPolicyScope(r.Context(), actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errPermissionRequired)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rows, err := s.db.ListWaitpointPolicies(r.Context(), db.ListWaitpointPoliciesParams{
		OrgID:         ids.ToPG(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("list waitpoint policies"))
		return
	}
	response := api.ListWaitpointPoliciesResponse{Policies: make([]api.WaitpointPolicyResponse, 0, len(rows))}
	for _, row := range rows {
		response.Policies = append(response.Policies, waitpointPolicyResponse(row))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) createWaitpointPolicy(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("waitpoint policy storage is not configured"))
		return
	}
	var request api.CreateWaitpointPolicyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid waitpoint policy request JSON: %w", err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.waitpointPolicyScope(r.Context(), actor, request.ProjectID, request.EnvironmentID)
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errPermissionRequired)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	normalized, err := normalizeWaitpointPolicyInput(request.Name, request.Label, request.Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	policy, err := s.db.CreateWaitpointPolicy(r.Context(), db.CreateWaitpointPolicyParams{
		ID:            ids.ToPG(ids.New()),
		OrgID:         ids.ToPG(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          normalized.name,
		Label:         normalized.label,
		Config:        normalized.config,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusBadRequest, errors.New("waitpoint policy name is already in use"))
			return
		}
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint policy"))
		return
	}
	writeJSON(w, http.StatusCreated, waitpointPolicyResponse(policy))
}

func (s *Server) getWaitpointPolicy(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("waitpoint policy storage is not configured"))
		return
	}
	name, err := waitpointPolicyNameParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.waitpointPolicyScope(r.Context(), actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errPermissionRequired)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	policy, err := s.db.GetWaitpointPolicyByName(r.Context(), db.GetWaitpointPolicyByNameParams{
		OrgID:         ids.ToPG(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          name,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("waitpoint policy not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("get waitpoint policy"))
		return
	}
	writeJSON(w, http.StatusOK, waitpointPolicyResponse(policy))
}

func (s *Server) updateWaitpointPolicy(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("waitpoint policy storage is not configured"))
		return
	}
	name, err := waitpointPolicyNameParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.waitpointPolicyScope(r.Context(), actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errPermissionRequired)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var request api.UpdateWaitpointPolicyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid waitpoint policy request JSON: %w", err))
		return
	}
	normalized, err := normalizeWaitpointPolicyInput(name, request.Label, request.Config)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	policy, err := s.db.UpdateWaitpointPolicy(r.Context(), db.UpdateWaitpointPolicyParams{
		OrgID:         ids.ToPG(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          name,
		Label:         normalized.label,
		Config:        normalized.config,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("waitpoint policy not found"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("update waitpoint policy"))
		return
	}
	writeJSON(w, http.StatusOK, waitpointPolicyResponse(policy))
}

func (s *Server) deleteWaitpointPolicy(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("waitpoint policy storage is not configured"))
		return
	}
	name, err := waitpointPolicyNameParam(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.waitpointPolicyScope(r.Context(), actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if errors.Is(err, errPermissionRequired) {
		writeError(w, http.StatusForbidden, errPermissionRequired)
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rows, err := s.db.DeleteWaitpointPolicy(r.Context(), db.DeleteWaitpointPolicyParams{
		OrgID:         ids.ToPG(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          name,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("delete waitpoint policy"))
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, errors.New("waitpoint policy not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type waitpointPolicyInput struct {
	name   string
	label  string
	config json.RawMessage
}

func normalizeWaitpointPolicyInput(name string, label string, config json.RawMessage) (waitpointPolicyInput, error) {
	name = strings.TrimSpace(name)
	if !waitpointPolicyNamePattern.MatchString(name) {
		return waitpointPolicyInput{}, fmt.Errorf("waitpoint policy name %q must match %s", name, waitpointPolicyNamePattern.String())
	}
	label = strings.TrimSpace(label)
	if len([]byte(label)) > 200 {
		return waitpointPolicyInput{}, errors.New("waitpoint policy label must be 200 bytes or fewer")
	}
	if len(config) == 0 {
		config = []byte(`{}`)
	}
	if !json.Valid(config) {
		return waitpointPolicyInput{}, errors.New("waitpoint policy config must be valid JSON")
	}
	var parsed api.WaitpointPolicyConfig
	if err := json.Unmarshal(config, &parsed); err != nil {
		return waitpointPolicyInput{}, fmt.Errorf("waitpoint policy config: %w", err)
	}
	if err := validateWaitpointPolicyConfig(parsed); err != nil {
		return waitpointPolicyInput{}, err
	}
	canonical, err := json.Marshal(parsed)
	if err != nil {
		return waitpointPolicyInput{}, err
	}
	if label == "" {
		label = name
	}
	return waitpointPolicyInput{name: name, label: label, config: canonical}, nil
}

func validateWaitpointPolicyConfig(config api.WaitpointPolicyConfig) error {
	for _, reviewer := range config.Reviewers {
		switch reviewer.Type {
		case "email":
			if err := validateEmailRecipient(reviewer.Address); err != nil {
				return errors.New("email reviewer address is required")
			}
		case "helmr_role":
			if strings.TrimSpace(reviewer.Role) == "" {
				return errors.New("helmr_role reviewer role is required")
			}
		case "":
			return errors.New("reviewer type is required")
		default:
			return fmt.Errorf("unsupported reviewer type %q", reviewer.Type)
		}
	}
	for _, delivery := range config.Deliveries {
		switch delivery.Type {
		case "email":
			if len(delivery.To) == 0 {
				return errors.New("email delivery must include at least one recipient")
			}
			for _, recipient := range delivery.To {
				if err := validateEmailRecipient(recipient); err != nil {
					return fmt.Errorf("invalid email delivery recipient %q", recipient)
				}
			}
		case "":
			return errors.New("delivery type is required")
		default:
			return fmt.Errorf("unsupported delivery type %q", delivery.Type)
		}
	}
	if config.OnTimeout != nil && config.OnTimeout.Type != "" && config.OnTimeout.Type != "expire" {
		return fmt.Errorf("unsupported waitpoint policy timeout type %q", config.OnTimeout.Type)
	}
	return nil
}

func (s *Server) waitpointPolicyScope(ctx context.Context, actor auth.Actor, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	scope, scopeProjectID, scopeEnvironmentID, err := s.requestScopeForPermission(ctx, actor, projectID, environmentID, auth.PermissionWaitpointPolicies, "waitpoint policy management")
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	if !actor.HasPermission(auth.PermissionWaitpointPolicies, scope) {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, errPermissionRequired
	}
	return scope, scopeProjectID, scopeEnvironmentID, nil
}

func (s *Server) resolveWaitpointPolicy(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, name string) (*resolvedWaitpointPolicy, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		if !waitpointPolicyNamePattern.MatchString(name) {
			return nil, fmt.Errorf("waitpoint policy %q must match %s", name, waitpointPolicyNamePattern.String())
		}
		policy, err := s.db.GetWaitpointPolicyByName(ctx, db.GetWaitpointPolicyByNameParams{
			OrgID:         ids.ToPG(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			Name:          name,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("waitpoint policy %q was not found", name)
		}
		if err != nil {
			return nil, fmt.Errorf("load waitpoint policy: %w", err)
		}
		return resolvedWaitpointPolicyFromDB(policy, false), nil
	}
	return nil, nil
}

func resolvedWaitpointPolicyFromDB(policy db.WaitpointPolicy, isDefault bool) *resolvedWaitpointPolicy {
	return &resolvedWaitpointPolicy{
		Name:      policy.Name,
		Label:     policy.Label,
		Config:    append(json.RawMessage(nil), policy.Config...),
		IsDefault: isDefault,
	}
}

func (policy resolvedWaitpointPolicy) snapshot() (json.RawMessage, error) {
	payload, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func waitpointPolicyNameParam(r *http.Request) (string, error) {
	name := strings.TrimSpace(chi.URLParam(r, "name"))
	if !waitpointPolicyNamePattern.MatchString(name) {
		return "", fmt.Errorf("waitpoint policy name %q must match %s", name, waitpointPolicyNamePattern.String())
	}
	return name, nil
}

func waitpointPolicyResponse(policy db.WaitpointPolicy) api.WaitpointPolicyResponse {
	return api.WaitpointPolicyResponse{
		ID:            ids.MustFromPG(policy.ID).String(),
		ProjectID:     ids.MustFromPG(policy.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(policy.EnvironmentID).String(),
		Name:          policy.Name,
		Label:         policy.Label,
		Config:        append(json.RawMessage(nil), policy.Config...),
		CreatedAt:     pgTime(policy.CreatedAt),
		UpdatedAt:     pgTime(policy.UpdatedAt),
	}
}

func waitpointPolicyEmailRecipients(config api.WaitpointPolicyConfig) []string {
	seen := map[string]struct{}{}
	recipients := []string{}
	for _, delivery := range config.Deliveries {
		if delivery.Type != "email" {
			continue
		}
		for _, raw := range delivery.To {
			recipient := normalizeEmailRecipient(raw)
			if recipient == "" {
				continue
			}
			if _, ok := seen[recipient]; ok {
				continue
			}
			seen[recipient] = struct{}{}
			recipients = append(recipients, recipient)
		}
	}
	return recipients
}

func normalizeEmailRecipient(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func validateEmailRecipient(value string) error {
	normalized := normalizeEmailRecipient(value)
	if normalized == "" {
		return errors.New("email is required")
	}
	address, err := mail.ParseAddress(normalized)
	if err != nil || address.Address != normalized {
		return errors.New("email is invalid")
	}
	return nil
}
