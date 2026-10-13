import { request } from "./api";

type Schedule = {
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

type ListSchedulesResponse = {
  schedules: Schedule[];
  next_cursor?: string;
};

type ScheduleScope = {
  projectID: string;
  environmentID: string;
};

export async function listSchedules(scope: ScheduleScope, cursor?: string, agent?: string): Promise<ListSchedulesResponse> {
  const query = new URLSearchParams();
  if (cursor) query.set("cursor", cursor);
  if (agent) query.set("agent", agent);
  const suffix = query.size ? `?${query}` : "";
  return request<ListSchedulesResponse>(schedulePath(scope) + suffix);
}

function schedulePath(scope: ScheduleScope): string {
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/schedules`;
}
