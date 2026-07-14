import { postJson, request } from "./api";

export type RunStatus =
  | "queued"
  | "running"
  | "waiting"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "expired";

export type RunFilter = RunStatus | "live" | "all";

export type TaskOutput = unknown;

export type PendingWait = {
  id: string;
  kind?: string;
  status?: string;
  params?: unknown;
  metadata?: unknown;
  tags?: string[];
  timeout?: number;
  created_at: string;
};

export type Run = {
  id: string;
  project_id: string;
  environment_id: string;
  version: string;
  deployment_version: string;
  session_id: string;
  api_version: string;
  sdk_version?: string;
  cli_version?: string;
  attempt_number?: number | null;
  task_id: string;
  status: RunStatus;
  exit_code: number | null;
  output?: TaskOutput;
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

export type RunEventRecord = {
  id: string;
  run_id?: string | null;
  attempt_number?: number | null;
  kind: string;
  message: string;
  at: string;
  attributes: unknown;
};

export type RunEventPage = {
  events: RunEventRecord[];
  cursor: string;
  next_cursor?: string | null;
};

export type ListRunEventsOptions = {
  cursor?: string;
  limit?: number;
};

export type CompletePendingTokenResponse = {
  status: "completed" | "already_completed";
};

export type ListRunsOptions = {
  filter?: RunFilter;
  limit?: number;
  projectID: string;
  environmentID: string;
};

export async function listRuns(options: ListRunsOptions): Promise<ListRunsResponse> {
  const filter = options.filter ?? "all";
  const rowLimit = options.limit ?? 100;
  const params = new URLSearchParams({ status: filter, limit: String(rowLimit) });
  return request<ListRunsResponse>(`${environmentPath(options.projectID, options.environmentID)}/runs?${params.toString()}`);
}

export async function countRuns(options: Pick<ListRunsOptions, "projectID" | "environmentID">): Promise<RunCountsResponse> {
  return request<RunCountsResponse>(`${environmentPath(options.projectID, options.environmentID)}/runs/counts`);
}

export async function getRun(id: string, projectID: string, environmentID: string): Promise<Run> {
  return request<Run>(`${environmentPath(projectID, environmentID)}/runs/${encodeURIComponent(id)}`);
}

export async function getRunLogs(id: string, projectID: string, environmentID: string): Promise<LogSnapshot> {
  return request<LogSnapshot>(`${environmentPath(projectID, environmentID)}/runs/${encodeURIComponent(id)}/logs`);
}

export async function getRunEvents(id: string, projectID: string, environmentID: string, options: ListRunEventsOptions = {}): Promise<RunEventPage> {
  const params = new URLSearchParams();
  if (options.cursor !== undefined) params.set("cursor", String(options.cursor));
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.toString();
  return request<RunEventPage>(`${environmentPath(projectID, environmentID)}/runs/${encodeURIComponent(id)}/events${query ? `?${query}` : ""}`);
}

export async function completePendingToken(
  tokenID: string,
  data: unknown,
  projectID: string,
  environmentID: string,
): Promise<CompletePendingTokenResponse> {
  return postJson<{ data: unknown }, CompletePendingTokenResponse>(
    `${environmentPath(projectID, environmentID)}/tokens/${encodeURIComponent(tokenID)}/complete`,
    { data },
  );
}

function environmentPath(projectID: string | undefined, environmentID: string | undefined): string {
  if (!projectID || !environmentID) {
    throw new Error("project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}`;
}
