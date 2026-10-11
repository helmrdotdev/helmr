import { request } from "./api";

export type Schedule = {
  id: string;
  agent_id: string;
  deployment_id: string;
  trigger_key: string;
  input: unknown;
  cron: { pattern: string; timezone: string };
  active_from: string;
  active_until?: string;
  next_fire_at?: string;
};

export type ListSchedulesResponse = {
  schedules: Schedule[];
  next_cursor?: string;
};

export type ScheduleScope = {
  projectID: string;
  environmentID: string;
};

export async function listSchedules(scope: ScheduleScope, cursor?: string): Promise<ListSchedulesResponse> {
  const suffix = cursor ? `?cursor=${encodeURIComponent(cursor)}` : "";
  return request<ListSchedulesResponse>(schedulePath(scope) + suffix);
}

function schedulePath(scope: ScheduleScope): string {
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/schedules`;
}
