import { del, postJson, request } from "./api";

export type ScheduleWorkspace = {
  repository?: string;
  ref?: string;
  sha?: string;
  subpath?: string;
};

export type CreateScheduleWorkspace = {
  repository: string;
  ref: string;
  subpath?: string;
};

export type Schedule = {
  id: string;
  type: "imperative" | "declarative";
  project_id: string;
  environment_id: string;
  task_id: string;
  dedup_key: string;
  cron: string;
  timezone: string;
  active: boolean;
  status: "active" | "inactive" | "errored";
  last_error?: string;
  payload?: unknown;
  workspace?: ScheduleWorkspace;
  next_scheduled_at?: string;
  next_due_at?: string;
  last_scheduled_at?: string;
  created_at: string;
  updated_at: string;
};

export type ListSchedulesResponse = {
  schedules: Schedule[];
};

export type CreateScheduleInput = {
  project_id: string;
  environment_id: string;
  dedup_key?: string;
  task_id: string;
  cron: string;
  timezone?: string;
  payload?: unknown;
  workspace: CreateScheduleWorkspace;
  active?: boolean;
};

export type ScheduleScope = {
  projectID: string;
  environmentID: string;
};

function scopeQuery(scope: ScheduleScope): string {
  return new URLSearchParams({
    project_id: scope.projectID,
    environment_id: scope.environmentID,
  }).toString();
}

export async function listSchedules(scope: ScheduleScope): Promise<ListSchedulesResponse> {
  return request<ListSchedulesResponse>(`/api/schedules?${scopeQuery(scope)}`);
}

export async function createSchedule(input: CreateScheduleInput): Promise<Schedule> {
  return postJson<CreateScheduleInput, Schedule>("/api/schedules", input);
}

export async function updateSchedule(id: string, input: CreateScheduleInput): Promise<Schedule> {
  return request<Schedule>(
    `/api/schedules/${encodeURIComponent(id)}?${scopeQuery({
      projectID: input.project_id,
      environmentID: input.environment_id,
    })}`,
    { method: "PUT", body: JSON.stringify(input) },
  );
}

export async function activateSchedule(id: string, scope: ScheduleScope): Promise<Schedule> {
  return postJson<Record<string, never>, Schedule>(
    `/api/schedules/${encodeURIComponent(id)}/activate?${scopeQuery(scope)}`,
    {},
  );
}

export async function deactivateSchedule(id: string, scope: ScheduleScope): Promise<Schedule> {
  return postJson<Record<string, never>, Schedule>(
    `/api/schedules/${encodeURIComponent(id)}/deactivate?${scopeQuery(scope)}`,
    {},
  );
}

export async function deleteSchedule(id: string, scope: ScheduleScope): Promise<void> {
  return del<void>(`/api/schedules/${encodeURIComponent(id)}?${scopeQuery(scope)}`);
}
