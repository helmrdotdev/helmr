import { postJson, request } from "./api";
import type { SessionActivity, SessionStatus } from "../features/sessions/display";

export type Session = {
  id: string;
  project_id: string;
  environment_id: string;
  task_id: string;
  initial_deployment_id: string;
  active_deployment_id: string;
  external_id?: string;
  status: SessionStatus;
  activity: SessionActivity;
  can_close: boolean;
  current_run_id?: string;
  workspace_id?: string;
  metadata?: unknown;
  tags?: string[];
  result?: unknown;
  error?: unknown;
  timed_out?: boolean;
  terminal_reason?: unknown;
  expires_at?: string;
  expired_at?: string;
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

function sessionPath(projectID: string | undefined, environmentID: string | undefined): string {
  if (!projectID || !environmentID) {
    throw new Error("project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}/sessions`;
}
