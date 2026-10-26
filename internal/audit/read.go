package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// DecodeDetails validates a retained closed payload before a reader projects it.
// It does not reconstruct facts from current resources or accept unknown fields.
func DecodeDetails(action, outcome string, data []byte) (Details, error) {
	decoder, ok := detailDecoder(action)
	if !ok {
		return nil, errors.New("unclassified audit details")
	}
	if action != "computer.control_settled" && (outcome == "denied" || outcome == "failed" || outcome == "unknown") {
		decoder = decodeDetail[FailureDetails]
	}
	details, err := decoder(data)
	if err != nil {
		return nil, err
	}
	if _, err = marshalEventDetails(action, outcome, details); err != nil {
		return nil, err
	}
	return details, nil
}

func detailDecoder(action string) (func([]byte) (Details, error), bool) {
	switch action {
	case "identity.sign_in", "identity.session_revoked", "identity.device_authorized", "identity.device_started", "identity.device_denied", "identity.magic_link_requested":
		return decodeDetail[AuthenticationDetails], true
	case "audit.searched":
		return decodeDetail[SearchDetails], true
	case "audit.retention_observed":
		return decodeDetail[RetentionObservedDetails], true
	case "audit.purge_completed":
		return decodeDetail[PurgeDetails], true
	case "audit.restore_recorded":
		return decodeDetail[RestoreDetails], true
	case "secret.exposed":
		return decodeDetail[SecretExposureDetails], true
	case "mcp.admitted":
		return decodeDetail[MCPDetails], true
	case "region.created", "region.updated":
		return decodeDetail[RegionDetails], true
	case "computer.retention_changed":
		return decodeDetail[ComputerRetentionDetails], true
	case "computer.control_requested", "computer.control_settled":
		return decodeDetail[ComputerControlDetails], true
	case "session.close_requested", "session.cancel_requested", "session.interrupt_requested", "session.resume_requested", "turn.interrupt_requested":
		return decodeDetail[SessionControlDetails], true
	case "template.refresh_requested":
		return decodeDetail[TemplateRefreshDetails], true
	case "session.authority_changed":
		return decodeDetail[AuthorityChangedDetails], true
	case "computer.restore_completed", "computer.checkpoint_published":
		return decodeDetail[CheckpointDetails], true
	case "trigger.content_read", "session.delivery_check_requested", "session.delivery_abandoned", "input.requested", "message.delivered", "message.rejected", "turn.interrupted", "computer.created", "command.cancel_requested", "message.admitted", "worker_group.updated", "session.created", "session.closed", "session.cancelled", "session.content_read", "turn.admitted", "turn.completed", "turn.cancelled", "input.answered", "input.rejected", "command.admitted", "command.completed", "command.cancelled", "command.output_read":
		return decodeDetail[LifecycleDetails], true
	case "slack_connection.disconnected":
		return decodeDisconnectDetails, true
	case "agent.deleted", "agent.restored":
		return decodeDetail[AgentChangeDetails], true
	case "member.ownership_changed":
		return decodeDetail[OwnershipChangedDetails], true
	case "access.recovered", "permission_group.created", "permission_group.updated", "permission_group.deleted", "permission_group.member_added", "permission_group.member_removed", "role.created", "role.updated", "role.deleted", "role.standard_created", "role_grant.created", "role_grant.updated", "role_grant.deleted":
		return decodeDetail[AccessChangeDetails], true
	case "member.granted", "member.removed", "invitation.accepted":
		return decodeDetail[MembershipDetails], true
	case "installation_admin.granted":
		return decodeDetail[RoleGrantedDetails], true
	case "invitation.revoked", "invitation.expired":
		return decodeDetail[InvitationStateDetails], true
	case "invitation.created":
		return decodeDetail[InvitationCreatedDetails], true
	case "api_key.updated":
		return decodeDetail[APIKeyChangedDetails], true
	case "api_key.issued":
		return decodeDetail[APIKeyIssuedDetails], true
	case "slack_connection.authorized":
		return decodeDetail[PermissionsDetails], true
	case "project.retention_changed":
		return decodeDetail[RetentionChangedDetails], true
	case "organization.created", "api_key.rotated", "api_key.revoked", "secret.created", "secret.rotated", "secret.revoked", "project.created", "project.updated", "environment.created", "environment.updated", "deployment.published", "deployment.promoted", "slack_app.credentials_configured", "slack_installation.authorization_lost", "slack_user.linked", "slack_user.unlinked", "worker_group.created", "worker_group.token_rotated", "worker_group.active", "worker_group.paused", "worker_group.draining", "worker_group.disabled", "worker_pool.created", "worker_pool.primary_selected", "worker_pool.resource_policy_updated", "worker_pool.draining", "worker_pool.disabled", "worker_host.draining", "worker_host.lost", "worker_host.provider_absence_confirmed", "worker_scale_in.completed", "runtime.operation_refused", "turn.failed", "command.failed", "mcp.refused", "computer.deleted":
		return decodeEmptyDetails, true
	}
	return nil, false
}
func decodeDetail[T Details](data []byte) (Details, error) {
	var value T
	if err := decodeObject(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}
func decodeEmptyDetails(data []byte) (Details, error) {
	var empty struct{}
	return nil, decodeObject(data, &empty)
}
func decodeDisconnectDetails(data []byte) (Details, error) {
	var d AgentCleanupDetails
	if err := decodeObject(data, &d); err == nil {
		return d, nil
	}
	return decodeEmptyDetails(data)
}

func decodeObject(data []byte, target any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return errors.New("audit evidence must be an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("trailing audit evidence")
	}
	// Re-encoding the closed type identifies its required fields, including zero
	// values. Optional omitempty/omitzero fields remain optional.
	expected, err := json.Marshal(target)
	if err != nil {
		return err
	}
	return requireObjectFields(data, expected)
}

// Array elements also carry typed observations; JSON null must not silently
// become a zero string, boolean or object inside captured lists.
func requireArrayFields(data, expected []byte) error {
	var actual, required []json.RawMessage
	if err := json.Unmarshal(data, &actual); err != nil {
		return err
	}
	if err := json.Unmarshal(expected, &required); err != nil {
		return err
	}
	for i, value := range required {
		item := bytes.TrimSpace(actual[i])
		if bytes.Equal(item, []byte("null")) && !bytes.Equal(value, []byte("null")) {
			return errors.New("null audit evidence list item")
		}
		if len(value) > 0 && value[0] == '{' {
			if err := requireObjectFields(item, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireObjectFields(data, expected []byte) error {
	var actualFields, requiredFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &actualFields); err != nil {
		return err
	}
	if err := json.Unmarshal(expected, &requiredFields); err != nil {
		return err
	}
	for key, actual := range actualFields {
		if bytes.Equal(bytes.TrimSpace(actual), []byte("null")) && !bytes.Equal(requiredFields[key], []byte("null")) {
			return errors.New("null audit evidence field: " + key)
		}
	}
	for key, value := range requiredFields {
		actual, ok := actualFields[key]
		if !ok {
			return errors.New("missing audit evidence field: " + key)
		}
		if len(value) > 0 && value[0] == '{' {
			if err := requireObjectFields(actual, value); err != nil {
				return err
			}
		} else if len(value) > 0 && value[0] == '[' {
			if err := requireArrayFields(actual, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// DecodeAuthorization restores only the captured closed check vocabulary.
func DecodeAuthorization(data []byte) (Authorization, error) {
	var wire struct {
		Decision string            `json:"decision"`
		Checks   []json.RawMessage `json:"checks"`
	}
	if err := decodeObject(data, &wire); err != nil {
		return Authorization{}, err
	}
	if wire.Checks == nil {
		return Authorization{}, errors.New("audit authorization checks must be an array")
	}
	result := Authorization{Decision: wire.Decision, Checks: []Check{}}
	for _, raw := range wire.Checks {
		var tag struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &tag); err != nil {
			return Authorization{}, err
		}
		var c Check
		switch tag.Type {
		case "permission":
			c = &PermissionCheck{}
		case "user_attribute":
			c = &UserAttributeCheck{}
		case "membership_attribute":
			c = &MembershipAttributeCheck{}
		case "runtime_binding":
			c = &RuntimeBindingCheck{}
		case "session_origin":
			c = &OriginCheck{}
		case "slack_thread":
			c = &SlackThreadCheck{}
		case "principal_currency":
			c = &PrincipalCurrencyCheck{}
		case "host_assignment":
			c = &HostAssignmentCheck{}
		default:
			return Authorization{}, errors.New("unknown audit authorization check")
		}
		if err := decodeObject(raw, c); err != nil {
			return Authorization{}, err
		}
		if err := c.validate(); err != nil {
			return Authorization{}, err
		}
		result.Checks = append(result.Checks, c)
	}
	return result, nil
}

// DecodeCausality uses the same closed shape validation as captured details.
func DecodeCausality(data []byte) (Causality, error) {
	var cause Causality
	err := decodeObject(data, &cause)
	return cause, err
}
