import { request } from "./api";
import type { DefinitionScope, RetainedAgent } from "./definitions";

export type AgentChange = RetainedAgent & {
  can_apply: boolean;
  changed: boolean;
  blockers: { code: string }[];
  effects: {
    selected_agent_edges_removed: number;
    ineffective_assignments_removed: number;
    mixed_assignments_preserved: number;
    slack_connection_disconnected: string | null;
    trigger_activations_sealed: number;
    sessions_cancelled: number;
  };
  warnings: string[];
};
function path(agent: string, scope: DefinitionScope): string {
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/agents/${encodeURIComponent(agent)}`;
}
export function previewAgentChange(agent: string, scope: DefinitionScope): Promise<AgentChange> {
  return request(`${path(agent, scope)}?dry_run=true`, { method: "DELETE" });
}
export function applyAgentChange(agent: string, scope: DefinitionScope, revision: number): Promise<AgentChange> {
  return request(path(agent, scope), { method: "DELETE", headers: { "If-Match": `"${revision}"` } });
}
