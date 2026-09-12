import { postJson, request } from "./api";

export type SessionStatus = "open" | "closed" | "cancelled" | "failed";

export type Session = {
  id: string;
  actor_id: string;
  deployment_id: string;
  workspace_id?: string;
  key?: string;
  status: SessionStatus;
  created_at: string;
  updated_at: string;
  current_run_id?: string;
  failure?: {
    code: string;
    message: string;
    details: { run_id?: string };
  };
};

export type SessionInputRecord = {
  id: string;
  sequence: number;
  data: unknown;
  source: {
    type: string;
    run_id?: string;
  };
  created_at: string;
};

export type SessionInputPage = {
  records: SessionInputRecord[];
  next_after: number;
  has_more: boolean;
};

export type SessionOutputRecord = {
  id: string;
  sequence: number;
  data: unknown;
  content_type: string;
  created_at: string;
  provenance: {
    run_id: string;
    attempt_number: number;
    deployment_id: string;
  };
};

export type SessionOutputPage = {
  records: SessionOutputRecord[];
  next_after: number;
  has_more: boolean;
};

export type SessionRecordPageOptions = { after?: number | undefined; limit?: number | undefined };

export type SessionCloseReceipt = {
  session_id: string;
  accepted_at: string;
};

export type SessionAddress = {
  sessionID: string;
  projectID: string;
  environmentID: string;
};

export type ListSessionsResponse = {
  sessions: Session[];
  next_cursor?: string;
};

export type ConversationEntry =
  | { direction: "input"; record: SessionInputRecord }
  | { direction: "output"; record: SessionOutputRecord };

export async function listSessions(options: {
  projectID: string;
  environmentID: string;
  statuses?: SessionStatus[] | undefined;
  cursor?: string | undefined;
  limit?: number | undefined;
}): Promise<ListSessionsResponse> {
  if (!options.projectID || !options.environmentID) {
    throw new Error("Session project and environment are required");
  }
  const params = new URLSearchParams();
  for (const status of options.statuses ?? []) params.append("status", status);
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  const query = params.size === 0 ? "" : `?${params.toString()}`;
  return request<ListSessionsResponse>(
    `/api/projects/${encodeURIComponent(options.projectID)}/environments/${encodeURIComponent(options.environmentID)}/sessions${query}`,
  );
}

export async function getSession(address: SessionAddress): Promise<Session> {
  return request<Session>(sessionAPIPath(address));
}

export async function getSessionInput(
  address: SessionAddress,
  options: SessionRecordPageOptions = {},
): Promise<SessionInputPage> {
  return request<SessionInputPage>(`${sessionAPIPath(address)}/inputs${recordPageQuery(options)}`);
}

export async function getSessionOutput(
  address: SessionAddress,
  options: SessionRecordPageOptions = {},
): Promise<SessionOutputPage> {
  return request<SessionOutputPage>(`${sessionAPIPath(address)}/outputs${recordPageQuery(options)}`);
}

export async function sendSessionInput(
  address: SessionAddress,
  input: { input: unknown; idempotency_key: string },
): Promise<SessionInputRecord> {
  return postJson<{ input: unknown; idempotency_key: string }, SessionInputRecord>(
    `${sessionAPIPath(address)}/inputs`,
    input,
  );
}

export async function closeSession(
  address: SessionAddress,
  input: { idempotency_key: string },
): Promise<SessionCloseReceipt> {
  return postJson<{ idempotency_key: string }, SessionCloseReceipt>(
    `${sessionAPIPath(address)}/close`,
    input,
  );
}

// Inputs and outputs live in separate sequence spaces, so the conversation
// view orders them by creation time; an input that shares an instant with an
// output is shown first because the Actor reacts to it.
export function interleaveSessionRecords(
  inputs: readonly SessionInputRecord[],
  outputs: readonly SessionOutputRecord[],
): ConversationEntry[] {
  const entries: ConversationEntry[] = [
    ...inputs.map((record): ConversationEntry => ({ direction: "input", record })),
    ...outputs.map((record): ConversationEntry => ({ direction: "output", record })),
  ];
  return entries.sort((a, b) => {
    const byTime = Date.parse(a.record.created_at) - Date.parse(b.record.created_at);
    if (byTime !== 0) return byTime;
    if (a.direction !== b.direction) return a.direction === "input" ? -1 : 1;
    return a.record.sequence - b.record.sequence;
  });
}

export function sessionConsolePath(
  sessionID: string,
  projectID: string,
  environmentID: string,
): string {
  const params = new URLSearchParams({
    project_id: projectID,
    environment_id: environmentID,
  });
  return `/sessions/${encodeURIComponent(sessionID)}?${params.toString()}`;
}

export function runSessionConsolePath(
  run: { session_id?: string },
  projectID: string,
  environmentID: string,
): string | undefined {
  if (!run.session_id) return undefined;
  return sessionConsolePath(run.session_id, projectID, environmentID);
}

function recordPageQuery(options: SessionRecordPageOptions): string {
  const params = new URLSearchParams();
  if (options.after !== undefined) params.set("after", String(options.after));
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  return params.size === 0 ? "" : `?${params.toString()}`;
}

function sessionAPIPath(address: SessionAddress): string {
  if (!address.projectID || !address.environmentID) {
    throw new Error("Session project and environment are required");
  }
  return `/api/projects/${encodeURIComponent(address.projectID)}/environments/${encodeURIComponent(address.environmentID)}/sessions/${encodeURIComponent(address.sessionID)}`;
}
