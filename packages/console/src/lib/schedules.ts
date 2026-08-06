import { request } from "./api";

export type Schedule = {
  id: string;
  task_id: string;
  cron: { pattern: string; timezone: string };
  status: "active" | "errored" | "archived";
  generation: number;
  effective_from: string;
  next_fire_at?: string;
  last_fire_at?: string;
  last_failure?: { code: string; message: string; details: Record<string, unknown> };
  created_at: string;
  updated_at: string;
};

export type ListSchedulesResponse = {
  schedules: Schedule[];
};

export type ScheduleScope = {
  projectID: string;
  environmentID: string;
};

export async function listSchedules(scope: ScheduleScope): Promise<ListSchedulesResponse> {
  return request<ListSchedulesResponse>(schedulePath(scope));
}

function schedulePath(scope: ScheduleScope): string {
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/schedules`;
}
