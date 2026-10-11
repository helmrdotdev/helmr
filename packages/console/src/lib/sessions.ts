import type { InputContent } from "../../../../sdk/typescript/src/content";
import { parseAsk, parseAskPage } from "../../../../sdk/typescript/src/internal/asks";
import type { JsonValue } from "../../../../sdk/typescript/src/contract";
import { postJson, request } from "./api";

export type SessionStatus = "open" | "closing" | "closed" | "cancelled";
export type Session = {
  slack_channel_id?: string | null;
  id: string;
  agent_id: string;
  deployment_id: string;
  computer_id: string;
  root_session_id: string;
  parent_session_id: string | null;
  requester_session_id: string | null;
  initial_turn: { id: string; status: SessionTurn["status"] } | null;
  key?: string;
  status: SessionStatus;
  created_at: string;
  holds: {
    id: string;
    session_id: string;
    scope: "local" | "subtree";
    reason: string;
    created_at: string;
  }[];
};
export type SessionTurn = {
  id: string;
  session_id: string;
  sequence: number;
  status:
    | "queued"
    | "running"
    | "finalizing"
    | "completed"
    | "failed"
    | "interrupted"
    | "cancelled";
  input?: InputContent;
  payload_expired_at?: string;
  result?: unknown;
  error?: { code: string; message?: string };
  response?: import("../../../../sdk/typescript/src/content").Content;
  started_at?: string;
  terminal_at?: string;
  completion_save_id?: string;
};
export type SessionTurnPage = { turns: SessionTurn[]; next_cursor?: string };
export type SessionEvent = {
  session_id: string;
  turn_id: string | null;
  sequence: number;
  kind: string;
  data: JsonValue;
  created_at: string;
};
export type SessionEventPage = {
  records: SessionEvent[];
  next_after: number;
  has_more: boolean;
  retained_after: number;
};
export type SessionEventPageOptions = {
  after?: number | undefined;
  limit?: number | undefined;
};
export type SessionReceipt = {
  id?: string;
  sequence?: number;
  status?: string;
  kind?: string;
  session_id?: string;
  turn_id?: string | null;
  message_id?: string;
  hold_id?: string;
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

export function getSessionEvents(
  address: SessionAddress,
  options: SessionEventPageOptions = {},
): Promise<SessionEventPage> {
  return request(`${sessionAPIPath(address)}/events${eventPageQuery(options)}`);
}
export function getSessionTurn(
  address: SessionAddress,
  turnID: string,
): Promise<SessionTurn> {
  return request(
    `${sessionAPIPath(address)}/turns/${encodeURIComponent(turnID)}`,
  );
}

export function interruptSession(
  address: SessionAddress,
  input: { idempotency_key: string },
): Promise<SessionReceipt> {
  return postJson(
    `${sessionAPIPath(address)}/interrupt`,
    input,
  );
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

function eventPageQuery(options: SessionEventPageOptions): string {
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

export function listSessionTurns(
  address: SessionAddress,
  options: { cursor?: string | undefined; limit?: number | undefined } = {},
): Promise<SessionTurnPage> {
  const params = new URLSearchParams();
  if (options.cursor) params.set("cursor", options.cursor);
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  return request(
    `${sessionAPIPath(address)}/turns${params.size ? `?${params}` : ""}`,
  );
}
export function cancelSession(
  address: SessionAddress,
  input: { idempotency_key: string },
): Promise<SessionReceipt> {
  return postJson(`${sessionAPIPath(address)}/cancel`, input);
}

export type AskAddress = SessionAddress & { turnID: string; askID: string };
// Quote every argument, including route-provided IDs, for a POSIX shell.
export function questionCLICommands(address: AskAddress, origin: string) {
  const quote = (value: string) => "'" + value.replaceAll("'", "'\"'\"'") + "'";
  const prefix = `helmr --api-url ${quote(origin)} session turn ask`;
  const target = [address.sessionID, address.turnID, address.askID].map(quote).join(" ");
  const scope = `--project ${quote(address.projectID)} --env ${quote(address.environmentID)}`;
  return {
    login: `helmr login ${quote(origin)}`,
    get: `${prefix} get ${target} ${scope} --json`,
    respond: `${prefix} respond ${target} ${scope} --answer-file answer.json --response-id RESPONSE_ID`,
  };
}

export type { AskState, AskPage } from "../../../../sdk/typescript/src/contract";
export function askConsolePath(address: AskAddress): string {
  const query = new URLSearchParams({ project_id: address.projectID, environment_id: address.environmentID });
  return `/sessions/${encodeURIComponent(address.sessionID)}/turns/${encodeURIComponent(address.turnID)}/asks/${encodeURIComponent(address.askID)}?${query}`;
}

export async function getAsk(address: AskAddress) {
  return parseAsk(await request(`${askAPIPath(address)}/${encodeURIComponent(address.askID)}`));
}
export async function listAsks(address: SessionAddress & { turnID: string }, cursor?: string) {
  return parseAskPage(await request(`${askAPIPath(address)}?limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`));
}

function askAPIPath(address: SessionAddress & { turnID: string }) {
  return `${sessionAPIPath(address)}/turns/${encodeURIComponent(address.turnID)}/asks`;
}

export function messageDeliveryNotice(event: SessionEvent): string | undefined {
 if (event.kind !== "message.rejected" || event.data === null || typeof event.data !== "object" || Array.isArray(event.data)) return undefined;
 return "delivery" in event.data && event.data["delivery"] === "uncertain"
  ? "Message delivery uncertain. Check the conversation before sending it again."
  : "Message not delivered. Send a new message when the Agent is available.";
}
