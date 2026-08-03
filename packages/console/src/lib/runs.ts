import { request } from "./api";

export type RunStatus =
  | "queued"
  | "running"
  | "waiting"
  | "retry-delayed"
  | "cancel-requested"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "expired"
  | "system-failed";

export type RunFilter = RunStatus | "live" | "all";
export type TaskOutput = unknown;

export type Run = {
  id: string;
  status: RunStatus;
  entrypoint: {
    kind: string;
    id: string;
  };
  deployment: {
    id: string;
    version: string;
  };
  workspace_id: string;
  actor_id?: string;
  parent_run_id?: string;
  parent_owns_lifecycle?: boolean;
  current_attempt_number: number;
  cause: {
    type: string;
    parent_run_id?: string;
    schedule_id?: string;
    scheduled_at?: string;
    last_scheduled_at?: string;
    timezone?: string;
  };
  metadata: unknown;
  tags: string[];
  output?: unknown;
  terminal_reason_code?: string;
  error?: {
    code: string;
    message: string;
    retryable: boolean;
    details?: unknown;
  };
  created_at: string;
  started_at?: string;
  terminal_at?: string;
};

export type ListRunsResponse = {
  runs: Run[];
  next_cursor?: string;
};

export type RunLogRecord = {
  id: string;
  kind: "stdout" | "stderr" | "structured";
  run_id: string;
  attempt_number: number;
  level?: string;
  message?: string;
  attributes?: unknown;
  observed_sequence?: number;
  content_base64?: string;
  bytes?: number;
  at: string;
};

export type RunLogPage = {
  logs: RunLogRecord[];
  next_cursor?: string;
};

export type RunEventRecord = {
  id: string;
  run_id?: string | null;
  attempt_number?: number | null;
  category: string;
  severity: string;
  source: string;
  kind: string;
  message: string;
  at: string;
  occurred_at: string;
  redaction_class: string;
  attributes: unknown;
};

export type RunEventPage = {
  events: RunEventRecord[];
  next_cursor?: string | null;
};

export type ListRunTelemetryOptions = {
  cursor?: string;
  limit?: number;
};

export type ListRunsOptions = {
  filter?: RunFilter;
  statuses?: RunStatus[];
  cursor?: string;
  limit?: number;
  projectID: string;
  environmentID: string;
};

const LIVE_STATUSES: RunStatus[] = [
  "queued",
  "running",
  "waiting",
  "retry-delayed",
  "cancel-requested",
];

export async function listRuns(options: ListRunsOptions): Promise<ListRunsResponse> {
  const params = new URLSearchParams();
  const statuses =
    options.statuses ??
    (options.filter === "live"
      ? LIVE_STATUSES
      : options.filter && options.filter !== "all"
        ? [options.filter]
        : []);
  for (const status of statuses) params.append("status", status);
  if (options.cursor) params.set("cursor", options.cursor);
  params.set("limit", String(options.limit ?? 100));
  return request<ListRunsResponse>(
    `${environmentPath(options.projectID, options.environmentID)}/runs?${params.toString()}`,
  );
}

export async function getRun(id: string, projectID: string, environmentID: string): Promise<Run> {
  return request<Run>(`${environmentPath(projectID, environmentID)}/runs/${encodeURIComponent(id)}`);
}

export async function getRunLogs(
  id: string,
  projectID: string,
  environmentID: string,
  options: ListRunTelemetryOptions = {},
): Promise<RunLogPage> {
  const params = telemetryParams(options);
  return request<RunLogPage>(
    `${environmentPath(projectID, environmentID)}/runs/${encodeURIComponent(id)}/logs${params}`,
  );
}

export async function getRunEvents(
  id: string,
  projectID: string,
  environmentID: string,
  options: ListRunTelemetryOptions = {},
): Promise<RunEventPage> {
  const params = telemetryParams(options);
  return request<RunEventPage>(
    `${environmentPath(projectID, environmentID)}/runs/${encodeURIComponent(id)}/events${params}`,
  );
}

function telemetryParams(options: ListRunTelemetryOptions): string {
  const params = new URLSearchParams();
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.toString();
  return query ? `?${query}` : "";
}

function environmentPath(projectID: string | undefined, environmentID: string | undefined): string {
  if (!projectID || !environmentID) {
    throw new Error("project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(projectID)}/environments/${encodeURIComponent(environmentID)}`;
}
