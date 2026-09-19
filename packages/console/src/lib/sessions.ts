import { postJson, request } from "./api";

export type SessionStatus = "open" | "closing" | "closed" | "failed";
export type Session = {
  id: string; actor_id: string; deployment_id: string; workspace_id?: string; key?: string;
  status: SessionStatus; created_at: string; updated_at: string;
  current_run_id: string | null; active_turn_id: string | null;
  dispatch: { state: string; hold_id?: string; reason?: string };
  failure?: { code: string; message: string; details: { run_id?: string } };
};
export type SessionTurn = {
  id: string; session_id: string; status: string; input: unknown;
  accepts_messages: boolean; interrupt_requested: boolean;
  result?: unknown; error?: unknown;
};
export type SessionEvent = {
  id: string; session_id: string; turn_id: string | null; sequence: number;
  kind: string; data: unknown; created_at: string;
  provenance: { run_id: string; attempt_number: number; run_generation: number; deployment_id: string } | null;
};
export type SessionEventPage = {
  records: SessionEvent[]; next_after: number; has_more: boolean; retained_after: number;
};
export type SessionRecordPageOptions = { after?: number | undefined; limit?: number | undefined };
export type SessionReceipt = { id: string; status?: string; kind?: string; session_id?: string; turn_id?: string | null; message_id?: string; hold_id?: string };
export type SessionAddress = { sessionID: string; projectID: string; environmentID: string };
export type ListSessionsResponse = { sessions: Session[]; next_cursor?: string };

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

export function getSessionEvents(address: SessionAddress, options: SessionRecordPageOptions = {}): Promise<SessionEventPage> {
  return request(`${sessionAPIPath(address)}/events${recordPageQuery(options)}`);
}
export function getSessionTurn(address: SessionAddress, turnID: string): Promise<SessionTurn> {
  return request(`${sessionAPIPath(address)}/turns/${encodeURIComponent(turnID)}`);
}
export function sendSession(address: SessionAddress, input: { data: unknown; idempotency_key: string }, mode: "send" | "enqueue"): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/${mode}`, input);
}
export function sendTurnMessage(address: SessionAddress, turnID: string, input: { data: unknown; idempotency_key: string }): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/turns/${encodeURIComponent(turnID)}/messages`, input);
}
export function interruptTurn(address: SessionAddress, turnID: string, input: { idempotency_key: string }): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/turns/${encodeURIComponent(turnID)}/interrupt`, input);
}
export function resumeSession(address: SessionAddress, input: { hold_id: string; idempotency_key: string }): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/resume`, input);
}
export function closeSession(address: SessionAddress, input: { idempotency_key: string }): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/close`, input);
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
