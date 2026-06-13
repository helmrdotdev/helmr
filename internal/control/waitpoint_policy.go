package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/waitpoint"
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
		writeError(w, unavailable(errors.New("waitpoint policy storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointPolicies, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	rows, err := s.db.ListWaitpointPolicies(r.Context(), db.ListWaitpointPoliciesParams{
		OrgID:         pgvalue.UUID(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		writeError(w, errors.New("list waitpoint policies"))
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
		writeError(w, unavailable(errors.New("waitpoint policy storage is not configured")))
		return
	}
	var request api.CreateWaitpointPolicyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid waitpoint policy request JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointPolicies, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	normalized, err := normalizeWaitpointPolicyInput(request.Name, request.Label, request.Config)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	policy, err := s.db.CreateWaitpointPolicy(r.Context(), db.CreateWaitpointPolicyParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          normalized.name,
		Label:         normalized.label,
		Config:        normalized.config,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, badRequest(errors.New("waitpoint policy name is already in use")))
			return
		}
		writeError(w, errors.New("create waitpoint policy"))
		return
	}
	writeJSON(w, http.StatusCreated, waitpointPolicyResponse(policy))
}

func (s *Server) getWaitpointPolicy(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("waitpoint policy storage is not configured")))
		return
	}
	name, err := waitpointPolicyNameParam(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointPolicies, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	policy, err := s.db.GetWaitpointPolicyByName(r.Context(), db.GetWaitpointPolicyByNameParams{
		OrgID:         pgvalue.UUID(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          name,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("waitpoint policy not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get waitpoint policy"))
		return
	}
	writeJSON(w, http.StatusOK, waitpointPolicyResponse(policy))
}

func (s *Server) updateWaitpointPolicy(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("waitpoint policy storage is not configured")))
		return
	}
	name, err := waitpointPolicyNameParam(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointPolicies, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	var request api.UpdateWaitpointPolicyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid waitpoint policy request JSON: %w", err)))
		return
	}
	normalized, err := normalizeWaitpointPolicyInput(name, request.Label, request.Config)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	policy, err := s.db.UpdateWaitpointPolicy(r.Context(), db.UpdateWaitpointPolicyParams{
		OrgID:         pgvalue.UUID(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          name,
		Label:         normalized.label,
		Config:        normalized.config,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("waitpoint policy not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("update waitpoint policy"))
		return
	}
	writeJSON(w, http.StatusOK, waitpointPolicyResponse(policy))
}

func (s *Server) deleteWaitpointPolicy(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("waitpoint policy storage is not configured")))
		return
	}
	name, err := waitpointPolicyNameParam(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointPolicies, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	rows, err := s.db.DeleteWaitpointPolicy(r.Context(), db.DeleteWaitpointPolicyParams{
		OrgID:         pgvalue.UUID(scope.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          name,
	})
	if err != nil {
		writeError(w, errors.New("delete waitpoint policy"))
		return
	}
	if rows == 0 {
		writeError(w, notFound(errors.New("waitpoint policy not found")))
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
			if err := waitpoint.ValidateEmailRecipient(reviewer.Address); err != nil {
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
				if err := waitpoint.ValidateEmailRecipient(recipient); err != nil {
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

func (s *Server) resolveWaitpointPolicy(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, name string) (*resolvedWaitpointPolicy, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		if !waitpointPolicyNamePattern.MatchString(name) {
			return nil, fmt.Errorf("waitpoint policy %q must match %s", name, waitpointPolicyNamePattern.String())
		}
		policy, err := s.db.GetWaitpointPolicyByName(ctx, db.GetWaitpointPolicyByNameParams{
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			Name:          name,
		})
		if isNoRows(err) {
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
		ID:            pgvalue.MustUUIDValue(policy.ID).String(),
		ProjectID:     pgvalue.MustUUIDValue(policy.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(policy.EnvironmentID).String(),
		Name:          policy.Name,
		Label:         policy.Label,
		Config:        append(json.RawMessage(nil), policy.Config...),
		CreatedAt:     pgvalue.Time(policy.CreatedAt),
		UpdatedAt:     pgvalue.Time(policy.UpdatedAt),
	}
}
