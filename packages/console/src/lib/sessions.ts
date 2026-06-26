import { postJson, request } from "./api";
import type { SessionStatus } from "../features/sessions/display";

export type Session = {
  id: string;
  project_id: string;
  environment_id: string;
  task_id: string;
  initial_deployment_id: string;
  active_deployment_id: string;
  external_id?: string;
  status: SessionStatus;
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

export type ListSessionsResponse = {
  sessions: Session[];
};

export type SessionRun = {
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

export type ListSessionRunsResponse = {
  runs: SessionRun[];
};

export type SessionStream = {
  id: string;
  session_id: string;
  name: string;
  direction: "input" | "output" | string;
  backend?: string;
  next_sequence: number;
  created_at: string;
};

export type ListSessionStreamsResponse = {
  streams: SessionStream[];
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

export type ListSessionsOptions = {
  projectID: string;
  environmentID: string;
  status?: SessionStatus | "all";
  taskID?: string;
  limit?: number;
};

export type SessionScope = {
  projectID: string;
  environmentID: string;
};

export async function listSessions(options: ListSessionsOptions): Promise<ListSessionsResponse> {
  const params = new URLSearchParams();
  if (options.status && options.status !== "all") params.set("status", options.status);
  if (options.taskID) params.set("task_id", options.taskID);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.toString();
  return request<ListSessionsResponse>(`${sessionPath(options.projectID, options.environmentID)}${query ? `?${query}` : ""}`);
}

export async function getSession(id: string, scope: SessionScope): Promise<Session> {
  return request<Session>(`${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}`);
}

export async function closeSession(id: string, scope: SessionScope, reason = "closed from console"): Promise<Session> {
  return postJson<{ reason: string }, Session>(
    `${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/close`,
    { reason },
  );
}

export async function cancelSession(id: string, scope: SessionScope, reason = "cancelled from console"): Promise<Session> {
  return postJson<{ reason: string }, Session>(
    `${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/cancel`,
    { reason },
  );
}

export async function listSessionRuns(id: string, scope: SessionScope): Promise<ListSessionRunsResponse> {
  return request<ListSessionRunsResponse>(`${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/runs`);
}

export async function listSessionStreams(id: string, scope: SessionScope): Promise<ListSessionStreamsResponse> {
  return request<ListSessionStreamsResponse>(`${sessionPath(scope.projectID, scope.environmentID)}/${encodeURIComponent(id)}/streams`);
}

export async function listSessionStreamRecords(
  id: string,
  scope: SessionScope,
  stream: SessionStream,
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
