import { postJson, request } from "./api";

export type RunStatus =
  | "queued"
  | "running"
  | "waiting"
  | "succeeded"
  | "failed"
  | "cancelled";

export type RunFilter = RunStatus | "live" | "all";

export type PendingWait = {
  kind: "approval" | "message";
  waitpoint_id: string;
  policy?: string | null;
  deliveries?: WaitpointDelivery[];
  message?: string;
  prompt?: string;
  timeout?: number;
  requested_at: string;
};

export type WaitpointDelivery = {
  id: string;
  channel: "email" | string;
  recipient_kind: "email" | string;
  recipient: string;
  status: "queued" | "sent" | "failed" | "cancelled" | "expired" | string;
  last_error?: string | null;
  sent_at?: string | null;
  created_at: string;
  updated_at: string;
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

export type RunEventRecord = {
  id: string;
  run_id?: string | null;
  kind: string;
  message: string;
  at: string;
  attributes: unknown;
};

export type RunEventPage = {
  events: RunEventRecord[];
  cursor: number;
  next_cursor?: number | null;
};

export type ListRunEventsOptions = {
  cursor?: number;
  limit?: number;
};

export type WaitpointResponseToken = {
  id: string;
  token: string;
  url: string;
  expires_at: string | null;
};

export type ListRunsOptions = {
  filter?: RunFilter;
  limit?: number;
  projectID?: string;
  environmentID?: string;
};

export async function listRuns(options: RunFilter | ListRunsOptions = "all", limit = 100): Promise<ListRunsResponse> {
  const resolved = typeof options === "string" ? { filter: options, limit } : options;
  const filter = resolved.filter ?? "all";
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

export async function getRunEvents(id: string, options: ListRunEventsOptions = {}): Promise<RunEventPage> {
  const params = new URLSearchParams();
  if (options.cursor !== undefined) params.set("cursor", String(options.cursor));
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.toString();
  return request<RunEventPage>(`/api/runs/${encodeURIComponent(id)}/events${query ? `?${query}` : ""}`);
}

export async function listRunEvents(id: string, limit = 200): Promise<RunEventPage> {
  const pages: RunEventPage[] = [];
  let cursor = 0;
  for (;;) {
    const page = await getRunEvents(id, cursor === 0 ? { limit } : { cursor, limit });
    pages.push(page);
    if (page.next_cursor == null) break;
    if (page.next_cursor <= cursor) break;
    cursor = page.next_cursor;
  }
  return {
    cursor: pages[0]?.cursor ?? 0,
    events: pages.flatMap((page) => page.events),
    next_cursor: null,
  };
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

export async function createWaitpointResponseToken(runID: string, waitpointID: string, kind: PendingWait["kind"]): Promise<WaitpointResponseToken> {
  const response = await postJson<
    { run_id: string; waitpoint_id: string; actions: string[] },
    { id: string; token: string; expires_at: string | null }
  >(
    "/api/waitpoints/tokens",
    {
      run_id: runID,
      waitpoint_id: waitpointID,
      actions: kind === "approval" ? ["approve", "deny"] : ["message"],
    },
  );
  const url = new URL("/waitpoints/respond", window.location.origin);
  url.searchParams.set("id", response.id);
  url.searchParams.set("token", response.token);
  return { ...response, url: url.toString() };
}
