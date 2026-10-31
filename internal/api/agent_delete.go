package api

import "time"

// AgentStatus describes a retained identity independently of deployment membership.
type AgentStatus struct {
	Agent               string     `json:"agent"`
	CurrentDeploymentID *string    `json:"current_deployment_id"`
	DefinedInCurrent    bool       `json:"defined_in_current"`
	DeletedAt           *time.Time `json:"deleted_at"`
	Revision            int64      `json:"revision"`
}

// AgentDeletionBlocker names one prerequisite that keeps deletion from
// applying.
type AgentDeletionBlocker struct {
	Code string `json:"code"`
}
type AgentDeletionEffects struct {
	SelectedAgentEdgesRemoved     int     `json:"selected_agent_edges_removed"`
	IneffectiveAssignmentsRemoved int     `json:"ineffective_assignments_removed"`
	MixedAssignmentsPreserved     int     `json:"mixed_assignments_preserved"`
	SlackConnectionDisconnected   *string `json:"slack_connection_disconnected"`
	TriggerActivationsSealed      int64   `json:"trigger_activations_sealed"`
	SessionsCancelled             int64   `json:"sessions_cancelled"`
}
type AgentChangeResponse struct {
	AgentStatus
	CanApply bool                   `json:"can_apply"`
	Changed  bool                   `json:"changed"`
	Blockers []AgentDeletionBlocker `json:"blockers"`
	Effects  AgentDeletionEffects   `json:"effects"`
	Warnings []string               `json:"warnings"`
}
