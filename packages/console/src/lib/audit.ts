import { request } from "./api";

// Where an authority check applied, as recorded.
export type AuditCheckScope = { org_id?: string; project_id?: string; environment_id?: string; agent_id?: string };

// One authority check Helmr recorded for an event. Each type records its own
// fields; anything this Console does not name stays readable in the JSON.
export type AdminAuditCheck = {
  type: string;
  result: "passed" | "failed" | string;
  permission?: string;
  reason?: string;
  scope?: AuditCheckScope;
  subject_user_id?: string;
  subject_api_key_id?: string;
  evidence?: { role_id?: string; role_revision?: number; assignment_id?: string; assignment_revision?: number; member_id?: string; group_id?: string; subject?: string };
  attribute?: string;
  expected?: boolean;
  observed?: boolean;
  observation_source?: string;
  member_id?: string;
  user_id?: string;
  session_id?: string;
  generation?: number;
  observed_generation?: number;
  initiator_kind?: string;
  initiator_id?: string;
  thread_id?: string;
  presented_version?: number;
  observed_version?: number;
  host_id?: string;
  target_kind?: string;
  target_id?: string;
  epoch?: number;
  [key: string]: unknown;
};

type AuditIdentity = { display_name: string; email?: string; source: string };
type AuditAuthentication = {
  result: "verified" | "failed" | "absent" | "not_attempted";
  origin: "request" | "internal"; provider: string | null;
  credential_kind: string | null; credential_id: string | null; credential_version: number | null;
};
type AuditMetadata = {
  id: string; org_id: string | null; project_id: string | null; environment_id: string | null;
  occurred_at: string; recorded_at: string; expires_at: string; retention_seconds: number;
  actor_operator_id: string | null; actor_user_id: string | null; actor_api_key_id: string | null; actor_session_id: string | null;
  actor_identity: AuditIdentity | null; authentication: AuditAuthentication;
  deployment_id: string | null; session_id: string | null; turn_id: string | null;
  computer_id: string | null; schema_version: number;
  action: string; outcome: string; target_kind: string; target_id: string | null;
  target_user_id: string | null; slack_team_id: string | null; slack_user_id: string | null;
  target_resolution: string; channel: string; operation_id: string;
};
export type AuditActorKind = "user" | "api_key" | "session" | "platform_service" | "unidentified";
export type AuditRestrictedSection = "actor_identity" | "authentication" | "authorization" | "runtime" | "causality" | "correlation" | "details" | "transport";
type PermissionScope = { org_id: string; project_id?: string; environment_id?: string; agent_id?: string };
type CheckScope = { kind: string; org_id?: string; project_id?: string; environment_id?: string; session_id?: string };
type PermissionWitness = { permission: string; scope: PermissionScope; assignment_id: string; assignment_revision: number; role_id: string; role_revision: number; member_id?: string; group_id?: string; subject: string };
export type AuditCheck =
  | { type: "permission"; subject_user_id?: string; subject_api_key_id?: string; permission: string; scope?: PermissionScope; result: string; reason?: string; evidence?: PermissionWitness }
  | { type: "membership_attribute"; member_id: string; attribute: string; expected: boolean; observed: boolean; scope: CheckScope; result: string }
  | { type: "principal_currency"; subject_user_id?: string; subject_api_key_id?: string; scope: CheckScope; observation_source: string; result: string; reason: string; presented_version?: number; observed_version?: number };

