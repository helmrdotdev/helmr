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

export async function listTriggers(scope: TriggerScope, cursor?: string, agent?: string, view: "current" | "all" = "current", key?: string): Promise<ListTriggersResponse> {
  const query = new URLSearchParams();
  if (view === "all") query.set("view", view);
  if (key) query.set("key", key);
  if (cursor) query.set("cursor", cursor);
  if (agent) query.set("agent", agent);
  const suffix = query.size ? `?${query}` : "";
  return request<ListTriggersResponse>(triggerPath(scope) + suffix);
}

function triggerPath(scope: TriggerScope): string {
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/triggers`;
}

export type TriggerOccurrence = { scheduled_at: string; evaluated_at: string; disposition: string; reason: string | null; session_id: string | null; turn_id: string | null };
export function listTriggerOccurrences(scope: TriggerScope, activation: string, cursor?: string): Promise<{ occurrences: TriggerOccurrence[]; next_cursor?: string }> {
  const query = new URLSearchParams(); if (cursor) query.set("cursor", cursor);
  return request(`${triggerPath(scope)}/${encodeURIComponent(activation)}/occurrences${query.size ? `?${query}` : ""}`);
}
