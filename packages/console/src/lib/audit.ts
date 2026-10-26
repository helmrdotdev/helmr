import { request } from "./api";

// Where an authority check applied, as recorded.
export type AuditCheckScope = { org_id?: string; project_id?: string; environment_id?: string; agent_id?: string };

// One authority check Helmr recorded for an event. Each type records its own
// fields; anything this Console does not name stays readable in the JSON.
export type AuditCheck = {
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

export type AuditEvent = {
  id: string; org_id: string | null; project_id: string | null; environment_id: string | null;
  occurred_at: string; recorded_at: string; expires_at: string; retention_seconds: number;
  actor_user_id: string | null; actor_api_key_id: string | null; actor_session_id: string | null;
  actor_worker_host_id: string | null; actor_component: string | null;
  actor_identity: { display_name: string; email?: string; source: string } | null;
  runtime_generation: number | null; process_epoch: number | null;
  authentication: {
    result: "verified" | "failed" | "absent" | "not_attempted";
    origin: "request" | "internal"; provider: string | null;
    credential_kind: string | null; credential_id: string | null; credential_version: number | null;
  };
  deployment_id: string | null; session_id: string | null; turn_id: string | null;
  computer_id: string | null; cause_event_id: string | null; schema_version: number;
  action: string; outcome: string; target_kind: string; target_id: string | null;
  target_user_id: string | null; slack_team_id: string | null; slack_user_id: string | null;
  target_resolution: string; source_ip: string | null; channel: string; operation_id: string;
  authorization: { decision: "allowed" | "denied" | "not_evaluated"; checks: AuditCheck[] }; causality: unknown; details: unknown;
};
export type AuditBefore = { recorded_at: string; id: string };
export type AuditPage = { events: AuditEvent[]; next_before?: AuditBefore };

// A search: a time range of at most 31 days and any of these filters. Several
// actions match any of them.
export type AuditFilter = {
  from: string; to: string;
  actions?: string[]; actor_kind?: string; actor_id?: string; target_kind?: string; target_id?: string;
  outcome?: string; environment_id?: string; organization_id?: string; operation_id?: string; event_id?: string;
};

export type AuditAction = { action: string; target_kind: string; outcomes: string[]; category: string };

const base = (installation: boolean) => (installation ? "/admin/api/v1" : "/api");

export function searchAuditEvents(filter: AuditFilter, before?: AuditBefore, installation = false): Promise<AuditPage> {
  const query = new URLSearchParams({ limit: "100" });
  for (const [key, value] of Object.entries(filter)) {
    if (key === "actions") for (const action of value as string[]) query.append("action", action);
    else if (typeof value === "string" && value.trim()) query.set(key, value.trim());
  }
  if (before) { query.set("before_recorded_at", before.recorded_at); query.set("before_id", before.id); }
  return request<AuditPage>(`${base(installation)}/audit-events?${query}`);
}

// Every action the audit trail can record, with its category.
export function listAuditActions(): Promise<{ actions: AuditAction[] }> {
  return request<{ actions: AuditAction[] }>("/api/audit-events/actions");
}