type AccessSnapshot = {
  api_key_id?: string; api_key_name?: string; grant_id?: string; agents_mode?: string; agent_ids?: string[]; revision?: number;
  member_name?: string; group_name?: string; role_name?: string; project_name?: string; environment_name?: string; agent_key?: string;
  name?: string; description?: string; permissions?: string[]; subject_kind?: string; member_id?: string; group_id?: string; role_id?: string;
  project_id?: string; environment_id?: string; target_kind?: string; agent_id?: string; user_id?: string;
};
type KeyPolicy = { name: string; expires_at: string | null };
// Action/outcome selects one closed detail shape. Empty detail objects carry no
// additional facts; the server rejects unclassified captured fields before output.
export type AuditDetails =
  | Record<string, never>
  | { was_owner: boolean; is_owner: boolean }
  | { user_id?: string; display_name?: string; membership_id: string; is_owner: boolean }
  | { expires_at: string | null }
  | KeyPolicy | { before: KeyPolicy; after: KeyPolicy }
  | { permissions: string[] }
  | { old_retention_seconds: number | null; new_retention_seconds: number | null }
  | { cause_agent_id?: string; reason?: string; before: AccessSnapshot | null; after: AccessSnapshot | null }
  | { cause_agent_id: string; reason: string }
  | { revision: number; selected_agent_edges_removed: number; ineffective_assignments_removed: number; mixed_assignments_preserved: number; slack_connection_disconnected: string | null; trigger_activations_sealed: number }
  | { method: string; provider?: string; subject?: string; device_code_id?: string; authorizing_credential_id?: string }
  | { reason: string; operation?: string }
  | { tool: string }
  | { exposure_id: string; version_id: string; revocation_generation: number; command_id?: string; image_build_id?: string }
  | { before_ms: number | null; after_ms: number | null }
  | { kind: string; request_id: string; reason?: string }
  | { request_id: string; hold_id?: string }
  | { image_build_id: string } | { hold_id: string } | { checkpoint_id: string }
  | { sequence?: number; request_id?: string; publication_revision?: number }
  | { from: string; to: string; result_count?: number; environment_ids?: string[]; requested_environment_id?: string; actions?: string[]; operation_id?: string; event_id?: string; actor_kind?: string; actor_id?: string; target_kind?: string; target_id?: string; target_user_id?: string; slack_team_id?: string; slack_user_id?: string; outcome?: string };
export type AuditEvent = AuditMetadata & {
  actor_kind: AuditActorKind; restricted: AuditRestrictedSection[];
  authorization: { decision: "allowed" | "denied" | "not_evaluated"; checks: AuditCheck[]; platform_authority: boolean };
  causality: { initiator_kind?: string; initiator_id?: string; source_id?: string }; details: AuditDetails;
};
export type AdminAuditEvent = AuditMetadata & {
  actor_worker_host_id: string | null; actor_component: string | null;
  runtime_generation: number | null; process_epoch: number | null; source_ip: string | null; cause_event_id: string | null;
  authorization: { decision: "allowed" | "denied" | "not_evaluated"; checks: AdminAuditCheck[] }; causality: unknown; details: unknown;
};
export type AuditBefore = { recorded_at: string; id: string };
export type AuditPage = { events: AuditEvent[]; next_before?: AuditBefore };
export type AdminAuditPage = { events: AdminAuditEvent[]; next_before?: AuditBefore };
export type AuditFilter = {
  from: string; to: string;
  actions?: string[]; actor_kind?: string; actor_id?: string; target_kind?: string; target_id?: string;
  outcome?: string; environment_id?: string; organization_id?: string; operation_id?: string; event_id?: string;
};

export type AuditAction = { action: string; target_kind: string; outcomes: string[]; category: string };

function auditQuery(filter: AuditFilter, before?: AuditBefore): URLSearchParams {
  const query = new URLSearchParams({ limit: "100" });
  for (const [key, value] of Object.entries(filter)) {
    if (key === "actions") for (const action of value as string[]) query.append("action", action);
    else if (typeof value === "string" && value.trim()) query.set(key, value.trim());
  }
  if (before) { query.set("before_recorded_at", before.recorded_at); query.set("before_id", before.id); }
  return query;
}
export type AnyAuditEvent = AuditEvent | AdminAuditEvent;
export function listAuditActions(adminView = false): Promise<{ actions: AuditAction[] }> {
  return request(`${adminView ? "/admin/api/v1" : "/api"}/audit-events/actions`);
}
export function searchAuditEvents(filter: AuditFilter, before?: AuditBefore): Promise<AuditPage> {
  return request<AuditPage>(`/api/audit-events?${auditQuery(filter, before)}`);
}
export function searchAdminAuditEvents(filter: AuditFilter, before?: AuditBefore): Promise<AdminAuditPage> {
  return request<AdminAuditPage>(`/admin/api/v1/audit-events?${auditQuery(filter, before)}`);
}
