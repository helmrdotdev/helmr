import { request } from "./api";

type Trigger = {
  id: string;
  agent: string;
  deployment_id: string;
  trigger_key: string;
  input: unknown;
  cron: { pattern: string; timezone: string };
  active_from: string;
  active_until?: string;
  next_fire_at?: string;
};

type ListTriggersResponse = {
  triggers: Trigger[];
  next_cursor?: string;
};

type TriggerScope = {
  projectID: string;
  environmentID: string;
};

export async function listTriggers(scope: TriggerScope, cursor?: string, agent?: string): Promise<ListTriggersResponse> {
  const query = new URLSearchParams();
  if (cursor) query.set("cursor", cursor);
  if (agent) query.set("agent", agent);
  const suffix = query.size ? `?${query}` : "";
  return request<ListTriggersResponse>(triggerPath(scope) + suffix);
}

function triggerPath(scope: TriggerScope): string {
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/triggers`;
}
