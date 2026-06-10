import { del, postJson, request } from "./api";

export type Schedule = {
  id: string;
  type: "imperative" | "declarative";
  project_id: string;
  environment_id: string;
  task: string;
  deduplication_key?: string;
  external_id?: string;
  cron: string;
  timezone: string;
  active: boolean;
  status: "active" | "inactive" | "errored";
  last_error?: string;
  next_fire_at?: string;
  last_fire_at?: string;
  created_at: string;
  updated_at: string;
};

export type ListSchedulesResponse = {
  schedules: Schedule[];
};

export type CreateScheduleInput = {
  project_id: string;
  environment_id: string;
  deduplication_key: string;
  external_id?: string;
  task: string;
  cron: string;
  timezone?: string;
  active?: boolean;
};

export type UpdateScheduleInput = Omit<CreateScheduleInput, "deduplication_key">;

export type ScheduleScope = {
  projectID: string;
  environmentID: string;
};

export async function listSchedules(scope: ScheduleScope): Promise<ListSchedulesResponse> {
  return request<ListSchedulesResponse>(`${schedulePath(scope)}`);
}

export async function createSchedule(input: CreateScheduleInput): Promise<Schedule> {
  return postJson<Omit<CreateScheduleInput, "project_id" | "environment_id">, Schedule>(
    schedulePath({ projectID: input.project_id, environmentID: input.environment_id }),
    scheduleRequest(input),
  );
}

export async function updateSchedule(id: string, input: UpdateScheduleInput): Promise<Schedule> {
  return request<Schedule>(
    `${schedulePath({ projectID: input.project_id, environmentID: input.environment_id })}/${encodeURIComponent(id)}`,
    { method: "PUT", body: JSON.stringify(scheduleRequest(input)) },
  );
}

export async function activateSchedule(id: string, scope: ScheduleScope): Promise<Schedule> {
  return postJson<Record<string, never>, Schedule>(
    `${schedulePath(scope)}/${encodeURIComponent(id)}/activate`,
    {},
  );
}

export async function deactivateSchedule(id: string, scope: ScheduleScope): Promise<Schedule> {
  return postJson<Record<string, never>, Schedule>(
    `${schedulePath(scope)}/${encodeURIComponent(id)}/deactivate`,
    {},
  );
}

export async function deleteSchedule(id: string, scope: ScheduleScope): Promise<void> {
  return del<void>(`${schedulePath(scope)}/${encodeURIComponent(id)}`);
}

function schedulePath(scope: ScheduleScope): string {
  return `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/schedules`;
}

function scheduleRequest<T extends CreateScheduleInput | UpdateScheduleInput>(input: T): Omit<T, "project_id" | "environment_id"> {
  const request = { ...input } as Partial<T> & { project_id?: string; environment_id?: string };
  delete request.project_id;
  delete request.environment_id;
  return request as Omit<T, "project_id" | "environment_id">;
}
