import { postJson, request } from "./api";

export type RunStatus =
  | "queued"
  | "claimed"
  | "running"
  | "waiting"
  | "succeeded"
  | "failed"
  | "cancelled";

export type RunFilter = RunStatus | "live" | "all";

export type PendingWait = {
  kind: "approval" | "message";
  waitpoint_id: string;
  message?: string;
  prompt?: string;
  timeout?: number;
  requested_at: string;
};

export type Run = {
  id: string;
  project_id: string;
  environment_id: string;
  task_id: string;
  status: RunStatus;
  exit_code: number | null;
  created_at: string;
  updated_at: string;
  pending_wait?: PendingWait;
};

export type ListRunsResponse = {
  runs: Run[];
};

export type RunCountsResponse = Record<RunStatus, number>;

export type LogSnapshot = {
  stdout_base64: string;
  stderr_base64: string;
  cursor: string;
  truncated: boolean;
};

export type ListRunsOptions = {
  filter?: RunFilter;
  limit?: number;
  projectID?: string;
  environmentID?: string;
};

export async function listRuns(options: RunFilter | ListRunsOptions = "live", limit = 100): Promise<ListRunsResponse> {
  const resolved = typeof options === "string" ? { filter: options, limit } : options;
  const filter = resolved.filter ?? "live";
  const rowLimit = resolved.limit ?? 100;
  const params = new URLSearchParams({ status: filter, limit: String(rowLimit) });
  if (resolved.projectID && resolved.environmentID) {
    params.set("project_id", resolved.projectID);
    params.set("environment_id", resolved.environmentID);
  }
  return request<ListRunsResponse>(`/api/runs?${params.toString()}`);
}

export async function countRuns(options: Pick<ListRunsOptions, "projectID" | "environmentID"> = {}): Promise<RunCountsResponse> {
  const params = new URLSearchParams();
  if (options.projectID && options.environmentID) {
    params.set("project_id", options.projectID);
    params.set("environment_id", options.environmentID);
  }
  const query = params.toString();
  return request<RunCountsResponse>(`/api/runs/counts${query ? `?${query}` : ""}`);
}

export async function getRun(id: string): Promise<Run> {
  return request<Run>(`/api/runs/${encodeURIComponent(id)}`);
}

export async function getRunLogs(id: string): Promise<LogSnapshot> {
  return request<LogSnapshot>(`/api/runs/${encodeURIComponent(id)}/logs`);
}

export async function approveWaitpoint(runID: string, waitpointID: string, reason = ""): Promise<void> {
  return postJson<{ reason?: string }, void>(
    `/api/runs/${encodeURIComponent(runID)}/waitpoints/${encodeURIComponent(waitpointID)}/approve`,
    reason ? { reason } : {},
  );
}

export async function denyWaitpoint(runID: string, waitpointID: string, reason = ""): Promise<void> {
  return postJson<{ reason?: string }, void>(
    `/api/runs/${encodeURIComponent(runID)}/waitpoints/${encodeURIComponent(waitpointID)}/deny`,
    reason ? { reason } : {},
  );
}

export async function replyToWaitpoint(runID: string, waitpointID: string, text: string): Promise<void> {
  return postJson<{ text: string }, void>(
    `/api/runs/${encodeURIComponent(runID)}/waitpoints/${encodeURIComponent(waitpointID)}/message`,
    { text },
  );
}
