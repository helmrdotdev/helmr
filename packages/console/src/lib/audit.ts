import { request } from "./api";

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
  computer_id: string | null; command_id: string | null; cause_event_id: string | null; schema_version: number;
  action: string; outcome: string; target_kind: string; target_id: string | null;
  target_user_id: string | null; slack_team_id: string | null; slack_user_id: string | null;
  target_resolution: string; source_ip: string | null; channel: string; operation_id: string;
  authorization: { decision: "allowed" | "denied" | "not_evaluated"; checks: unknown[] }; causality: unknown; details: unknown;
};
export type AuditBefore = { recorded_at: string; id: string };
export type AuditPage = { events: AuditEvent[]; next_before?: AuditBefore };
export type AuditFilter = { from: string; to: string; action: string; actor_id: string; target_id: string; outcome: string; environment_id: string; organization_id: string };

export function searchAuditEvents(filter: AuditFilter, before?: AuditBefore, installation = false): Promise<AuditPage> {
  const query = new URLSearchParams({ limit: "100" });
  for (const [key, value] of Object.entries(filter)) if (value.trim()) query.set(key, value.trim());
  if (before) { query.set("before_recorded_at", before.recorded_at); query.set("before_id", before.id); }
  return request<AuditPage>(`${installation ? "/admin/api/v1" : "/api"}/audit-events?${query}`);
}
