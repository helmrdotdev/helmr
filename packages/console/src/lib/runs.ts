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

type PendingWaitpointBase = {
  waitpoint_id: string;
  policy?: string | null;
  deliveries?: WaitpointDelivery[];
  request?: unknown;
  display_text?: string;
  timeout?: number;
  requested_at: string;
};

export type PendingWaitpoint =
  PendingWaitpointBase & {
    kind: "human" | "delay";
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
  version: string;
  deployment_version: string;
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
  pending_waitpoint?: PendingWaitpoint;
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
  session_id?: string | null;
  attempt_number?: number | null;
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

export async function listRunEvents(id: string, projectID: string, environmentID: string, limit = 200): Promise<RunEventPage> {
  const pages: RunEventPage[] = [];
  let cursor = 0;
  for (;;) {
    const page = await getRunEvents(id, projectID, environmentID, cursor === 0 ? { limit } : { cursor, limit });
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

export async function respondWaitpoint(waitpointID: string, projectID: string, environmentID: string, value?: unknown): Promise<void> {
  return postJson<{ value?: unknown }, void>(
    `${environmentPath(projectID, environmentID)}/waitpoints/${encodeURIComponent(waitpointID)}/respond`,
    value === undefined ? {} : { value },
  );
}

export async function createWaitpointResponseToken(waitpointID: string, kind: PendingWaitpoint["kind"], projectID: string, environmentID: string): Promise<WaitpointResponseToken> {
  if (kind === "delay") {
    throw new Error("Delay waitpoints do not support confirmation links.");
  }
  const response = await postJson<
    { waitpoint_id: string },
    { id: string; token: string; expires_at: string | null }
  >(
    `${environmentPath(projectID, environmentID)}/waitpoints/tokens`,
    {
      waitpoint_id: waitpointID,
    },
  );
  const url = new URL("/waitpoints/respond", window.location.origin);
  url.searchParams.set("id", response.id);
  url.searchParams.set("token", response.token);
  return { ...response, url: url.toString() };
}

function environmentPath(projectID: string | undefined, environmentID: string | undefined): string {
  if (!projectID || !environmentID) {
    throw new Error("project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}`;
}
