import { request } from "./api";
import type { DefinitionScope, RetainedAgent } from "./definitions";

export type AgentChange = RetainedAgent & {
  can_apply: boolean;
  changed: boolean;
  blockers: { code: string; count: number; session_ids?: string[]; samples_incomplete: boolean }[];
  effects: {
    selected_agent_edges_removed: number;
    ineffective_assignments_removed: number;
    mixed_assignments_preserved: number;
    slack_connection_disconnected: string | null;
    trigger_activations_sealed: number;
  };
  warnings: string[];
};
function path(agent: string, scope: DefinitionScope): string {
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/agents/${encodeURIComponent(agent)}`;
}
export function previewAgentChange(agent: string, scope: DefinitionScope, restore: boolean): Promise<AgentChange> {
  return restore
    ? request(`${path(agent, scope)}/restore`, { method: "POST", body: JSON.stringify({ dry_run: true }) })
    : request(`${path(agent, scope)}?dry_run=true`, { method: "DELETE" });
}
export function applyAgentChange(agent: string, scope: DefinitionScope, restore: boolean, revision: number): Promise<AgentChange> {
  const headers = { "If-Match": `"${revision}"` };
  return restore
    ? request(`${path(agent, scope)}/restore`, { method: "POST", headers, body: "{}" })
    : request(path(agent, scope), { method: "DELETE", headers });
}
