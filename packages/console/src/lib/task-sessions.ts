import { postJson, request } from "./api";
import type { TaskSessionStatus } from "../features/sessions/display";

export type TaskSession = {
  id: string;
  project_id: string;
  environment_id: string;
  task_id: string;
  initial_deployment_id: string;
  active_deployment_id: string;
  external_id?: string;
  status: TaskSessionStatus;
  current_run_id?: string;
  workspace_id?: string;
  metadata?: unknown;
  tags?: string[];
  result?: unknown;
  error?: unknown;
  timed_out?: boolean;
  terminal_reason?: unknown;
  expires_at?: string;
  created_at: string;
  updated_at: string;
};

export type ListTaskSessionsResponse = {
  sessions: TaskSession[];
};

export type TaskSessionRun = {
  id: string;
  run_id: string;
  deployment_id: string;
  previous_run_id?: string;
  turn_index: number;
  status: string;
  execution_status: string;
  terminal_outcome?: string;
  created_at: string;
  ended_at?: string;
};

export type ListTaskSessionRunsResponse = {
  runs: TaskSessionRun[];
};

export type TaskSessionStream = {
  id: string;
  task_session_id: string;
  name: string;
  direction: "input" | "output" | string;
  backend?: string;
  next_sequence: number;
  created_at: string;
};

export type ListTaskSessionStreamsResponse = {
  streams: TaskSessionStream[];
};

export type StreamRecord = {
  id: string;
  stream_id: string;
  sequence: number;
  data: unknown;
  correlation_id?: string;
  content_type?: string;
  created_at: string;
};

export type ListStreamRecordsResponse = {
  records: StreamRecord[];
};

export type ListTaskSessionsOptions = {
  projectID: string;
  environmentID: string;
  status?: TaskSessionStatus | "all";
  taskID?: string;
  limit?: number;
};

export type TaskSessionScope = {
  projectID: string;
  environmentID: string;
};

export async function listTaskSessions(options: ListTaskSessionsOptions): Promise<ListTaskSessionsResponse> {
  const params = new URLSearchParams();
  if (options.status && options.status !== "all") params.set("status", options.status);
  if (options.taskID) params.set("task_id", options.taskID);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.toString();
  return request<ListTaskSessionsResponse>(`${sessionPath(options.projectID, options.environmentID)}${query ? `?${query}` : ""}`);
}

export async function getTaskSession(id: string, scope: TaskSessionScope): Promise<TaskSession> {
  return request<TaskSession>(`${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}`);
}

export async function closeTaskSession(id: string, scope: TaskSessionScope, reason = "closed from console"): Promise<TaskSession> {
  return postJson<{ reason: string }, TaskSession>(
    `${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/close`,
    { reason },
  );
}

export async function cancelTaskSession(id: string, scope: TaskSessionScope, reason = "cancelled from console"): Promise<TaskSession> {
  return postJson<{ reason: string }, TaskSession>(
    `${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/cancel`,
    { reason },
  );
}

export async function listTaskSessionRuns(id: string, scope: TaskSessionScope): Promise<ListTaskSessionRunsResponse> {
  return request<ListTaskSessionRunsResponse>(`${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/runs`);
}

export async function listTaskSessionStreams(id: string, scope: TaskSessionScope): Promise<ListTaskSessionStreamsResponse> {
  return request<ListTaskSessionStreamsResponse>(`${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/streams`);
}

export async function listTaskSessionStreamRecords(
  id: string,
  scope: TaskSessionScope,
  stream: TaskSessionStream,
  options: { limit?: number } = {},
): Promise<ListStreamRecordsResponse> {
  const params = new URLSearchParams();
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.toString();
  const directionPath = stream.direction === "input" ? "inputs" : "outputs";
  return request<ListStreamRecordsResponse>(
    `${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/${directionPath}/${encodeURIComponent(stream.name)}${query ? `?${query}` : ""}`,
  );
}

function sessionPath(projectID: string | undefined, environmentID: string | undefined): string {
  if (!projectID || !environmentID) {
    throw new Error("project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}/sessions`;
}
